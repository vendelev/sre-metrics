package memory_test

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/vendelev/sre-metrics/user-login/internal/application"
	"github.com/vendelev/sre-metrics/user-login/internal/domain"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/memory"
)

// together создаёт барьер старта и ограничивает ожидание; выбранного победителя нет.
func together(t *testing.T, operations ...func() error) []error {
	t.Helper()
	type result struct {
		index int
		err   error
	}
	ready := make(chan struct{}, len(operations))
	start := make(chan struct{})
	started := false
	results := make(chan result, len(operations))
	for i, operation := range operations {
		go func() {
			ready <- struct{}{}
			<-start
			results <- result{index: i, err: operation()}
		}()
	}
	defer func() {
		if !started {
			close(start)
		}
	}()
	for range operations {
		await(t, ready)
	}
	close(start)
	started = true
	errors := make([]error, len(operations))
	for range operations {
		select {
		case result := <-results:
			errors[result.index] = result.err
		case <-time.After(5 * time.Second):
			t.Fatal("конкурентные операции не завершились")
		}
	}
	return errors
}

func await(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("не получен сигнал барьера")
	}
}

func processOperation(f *fixture, e domain.Event) func() error {
	return func() error {
		_, err := f.observer.Process(e)
		return err
	}
}

func assertErrors(t *testing.T, got []error, counts map[error]int) {
	t.Helper()
	for expected, wantCount := range counts {
		var count int
		for _, err := range got {
			if errors.Is(err, expected) {
				count++
			}
		}
		if count != wantCount {
			t.Fatalf("категория %v: %d, ожидалось %d; результаты %v", expected, count, wantCount, got)
		}
	}
}

func TestProcessConcurrentDuplicates(t *testing.T) {
	f := newFixture(t, memory.DefaultLimits())
	process(t, f, event(1, 100, domain.EventTypeLoginStarted, f.base), nil)
	process(t, f, event(2, 100, domain.EventTypeLoginModeDetermined, f.base), nil)
	e := event(3, 100, domain.EventTypeLoginSucceeded, f.base)
	operations := make([]func() error, 12)
	for i := range operations {
		operations[i] = processOperation(f, e)
	}
	assertErrors(t, together(t, operations...), map[error]int{nil: 12})
	a := snapshot(t, f)
	if a.Completed[domain.OutcomeSuccess] != 1 || a.Durations[domain.LoginModeCredentials].Count != 1 {
		t.Fatalf("повторы повторно изменили агрегаты: %+v", a)
	}
}

func TestProcessConcurrentOutcomes(t *testing.T) {
	f := newFixture(t, memory.DefaultLimits())
	got := together(t,
		processOperation(f, event(1, 100, domain.EventTypeLoginSucceeded, f.base)),
		processOperation(f, event(2, 100, domain.EventTypeLoginFailed, f.base)),
		processOperation(f, event(3, 100, domain.EventTypeLoginRejected, f.base)),
	)
	assertErrors(t, got, map[error]int{nil: 1, domain.ErrConflict: 2})
	a := snapshot(t, f)
	winner := scenario(t, f, 100).Outcome()
	var completions int64
	for _, count := range a.Completed {
		completions += count
	}
	if completions != 1 || a.Completed[winner] != 1 {
		t.Fatalf("состояние не соответствует единственному победителю: %+v", a)
	}
	for i, err := range got {
		if hasEvent(t, f, i+1) != (err == nil) {
			t.Fatal("индекс содержит проигравшее событие или потерял победившее")
		}
	}
}

func TestProcessConcurrentMaintenance(t *testing.T) {
	for _, operation := range []string{"закрытие", "закрытие при полном лимите", "очистка"} {
		t.Run(operation, func(t *testing.T) {
			limit := 3
			if operation == "закрытие при полном лимите" {
				limit = 2
			}
			f := newFixture(t, memory.Limits{MaxScenarios: 1, MaxEventsPerScenario: limit})
			process(t, f, event(1, 100, domain.EventTypeLoginStarted, f.base), nil)
			process(t, f, event(2, 100, domain.EventTypeLoginModeDetermined, f.base), nil)
			f.clock.Set(f.base.Add(15 * time.Minute))
			e := event(3, 100, domain.EventTypeLoginSucceeded, f.clock.Now())
			want := domain.ErrScenarioExpired
			if operation == "закрытие при полном лимите" {
				e = event(3, 100, domain.EventTypeLoginModeDetermined, f.clock.Now())
				want = domain.ErrCapacityExceeded
			}
			if operation == "очистка" {
				maintain(t, f)
				f.clock.Set(f.base.Add(30 * time.Minute))
				e = event(1, 101, domain.EventTypeLoginSucceeded, f.clock.Now())
				want = nil
			}
			got := together(t, processOperation(f, e), f.observer.Maintain)
			if !errors.Is(got[0], want) || got[1] != nil {
				t.Fatalf("обработка против обслуживания: %v", got)
			}
			if snapshot(t, f).Completed[domain.OutcomeUnknown] != 1 {
				t.Fatal("гонка потеряла или повторила unknown")
			}
			if operation == "очистка" {
				if scenario(t, f, 100) != nil || scenario(t, f, 101) == nil {
					t.Fatal("очистка восстановила старую запись или удалила новую")
				}
			} else if hasEvent(t, f, 3) {
				t.Fatal("отклонённое событие сохранено")
			}
		})
	}
}

func TestProcessConcurrentCapacity(t *testing.T) {
	for _, scope := range []string{"сценарии", "события"} {
		t.Run(scope, func(t *testing.T) {
			f := newFixture(t, memory.Limits{MaxScenarios: 1, MaxEventsPerScenario: 2})
			firstScenario, secondScenario := 100, 101
			if scope == "события" {
				secondScenario = firstScenario
				process(t, f, event(3, 100, domain.EventTypeLoginFailed, f.base), nil)
			}
			got := together(t,
				processOperation(f, event(1, firstScenario, domain.EventTypeLoginFailed, f.base)),
				processOperation(f, event(2, secondScenario, domain.EventTypeLoginFailed, f.base)),
			)
			assertErrors(t, got, map[error]int{nil: 1, domain.ErrCapacityExceeded: 1})
			for i, err := range got {
				if hasEvent(t, f, i+1) != (err == nil) {
					t.Fatal("индекс не соответствует вместимости")
				}
			}
			if snapshot(t, f).Completed[domain.OutcomeTechnicalError] != 1 {
				t.Fatal("приём последнего места повторно увеличил счётчик завершений")
			}
		})
	}
}

func TestSDKCollectConcurrentCommit(t *testing.T) {
	f := newFixture(t, memory.DefaultLimits())
	process(t, f, event(1, 100, domain.EventTypeLoginStarted, f.base), nil)
	process(t, f, event(2, 100, domain.EventTypeLoginModeDetermined, f.base), nil)
	got := together(t,
		processOperation(f, event(3, 100, domain.EventTypeLoginSucceeded, f.base)),
		func() error {
			var data metricdata.ResourceMetrics
			// Не требуем согласованного снимка разных инструментов во время Commit.
			return f.recorder.Reader().Collect(t.Context(), &data)
		},
	)
	assertErrors(t, got, map[error]int{nil: 2})
	a := snapshot(t, f)
	if a.Completed[domain.OutcomeSuccess] != 1 || a.Durations[domain.LoginModeCredentials].Count != 1 {
		t.Fatalf("сбор после завершения потерял измерения: %+v", a)
	}
}

type announcingStore struct {
	domain.Store
	entered chan struct{}
}

func (s announcingStore) Within(operation func(domain.State) error) error {
	close(s.entered)
	return s.Store.Within(operation)
}

type countedClock struct {
	clock *controlledClock
	calls atomic.Int32
}

func (c *countedClock) Now() time.Time {
	c.calls.Add(1)
	return c.clock.Now()
}

func TestOperationTimeInsideLock(t *testing.T) {
	f := newFixture(t, memory.DefaultLimits())
	process(t, f, event(1, 100, domain.EventTypeLoginStarted, f.base), nil)
	locked := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	lockResult := make(chan error, 1)
	go func() {
		lockResult <- f.store.Within(func(domain.State) error {
			close(locked)
			<-release
			return nil
		})
	}()
	await(t, locked)
	clock := &countedClock{clock: f.clock}
	entered := make(chan struct{})
	observer, err := application.NewLoginObserver(announcingStore{Store: f.store, entered: entered}, clock)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := observer.Process(event(2, 100, domain.EventTypeLoginSucceeded, f.base))
		result <- err
	}()
	await(t, entered)
	if clock.calls.Load() != 0 {
		t.Fatal("время получено до освобождения блокировки")
	}
	f.clock.Set(f.base.Add(15 * time.Minute))
	release <- struct{}{}
	for _, ch := range []<-chan error{result, lockResult} {
		select {
		case got := <-ch:
			if ch == result && !errors.Is(got, domain.ErrScenarioExpired) {
				t.Fatalf("использовано время до входа в блокировку: %v", got)
			}
			if ch == lockResult && got != nil {
				t.Fatal(got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("операция не завершилась после освобождения блокировки")
		}
	}
	if clock.calls.Load() != 1 || snapshot(t, f).Completed[domain.OutcomeUnknown] != 1 {
		t.Fatal("операция должна использовать одно время и увеличить счётчик unknown один раз")
	}
	observer, err = application.NewLoginObserver(f.store, clock)
	if err != nil {
		t.Fatal(err)
	}
	_ = snapshot(t, f)
	if clock.calls.Load() != 1 {
		t.Fatal("снимок не должен получать время или выполнять обслуживание")
	}
	f.clock.Set(f.base.Add(30 * time.Minute))
	if err := observer.Maintain(); err != nil {
		t.Fatal(err)
	}
	if clock.calls.Load() != 2 || scenario(t, f, 100) != nil {
		t.Fatal("обслуживание должно использовать одно время для очистки")
	}
}

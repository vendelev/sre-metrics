package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"

	"github.com/vendelev/sre-metrics/user-login/internal/application"
	"github.com/vendelev/sre-metrics/user-login/internal/domain/contract"
	dError "github.com/vendelev/sre-metrics/user-login/internal/domain/error"
	dEvent "github.com/vendelev/sre-metrics/user-login/internal/domain/event"
	dScenario "github.com/vendelev/sre-metrics/user-login/internal/domain/scenario"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/memory"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/otel"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/otel/oteltest"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/queue"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
}

// barrierClock приостанавливает одну предметную операцию на существующей
// границе часов. Это позволяет воспроизвести незавершённое обслуживание
// без хука в воркере.
type barrierClock struct {
	*testClock
	armed                    atomic.Bool
	entered, release, exited chan struct{}
}

func newBarrierClock(now time.Time) *barrierClock {
	return &barrierClock{
		testClock: &testClock{now: now},
		entered:   make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{}),
	}
}

func (c *barrierClock) Now() time.Time {
	if c.armed.CompareAndSwap(true, false) {
		close(c.entered)
		<-c.release
		defer close(c.exited)
	}
	return c.testClock.Now()
}

// waitSignal ограничивает ожидание сигнала, чтобы сбой теста не подвешивал набор.
func waitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("не дождались сигнала")
	}
}

// waitResult ограничивает ожидание завершения задачи.
func waitResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("не дождались завершения задачи")
		return nil
	}
}

func ingestEvent(id int, kind dEvent.Type, at time.Time) *dEvent.Request {
	e := &dEvent.Request{
		EventID:    uuid.MustParse(fmt.Sprintf("019aabcd-0000-7000-8000-%012x", id)),
		ScenarioID: uuid.MustParse("019aabcd-0000-7000-9000-000000000100"),
		OccurredAt: at,
		Type:       kind,
		Source:     dEvent.SourceMIS,
	}
	if kind == dEvent.LoginModeDetermined {
		mode := dEvent.LoginModeCredentials
		e.Source = dEvent.SourceAuth
		e.AuthMode = &mode
	}
	return e
}

func workerFixture(
	t *testing.T,
	limits memory.Limits,
	c contract.Clock,
) (*Worker, *memory.Store, *otel.Recorder, *bytes.Buffer) {
	t.Helper()
	recorder := oteltest.New(t)
	store, err := memory.NewStore(limits, recorder)
	if err != nil {
		t.Fatal(err)
	}
	observer, err := application.NewLoginObserver(store, c)
	if err != nil {
		t.Fatal(err)
	}
	ingest := queue.NewEventQueue(16)
	logs := new(bytes.Buffer)
	// Интервал обслуживания прежней фикстуры config.Default(); обслуживание
	// отключается до проверок, тики не влияют на исход.
	w, err := New(observer, ingest, time.Second, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return w, store, recorder, logs
}

// drainWorker дочитывает закрытую очередь текущим воркером: планирование
// обслуживания прекращается, как при штатной остановке до HTTP Shutdown.
func drainWorker(t *testing.T, w *Worker) {
	t.Helper()
	w.StopMaintenance()
	w.queue.Close()
	if err := w.Run(); err != nil {
		t.Fatal(err)
	}
}

func logRecords(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	decoder := json.NewDecoder(bytes.NewReader(logs.Bytes()))
	for {
		var entry map[string]any
		err := decoder.Decode(&entry)
		if errors.Is(err, io.EOF) {
			return records
		}
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, entry)
	}
}

// BR-38, BR-49, BR-57, BR-58: FIFO, повторы и безопасная диагностика обработки.
func TestWorkerFIFOAndDiagnostics(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	w, store, recorder, logs := workerFixture(t, memory.DefaultLimits(), &testClock{now: now})
	events := []*dEvent.Request{
		ingestEvent(1, dEvent.LoginStarted, now.Add(5*time.Second)),
		ingestEvent(2, dEvent.LoginStarted, now.Add(6*time.Second)),
		ingestEvent(3, dEvent.LoginModeDetermined, now),
		ingestEvent(4, dEvent.LoginSucceeded, now.Add(3*time.Second)),
		ingestEvent(4, dEvent.LoginSucceeded, now.Add(3*time.Second)),
		ingestEvent(5, dEvent.LoginFailed, now),
	}
	for _, e := range events {
		if err := w.queue.Enqueue(e); err != nil {
			t.Fatal(err)
		}
	}
	drainWorker(t, w)
	{
		scenario, ok := store.FindScenario(events[0].ScenarioID)
		if !ok || scenario.Outcome() != dScenario.OutcomeSuccess {
			t.Fatal("не сохранён первый исход")
		}
		start, _ := scenario.StartedAt()
		if !start.Equal(events[0].OccurredAt) {
			t.Fatal("нарушен FIFO начала")
		}
		for _, id := range []int{1, 3, 4} {
			if _, ok := store.FindEvent(ingestEvent(id, dEvent.LoginStarted, now).EventID); !ok {
				t.Fatal("потерян принятый ID")
			}
		}
		for _, id := range []int{2, 5} {
			if _, ok := store.FindEvent(ingestEvent(id, dEvent.LoginStarted, now).EventID); ok {
				t.Fatal("сохранён конфликтующий ID")
			}
		}
	}
	values := oteltest.Read(t, recorder)
	if values.Completed[dScenario.OutcomeSuccess] != 1 || values.Durations[dEvent.LoginModeCredentials].Count != 0 {
		t.Fatal("неверный учёт успеха или отрицательной длительности")
	}
	records := logRecords(t, logs)
	if len(records) != 3 {
		t.Fatalf("ожидались два конфликта и одно предупреждение, получено %s", logs.String())
	}
	for _, index := range []int{0, 2} {
		entry := records[index]
		if entry["reason"] != "event_conflict" || entry["scenario_id"] != events[0].ScenarioID.String() ||
			entry["type"] == nil ||
			entry["event_id"] == nil {
			t.Fatalf("неполная диагностика отказа: %v", entry)
		}
	}
	if records[1]["level"] != "WARN" || records[1]["scenario_id"] != events[0].ScenarioID.String() ||
		len(records[1]) != 4 {
		t.Fatalf("неверная диагностика отрицательной длительности: %v", records[1])
	}
	for _, forbidden := range []string{"occurred_at", "credentials", "AuthMode"} {
		if strings.Contains(logs.String(), forbidden) {
			t.Fatal("лог содержит полное событие или его лишние поля")
		}
	}
}

func TestWorkerRefusalReasons(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "лимит", true: "закрытый сценарий"}[expired], func(t *testing.T) {
			now := time.Now()
			w, _, _, logs := workerFixture(
				t,
				memory.Limits{MaxScenarios: 1, MaxEventsPerScenario: 1},
				&testClock{now: now},
			)
			at := now
			if expired {
				at = now.Add(-20 * time.Minute)
			}
			if err := w.queue.Enqueue(ingestEvent(1, dEvent.LoginStarted, at)); err != nil {
				t.Fatal(err)
			}
			if err := w.queue.Enqueue(ingestEvent(2, dEvent.LoginSucceeded, now)); err != nil {
				t.Fatal(err)
			}
			drainWorker(t, w)
			want := "capacity_exceeded"
			if expired {
				want = "scenario_expired"
			}
			records := logRecords(t, logs)
			if len(records) != 1 || records[0]["reason"] != want {
				t.Fatalf("неверная диагностика: %s", logs.String())
			}
		})
	}
}

func TestWorkerDoesNotRecheckQueuedAge(t *testing.T) {
	c := &testClock{now: time.Now()}
	w, _, recorder, _ := workerFixture(t, memory.DefaultLimits(), c)
	if err := w.queue.Enqueue(ingestEvent(1, dEvent.LoginSucceeded, c.Now().Add(-30*time.Minute))); err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Minute)
	drainWorker(t, w)
	if oteltest.Read(t, recorder).Completed[dScenario.OutcomeSuccess] != 1 {
		t.Fatal("очередь отменила допустимость возраста при приёме")
	}
}

// BR-12, BR-55: время динамического решения берётся при обработке,
// а не при постановке события, ожидавшего занятого воркера.
func TestWorkerDeadlineAfterQueueWait(t *testing.T) {
	for _, offset := range []time.Duration{-time.Millisecond, 0} {
		t.Run(offset.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newBarrierClock(time.Now())
				now := c.Now()
				w, store, recorder, _ := workerFixture(t, memory.DefaultLimits(), c)
				mode := ingestEvent(2, dEvent.LoginModeDetermined, now)
				for _, e := range []*dEvent.Request{ingestEvent(1, dEvent.LoginStarted, now), mode} {
					if err := w.queue.Enqueue(e); err != nil {
						t.Fatal(err)
					}
				}
				done := make(chan error, 1)
				finished := make(chan struct{})
				go func() {
					done <- w.Run()
					close(finished)
				}()
				release := sync.OnceFunc(func() { close(c.release) })
				t.Cleanup(func() {
					w.Stop()
					w.queue.Close()
					release()
					waitSignal(t, finished)
				})
				synctest.Wait()
				c.armed.Store(true)
				// Точный повтор занимает воркер, но не должен сам закрыть сценарий.
				if err := w.queue.Enqueue(mode); err != nil {
					t.Fatal(err)
				}
				waitSignal(t, c.entered)
				success := ingestEvent(3, dEvent.LoginSucceeded, now.Add(time.Second))
				if err := w.queue.Enqueue(success); err != nil {
					t.Fatal(err)
				}
				c.Advance(15*time.Minute + offset)
				w.StopMaintenance()
				w.queue.Close()
				release()
				if err := waitResult(t, done); err != nil {
					t.Fatal(err)
				}
				want := dScenario.OutcomeSuccess
				if offset == 0 {
					want = dScenario.OutcomeUnknown
				}
				scenario, ok := store.FindScenario(success.ScenarioID)
				if !ok || scenario.Outcome() != want || oteltest.Read(t, recorder).Completed[want] != 1 {
					t.Fatal("ожидание в очереди не учтено при проверке deadline")
				}
				if _, ok := store.FindEvent(success.EventID); ok != (offset < 0) {
					t.Fatal("индекс событий не соответствует решению на границе deadline")
				}
			})
		})
	}
}

// Реальный memory не выдаёт произвольную техническую ошибку. Подмена
// существующего domain.Store нужна для проверки отказа обработки и того,
// что отказ воркера закрывает приём.
type failedStore struct {
	contract.Store
	err error
}

func (s failedStore) RemoveRetained(time.Time) error {
	return s.err
}

func TestWorkerInternalFailureStopsAdmission(t *testing.T) {
	c := &testClock{now: time.Now()}
	w, _, _, logs := workerFixture(t, memory.DefaultLimits(), c)
	cause := errors.New("синтетический сбой обработки")
	observer, err := application.NewLoginObserver(failedStore{err: cause}, c)
	if err != nil {
		t.Fatal(err)
	}
	w.observer = observer
	if err := w.queue.Enqueue(ingestEvent(1, dEvent.LoginSucceeded, c.Now())); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- w.Run() }()
	if err := waitResult(t, done); !errors.Is(err, cause) {
		t.Fatalf("сбой обработки потерян: %v", err)
	}
	// Выход воркера закрывает очередь: приём остановлен вместе с обработкой.
	if err := w.queue.Enqueue(
		ingestEvent(2, dEvent.LoginSucceeded, c.Now()),
	); err == nil ||
		errors.Is(err, dError.ErrOverloaded) {
		t.Fatal("после сбоя обработчика приём продолжился")
	}
	if entries := logRecords(t, logs); len(entries) != 1 || entries[0]["reason"] != "internal_error" {
		t.Fatal("внутренний отказ не диагностирован")
	}
}

// Один цикл не запускает Maintain одновременно с Process; после отмены новые
// события не обрабатываются. Барьер использует существующую зависимость часов.
// Маршрут ошибки задачи в сигнал остановки проверяют сервисные тесты.
func TestWorkerStopsSchedulingAndHonorsCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newBarrierClock(time.Now())
		w, _, _, _ := workerFixture(t, memory.DefaultLimits(), c)
		for id := 1; id <= 2; id++ {
			if err := w.queue.Enqueue(ingestEvent(id, dEvent.LoginSucceeded, c.Now())); err != nil {
				t.Fatal(err)
			}
		}
		c.armed.Store(true)
		done := make(chan error, 1)
		go func() { done <- w.Run() }()
		waitSignal(t, c.entered)
		time.Sleep(3 * time.Second)
		synctest.Wait()
		w.StopMaintenance()
		w.Stop()
		close(c.release)
		if err := waitResult(t, done); err != nil {
			t.Fatalf("отмена не завершила воркер: %v", err)
		}
		if len(w.queue.Events()) != 1 {
			t.Fatal("после отмены началась обработка следующего события")
		}
	})
}

func TestWorkerMaintenanceAndDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &testClock{now: time.Now()}
		w, store, recorder, _ := workerFixture(t, memory.DefaultLimits(), c)
		if err := w.queue.Enqueue(ingestEvent(1, dEvent.LoginStarted, c.Now())); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- w.Run() }()
		synctest.Wait()
		c.Advance(15 * time.Minute)
		time.Sleep(time.Second)
		synctest.Wait()
		if oteltest.Read(t, recorder).Completed[dScenario.OutcomeUnknown] != 1 {
			t.Fatal("периодическое обслуживание не закрыло ожидание")
		}
		w.StopMaintenance()
		synctest.Wait()
		c.Advance(15 * time.Minute)
		time.Sleep(3 * time.Second)
		synctest.Wait()
		w.queue.Close()
		if err := waitResult(t, done); err != nil {
			t.Fatal(err)
		}
		// Состояние читается только после завершения его единственного владельца.
		// Без обработки новых событий запись с истёкшим retention остаётся в памяти,
		// если после отмены не было новых вызовов обслуживания.
		scenario, ok := store.FindScenario(ingestEvent(1, dEvent.LoginStarted, c.Now()).ScenarioID)
		if !ok || scenario.Outcome() != dScenario.OutcomeUnknown {
			t.Fatal("после отмены обслуживания выполнена новая очистка")
		}
		before := oteltest.Read(t, recorder)
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if oteltest.Read(t, recorder).Completed[dScenario.OutcomeUnknown] != before.Completed[dScenario.OutcomeUnknown] {
			t.Fatal("после завершения появились новые измерения")
		}
	})
}

func TestWorkerCancellationWithClosedQueue(t *testing.T) {
	w, _, _, _ := workerFixture(t, memory.DefaultLimits(), &testClock{now: time.Now()})
	if err := w.queue.Enqueue(ingestEvent(1, dEvent.LoginSucceeded, time.Now())); err != nil {
		t.Fatal(err)
	}
	w.queue.Close()
	w.Stop()
	if err := w.Run(); err != nil {
		t.Fatalf("отмена владельцем стала ошибкой воркера: %v", err)
	}
	if len(w.queue.Events()) != 1 {
		t.Fatal("после отмены началась обработка закрытой очереди")
	}
}

// AC-06/14/17/22/23: конкурируют отправители, а не операции с памятью.
// Состояние и индекс проверяются только после завершения единственного воркера.
func TestWorkerConcurrentSenders(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	start := ingestEvent(10, dEvent.LoginStarted, now)
	mode := ingestEvent(11, dEvent.LoginModeDetermined, now)
	success := ingestEvent(12, dEvent.LoginSucceeded, now.Add(2*time.Second))
	otherMode := ingestEvent(2, dEvent.LoginModeDetermined, now)
	*otherMode.AuthMode = dEvent.LoginModeExistingSession
	otherScenario := ingestEvent(2, dEvent.LoginFailed, now)
	otherScenario.ScenarioID = uuid.MustParse("019aabcd-0000-7000-9000-000000000101")
	duplicates := make([]*dEvent.Request, 12)
	for i := range duplicates {
		duplicates[i] = success
	}
	cases := []struct {
		name          string
		limits        memory.Limits
		before        []*dEvent.Request
		concurrent    []*dEvent.Request
		after         []*dEvent.Request
		stored        int
		conflicts     int
		capacity      int
		wantDurations uint64
	}{
		{
			name:          "точные повторы",
			before:        []*dEvent.Request{start, mode},
			concurrent:    duplicates,
			stored:        12,
			wantDurations: 1,
		},
		{
			name:   "разные исходы",
			before: []*dEvent.Request{start, mode},
			concurrent: []*dEvent.Request{
				success,
				ingestEvent(1, dEvent.LoginFailed, now),
				ingestEvent(2, dEvent.LoginRejected, now),
			},
			stored:    1,
			conflicts: 2,
		},
		{
			name:          "разные начала",
			concurrent:    []*dEvent.Request{start, ingestEvent(1, dEvent.LoginStarted, now.Add(time.Second))},
			after:         []*dEvent.Request{mode, success},
			stored:        1,
			conflicts:     1,
			wantDurations: 1,
		},
		{
			name:          "разные варианты",
			before:        []*dEvent.Request{start},
			concurrent:    []*dEvent.Request{mode, otherMode},
			after:         []*dEvent.Request{success},
			stored:        1,
			conflicts:     1,
			wantDurations: 1,
		},
		{
			name:       "последнее место сценария",
			limits:     memory.Limits{MaxScenarios: 1, MaxEventsPerScenario: 2},
			concurrent: []*dEvent.Request{ingestEvent(1, dEvent.LoginFailed, now), otherScenario},
			stored:     1,
			capacity:   1,
		},
		{
			name:   "последнее место события",
			limits: memory.Limits{MaxScenarios: 1, MaxEventsPerScenario: 2},
			before: []*dEvent.Request{ingestEvent(10, dEvent.LoginFailed, now)},
			concurrent: []*dEvent.Request{
				ingestEvent(1, dEvent.LoginFailed, now),
				ingestEvent(2, dEvent.LoginFailed, now),
			},
			stored:   1,
			capacity: 1,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			limits := tt.limits
			if limits == (memory.Limits{}) {
				limits = memory.DefaultLimits()
			}
			w, store, recorder, logs := workerFixture(t, limits, &testClock{now: now})
			for _, e := range tt.before {
				if err := w.queue.Enqueue(e); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			finished := make(chan struct{})
			go func() {
				done <- w.Run()
				close(finished)
			}()
			t.Cleanup(func() {
				w.Stop()
				w.queue.Close()
				waitSignal(t, finished)
			})
			ready := make(chan struct{}, len(tt.concurrent))
			barrier := make(chan struct{})
			release := sync.OnceFunc(func() { close(barrier) })
			defer release()
			results := make(chan error, len(tt.concurrent))
			for _, e := range tt.concurrent {
				go func() {
					ready <- struct{}{}
					<-barrier
					results <- w.queue.Enqueue(e)
				}()
			}
			for range tt.concurrent {
				waitSignal(t, ready)
			}
			release()
			for range tt.concurrent {
				if err := waitResult(t, results); err != nil {
					t.Fatalf("приём не должен зависеть от предметного решения: %v", err)
				}
			}
			for _, e := range tt.after {
				if err := w.queue.Enqueue(e); err != nil {
					t.Fatal(err)
				}
			}
			w.StopMaintenance()
			w.queue.Close()
			if err := waitResult(t, done); err != nil {
				t.Fatal(err)
			}
			stored := 0
			for _, e := range tt.concurrent {
				if _, ok := store.FindEvent(e.EventID); !ok {
					continue
				}
				stored++
				scenario, ok := store.FindScenario(e.ScenarioID)
				if !ok {
					t.Fatal("индекс ссылается на отсутствующий сценарий")
				}
				switch e.Type {
				case dEvent.LoginStarted:
					if at, _ := scenario.StartedAt(); !at.Equal(e.OccurredAt) {
						t.Fatal("начало не соответствует победившему событию")
					}
				case dEvent.LoginModeDetermined:
					if mode, _ := scenario.Mode(); mode != *e.AuthMode {
						t.Fatal("вариант не соответствует победившему событию")
					}
				case dEvent.LoginSucceeded, dEvent.LoginFailed, dEvent.LoginRejected:
					outcomes := map[dEvent.Type]dScenario.Outcome{
						dEvent.LoginSucceeded: dScenario.OutcomeSuccess,
						dEvent.LoginFailed:    dScenario.OutcomeTechnicalError,
						dEvent.LoginRejected:  dScenario.OutcomeRejected,
					}
					if scenario.Outcome() != outcomes[e.Type] {
						t.Fatal("исход не соответствует победившему событию")
					}
				}
			}
			if stored != tt.stored {
				t.Fatalf("сохранено входов %d, ожидалось %d", stored, tt.stored)
			}
			reasons := make(map[string]int)
			records := logRecords(t, logs)
			for _, entry := range records {
				reasons[entry["reason"].(string)]++
			}
			if len(records) != tt.conflicts+tt.capacity ||
				reasons["event_conflict"] != tt.conflicts || reasons["capacity_exceeded"] != tt.capacity {
				t.Fatalf("неверные отказы обработки: %s", logs.String())
			}
			values := oteltest.Read(t, recorder)
			var completions int64
			for _, count := range values.Completed {
				completions += count
			}
			if completions != 1 {
				t.Fatalf("ожидался единственный исход, получено %d", completions)
			}
			var durations uint64
			for _, histogram := range values.Durations {
				durations += histogram.Count
			}
			if tt.name != "разные исходы" && durations != tt.wantDurations {
				t.Fatalf("наблюдений длительности %d, ожидалось %d", durations, tt.wantDurations)
			}
			if tt.name == "разные исходы" && durations != uint64(values.Completed[dScenario.OutcomeSuccess]) {
				t.Fatal("длительность не соответствует единственному исходу")
			}
		})
	}
}

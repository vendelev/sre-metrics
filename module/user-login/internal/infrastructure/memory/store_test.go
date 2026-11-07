package memory_test

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vendelev/sre-metrics/user-login/internal/application"
	"github.com/vendelev/sre-metrics/user-login/internal/domain/contract"
	dError "github.com/vendelev/sre-metrics/user-login/internal/domain/error"
	event2 "github.com/vendelev/sre-metrics/user-login/internal/domain/event"
	scenario2 "github.com/vendelev/sre-metrics/user-login/internal/domain/scenario"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/memory"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/otel"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/otel/oteltest"
)

type controlledClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *controlledClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *controlledClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

type fixture struct {
	observer *application.LoginObserver
	store    *memory.Store
	clock    *controlledClock
	base     time.Time
	recorder *otel.Recorder
}

func newFixture(t *testing.T, limits memory.Limits) *fixture {
	t.Helper()
	recorder := oteltest.New(t)
	store, err := memory.NewStore(limits, recorder)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, time.November, 2, 12, 0, 0, 0, time.UTC)
	clock := &controlledClock{now: base}
	observer, err := application.NewLoginObserver(store, clock)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{observer: observer, store: store, clock: clock, base: base, recorder: recorder}
}

func testID(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("019aabcd-0000-7000-8000-%012x", n))
}

func event(id, scenario int, kind event2.Type, at time.Time) *event2.Request {
	e := event2.Request{
		EventID:    testID(id),
		ScenarioID: testID(scenario),
		Type:       kind,
		Source:     event2.SourceMIS,
		OccurredAt: at,
	}
	if kind == event2.LoginModeDetermined {
		mode := event2.LoginModeCredentials
		e.Source = event2.SourceAuth
		e.AuthMode = &mode
	}
	return &e
}

func process(t *testing.T, f *fixture, e *event2.Request, want error) application.ProcessResult {
	t.Helper()
	result, err := f.observer.Process(e)
	if !errors.Is(err, want) {
		t.Fatalf("обработка %s: %v, ожидалось %v", e.Type, err, want)
	}
	return result
}

func snapshot(t *testing.T, f *fixture) oteltest.Values {
	t.Helper()
	return oteltest.Read(t, f.recorder)
}

func scenario(t *testing.T, f *fixture, id int) *scenario2.Scenario {
	t.Helper()
	var result *scenario2.Scenario
	if err := f.store.Within(func(state contract.State) error {
		result, _ = state.FindScenario(testID(id))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

func hasEvent(t *testing.T, f *fixture, id int) bool {
	t.Helper()
	var exists bool
	if err := f.store.Within(func(state contract.State) error {
		_, exists = state.FindEvent(testID(id))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return exists
}

func maintain(t *testing.T, f *fixture) {
	t.Helper()
	if err := f.observer.Maintain(); err != nil {
		t.Fatal(err)
	}
}

func TestNewStoreLimits(t *testing.T) {
	if _, err := memory.NewStore(memory.DefaultLimits(), nil); err == nil {
		t.Fatal("отсутствие регистратора не проверено")
	}
	if got := memory.DefaultLimits(); got != (memory.Limits{MaxScenarios: 10_000, MaxEventsPerScenario: 32}) {
		t.Fatalf("начальные ограничения: %+v", got)
	}
	for _, limits := range []memory.Limits{
		{}, {MaxScenarios: -1, MaxEventsPerScenario: 1},
		{MaxScenarios: 1}, {MaxScenarios: 1, MaxEventsPerScenario: -1},
	} {
		t.Run(fmt.Sprintf("%d/%d", limits.MaxScenarios, limits.MaxEventsPerScenario), func(t *testing.T) {
			if _, err := memory.NewStore(limits, oteltest.New(t)); err == nil {
				t.Fatal("недопустимые ограничения приняты")
			}
		})
	}
}

// BR-49, BR-53, AC-44: глобальный ID и равенство разобранных значений.
func TestProcessDeduplicationAndEquality(t *testing.T) {
	f := newFixture(t, memory.DefaultLimits())
	e := event(1, 100, event2.LoginModeDetermined, f.base)
	before := e.Clone()
	process(t, f, e, nil)
	if !reflect.DeepEqual(*e, before) {
		t.Fatal("обработка изменила входное событие")
	}
	copy := e.Clone()
	copy.OccurredAt = copy.OccurredAt.In(time.FixedZone("UTC+3", 3*60*60))
	process(t, f, &copy, nil)
	// Изменение входного указателя после приёма не меняет сохранённую историю.
	*e.AuthMode = event2.LoginModeExistingSession
	process(t, f, &copy, nil)
	process(t, f, e, dError.ErrConflict)
	changes := []struct {
		name   string
		change func(*event2.Request)
	}{
		{name: "другой сценарий", change: func(e *event2.Request) { e.ScenarioID = testID(101) }},
		{name: "другое время", change: func(e *event2.Request) { e.OccurredAt = e.OccurredAt.Add(time.Millisecond) }},
		{name: "другой вариант", change: func(e *event2.Request) { *e.AuthMode = event2.LoginModeExistingSession }},
		{name: "другой тип", change: func(e *event2.Request) { e.Type = event2.LoginFailed; e.AuthMode = nil }},
	}
	for _, tt := range changes {
		t.Run(tt.name, func(t *testing.T) {
			changed := copy.Clone()
			tt.change(&changed)
			process(t, f, &changed, dError.ErrConflict)
		})
	}
	if scenario(t, f, 101) != nil || snapshot(t, f).Completed[scenario2.OutcomeSuccess] != 0 {
		t.Fatal("конфликт создал сценарий или измерение")
	}
	failed := event(2, 102, event2.LoginFailed, f.base)
	process(t, f, failed, nil)
	failed.Source = event2.SourceAuth
	process(t, f, failed, dError.ErrConflict)
	process(t, f, event(3, 100, event2.LoginSucceeded, f.base), nil)
	if got := scenario(t, f, 100); got.Outcome() != scenario2.OutcomeSuccess {
		t.Fatal("новое событие не дополнило существующий сценарий")
	}
}

// BR-37, BR-49, BR-55: точный повтор старого ID не проверяет возраст и deadline.
func TestProcessAgeAndDeduplicationPriority(t *testing.T) {
	f := newFixture(t, memory.DefaultLimits())
	e := event(1, 100, event2.LoginModeDetermined, f.base)
	process(t, f, e, nil)
	f.clock.Set(f.base.Add(31 * time.Minute))
	process(t, f, e, nil)
	changed := e.Clone()
	changed.ScenarioID = testID(101)
	process(t, f, &changed, dError.ErrConflict)
	newID := e.Clone()
	newID.EventID = testID(2)
	_, err := f.observer.Process(&newID)
	if !errors.Is(err, dError.ErrInvalidEvent) || !errors.Is(err, dError.ErrEventTime) {
		t.Fatalf("возраст должен сохранять обе категории: %v", err)
	}
	if got := scenario(t, f, 100).Outcome(); got != "" {
		t.Fatalf("ранний возврат закрыл ожидание: %q", got)
	}
	maintain(t, f)
	if got := snapshot(t, f).Completed[scenario2.OutcomeUnknown]; got != 1 {
		t.Fatalf("самостоятельное обслуживание: %d", got)
	}
}

func TestProcessNewEventAgeBoundaries(t *testing.T) {
	for _, offset := range []time.Duration{
		-30*time.Minute - time.Millisecond, -30 * time.Minute, 30 * time.Second, 30*time.Second + time.Millisecond,
	} {
		t.Run(offset.String(), func(t *testing.T) {
			f := newFixture(t, memory.DefaultLimits())
			var want error
			if offset < -30*time.Minute || offset > 30*time.Second {
				want = dError.ErrInvalidEvent
			}
			process(t, f, event(1, 100, event2.LoginRejected, f.base.Add(offset)), want)
			if (scenario(t, f, 100) != nil) != (want == nil) {
				t.Fatal("отказ по возрасту сохранил запись или допустимый возраст не принят")
			}
		})
	}
}

// AC-23, AC-40, AC-47: приоритеты и независимое закрытие при полном лимите.
func TestProcessCapacityAndIndependentExpiry(t *testing.T) {
	f := newFixture(t, memory.Limits{MaxScenarios: 1, MaxEventsPerScenario: 2})
	started := event(1, 100, event2.LoginStarted, f.base)
	mode := event(2, 100, event2.LoginModeDetermined, f.base)
	process(t, f, started, nil)
	process(t, f, mode, nil)
	process(t, f, started, nil)
	process(t, f, event(3, 100, event2.LoginSucceeded, f.base), dError.ErrCapacityExceeded)
	process(t, f, event(4, 101, event2.LoginRejected, f.base), dError.ErrCapacityExceeded)
	conflict := event(5, 100, event2.LoginStarted, f.base.Add(time.Second))
	f.clock.Set(f.base.Add(15 * time.Minute))
	process(t, f, conflict, dError.ErrConflict)
	if scenario(t, f, 100).Outcome() != "" {
		t.Fatal("конфликт не должен закрывать ожидание")
	}
	confirmation := event(6, 100, event2.LoginModeDetermined, f.clock.Now())
	process(t, f, confirmation, dError.ErrCapacityExceeded)
	closed := scenario(t, f, 100)
	closedAt, ok := closed.ClosedAt()
	if !ok || !closedAt.Equal(f.clock.Now()) || closed.Outcome() != scenario2.OutcomeUnknown {
		t.Fatal("закрытие при отказе не сохранилось")
	}
	process(t, f, confirmation, dError.ErrCapacityExceeded)
	process(t, f, event(7, 100, event2.LoginSucceeded, f.clock.Now()), scenario2.ErrScenarioExpired)
	if hasEvent(t, f, 3) || hasEvent(t, f, 6) || hasEvent(t, f, 7) || scenario(t, f, 101) != nil {
		t.Fatal("отклонённые события занимают память")
	}
	maintain(t, f)
	if got := snapshot(t, f).Completed[scenario2.OutcomeUnknown]; got != 1 {
		t.Fatalf("закрытие должно учитываться один раз: %d", got)
	}
	if deadline, _ := closed.RetentionDeadline(); !deadline.Equal(f.clock.Now().Add(15 * time.Minute)) {
		t.Fatal("retention должен идти от самостоятельного закрытия")
	}
}

func TestProcessScenarioLimitAndSemanticConfirmation(t *testing.T) {
	f := newFixture(t, memory.Limits{MaxScenarios: 1, MaxEventsPerScenario: 2})
	process(t, f, event(1, 100, event2.LoginFailed, f.base), nil)
	process(t, f, event(2, 101, event2.LoginFailed, f.base), dError.ErrCapacityExceeded)
	process(t, f, event(3, 100, event2.LoginFailed, f.base.Add(time.Second)), nil)
	process(t, f, event(4, 100, event2.LoginFailed, f.base), dError.ErrCapacityExceeded)
	process(t, f, event(5, 100, event2.LoginRejected, f.base), dError.ErrConflict)
	if got := snapshot(t, f).Completed[scenario2.OutcomeTechnicalError]; got != 1 {
		t.Fatalf("смысловое подтверждение повторно увеличило счётчик завершений: %d", got)
	}
}

// BR-52: первое просроченное начало не создаёт unknown без места для приёма.
func TestProcessFirstOverdueStart(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, full := range []bool{false, true} {
			t.Run(fmt.Sprintf("существует=%t/заполнено=%t", existing, full), func(t *testing.T) {
				limit := 2
				if full {
					limit = 1
				}
				f := newFixture(t, memory.Limits{MaxScenarios: 1, MaxEventsPerScenario: limit})
				if existing {
					process(t, f, event(1, 100, event2.LoginModeDetermined, f.base), nil)
				} else if full {
					process(t, f, event(1, 101, event2.LoginRejected, f.base), nil)
				}
				var want error
				wantUnknown := int64(1)
				if full {
					want = dError.ErrCapacityExceeded
					wantUnknown = 0
				}
				process(t, f, event(2, 100, event2.LoginStarted, f.base.Add(-20*time.Minute)), want)
				if got := snapshot(t, f).Completed[scenario2.OutcomeUnknown]; got != wantUnknown {
					t.Fatalf("unknown=%d, ожидалось %d", got, wantUnknown)
				}
				if full && !existing && scenario(t, f, 100) != nil {
					t.Fatal("непринятое начало создало запись")
				}
				if full && existing {
					if _, ok := scenario(t, f, 100).StartedAt(); ok {
						t.Fatal("непринятое начало дополнило запись")
					}
				}
			})
		}
	}
}

// AC-48: на логической границе прежний успех уже нельзя дополнить.
func TestProcessRetentionBoundaries(t *testing.T) {
	for _, offset := range []time.Duration{-time.Millisecond, 0, time.Millisecond} {
		t.Run(offset.String(), func(t *testing.T) {
			f := newFixture(t, memory.Limits{MaxScenarios: 1, MaxEventsPerScenario: 3})
			process(t, f, event(1, 100, event2.LoginStarted, f.base.Add(-time.Second)), nil)
			process(t, f, event(2, 100, event2.LoginSucceeded, f.base), nil)
			f.clock.Set(f.base.Add(15*time.Minute + offset))
			process(t, f, event(3, 100, event2.LoginModeDetermined, f.clock.Now()), nil)
			a := snapshot(t, f)
			wantCount := uint64(0)
			if offset < 0 {
				wantCount = 1
			}
			if a.Completed[scenario2.OutcomeSuccess] != 1 ||
				a.Durations[event2.LoginModeCredentials].Count != wantCount {
				t.Fatalf("неверное дополнение на retention: %+v", a)
			}
			if hasEvent(t, f, 1) != (offset < 0) || hasEvent(t, f, 2) != (offset < 0) {
				t.Fatal("глобальный индекс не соответствует retention")
			}
		})
	}
}

// AC-49: удаляется и владелец ID другого scenario_id, общий лимит освобождается.
func TestProcessRetentionReleasesGlobalIDAndCapacity(t *testing.T) {
	f := newFixture(t, memory.Limits{MaxScenarios: 1, MaxEventsPerScenario: 1})
	process(t, f, event(1, 100, event2.LoginSucceeded, f.base), nil)
	f.clock.Set(f.base.Add(15 * time.Minute))
	process(t, f, event(1, 101, event2.LoginFailed, f.clock.Now()), nil)
	if scenario(t, f, 100) != nil || scenario(t, f, 101).Outcome() != scenario2.OutcomeTechnicalError {
		t.Fatal("retention не освободил запись и глобальный ID")
	}
	a := snapshot(t, f)
	if a.Completed[scenario2.OutcomeSuccess] != 1 || a.Completed[scenario2.OutcomeTechnicalError] != 1 {
		t.Fatal("очистка уменьшила агрегаты")
	}
}

func TestProcessRetentionPersistsAfterRefusal(t *testing.T) {
	f := newFixture(t, memory.DefaultLimits())
	old := event(1, 100, event2.LoginStarted, f.base.Add(-20*time.Minute))
	process(t, f, old, nil)
	f.clock.Set(f.base.Add(15 * time.Minute))
	process(t, f, old, dError.ErrInvalidEvent)
	if scenario(t, f, 100) != nil || hasEvent(t, f, 1) {
		t.Fatal("отказ по возрасту восстановил истёкшую историю")
	}
	if snapshot(t, f).Completed[scenario2.OutcomeUnknown] != 1 {
		t.Fatal("очистка изменила агрегат")
	}
	process(t, f, event(2, 100, event2.LoginSucceeded, f.clock.Now()), nil)
}

func TestMaintainIdempotenceAndRetention(t *testing.T) {
	f := newFixture(t, memory.DefaultLimits())
	process(t, f, event(1, 100, event2.LoginStarted, f.base), nil)
	process(t, f, event(2, 101, event2.LoginModeDetermined, f.base), nil)
	process(t, f, event(3, 102, event2.LoginRejected, f.base), nil)
	f.clock.Set(f.base.Add(15*time.Minute - time.Millisecond))
	maintain(t, f)
	if snapshot(t, f).Completed[scenario2.OutcomeUnknown] != 0 {
		t.Fatal("закрытие раньше deadline")
	}
	f.clock.Set(f.base.Add(15 * time.Minute))
	maintain(t, f)
	maintain(t, f)
	a := snapshot(t, f)
	if a.Completed[scenario2.OutcomeUnknown] != 2 || a.Completed[scenario2.OutcomeRejected] != 1 {
		t.Fatalf("неверные изменения счётчиков при обслуживании: %+v", a)
	}
	if scenario(t, f, 102) != nil || hasEvent(t, f, 3) {
		t.Fatal("retention закрытой записи не очищен")
	}
	f.clock.Set(f.base.Add(30 * time.Minute))
	maintain(t, f)
	maintain(t, f)
	if scenario(t, f, 100) != nil || scenario(t, f, 101) != nil || hasEvent(t, f, 1) || hasEvent(t, f, 2) {
		t.Fatal("очистка должна удалить записи вместе с индексом")
	}
	if !reflect.DeepEqual(a, snapshot(t, f)) {
		t.Fatal("очистка или повторное обслуживание изменили накопленные значения")
	}
}

func TestStoreCopiesAndNoUniversalRollback(t *testing.T) {
	f := newFixture(t, memory.DefaultLimits())
	e := event(1, 100, event2.LoginModeDetermined, f.base)
	process(t, f, e, nil)
	var access contract.State
	errExpected := errors.New("учебный отказ после фиксации")
	if err := f.store.Within(func(state contract.State) error {
		access = state
		stored, _ := state.FindEvent(e.EventID)
		*stored.AuthMode = event2.LoginModeExistingSession
		s, _ := state.FindScenario(e.ScenarioID)
		if err := s.Fail(f.base); err != nil {
			return err
		}
		if err := state.Commit(s, nil, s.TakeMeasurements()); err != nil {
			return err
		}
		if err := s.Start(f.base, f.base); err != nil {
			return err
		}
		return errExpected
	}); !errors.Is(err, errExpected) {
		t.Fatal(err)
	}
	stored := scenario(t, f, 100)
	if stored.Outcome() != scenario2.OutcomeTechnicalError {
		t.Fatal("ошибка callback откатила успешную фиксацию")
	}
	if _, ok := stored.StartedAt(); ok {
		t.Fatal("изменение сущности после Commit проникло в память")
	}
	process(t, f, e, nil)
	if got := snapshot(t, f).TechnicalErrors[event2.LoginModeCredentials]; got != 1 {
		t.Fatal("сущность и изменение счётчика ошибок не согласованы")
	}
	if err := access.CheckCapacity(e.ScenarioID); !errors.Is(err, dError.ErrInternal) {
		t.Fatal("отозванный State разрешил работу вне защищённого участка")
	}
	if err := f.store.Within(
		func(state contract.State) error { return state.DeleteScenario(e.ScenarioID) },
	); err != nil {
		t.Fatal(err)
	}
	if hasEvent(t, f, 1) || scenario(t, f, 100) != nil {
		t.Fatal("удаление должно очищать и запись, и индекс")
	}
}

// Ошибка одной фиксации не сохраняет ни отметки выдачи, ни агрегат, ни новый ID.
func TestCommitRefusalIsAtomic(t *testing.T) {
	cases := []struct {
		name        string
		measurement scenario2.Measurements
	}{
		{name: "неизвестный исход", measurement: scenario2.Measurements{Completion: "other"}},
		{name: "неизвестный вариант ошибки", measurement: scenario2.Measurements{TechnicalErrorMode: "other"}},
		{name: "неизвестный вариант длительности", measurement: scenario2.Measurements{
			SuccessfulDuration: &scenario2.DurationObservation{Mode: "other", Duration: time.Second},
		}},
		{name: "отрицательное наблюдение", measurement: scenario2.Measurements{
			SuccessfulDuration: &scenario2.DurationObservation{
				Mode:     event2.LoginModeCredentials,
				Duration: -time.Second,
			},
		}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, memory.DefaultLimits())
			process(t, f, event(1, 100, event2.LoginModeDetermined, f.base), nil)
			before := snapshot(t, f)
			err := f.store.Within(func(state contract.State) error {
				s, _ := state.FindScenario(testID(100))
				if err := s.Fail(f.base); err != nil {
					return err
				}
				s.TakeMeasurements()
				e := event(2, 100, event2.LoginFailed, f.base)
				return state.Commit(s, e, tt.measurement)
			})
			if !errors.Is(err, dError.ErrInternal) {
				t.Fatalf("некорректная фиксация: %v", err)
			}
			if scenario(t, f, 100).Outcome() != "" || hasEvent(t, f, 2) || !reflect.DeepEqual(before, snapshot(t, f)) {
				t.Fatal("отказ частично сохранил сущность, ID или измерение")
			}
			process(t, f, event(2, 100, event2.LoginFailed, f.base), nil)
			if snapshot(t, f).TechnicalErrors[event2.LoginModeCredentials] != 1 {
				t.Fatal("отклонённая фиксация потеряла отметки выдачи")
			}
		})
	}
}

func TestProcessUnknownFactPriority(t *testing.T) {
	cases := []struct {
		name string
		kind event2.Type
		mode event2.OAuthMode
		want error
	}{
		{name: "известный вариант", kind: event2.LoginModeDetermined, mode: event2.LoginModeCredentials},
		{name: "противоречащий вариант", kind: event2.LoginModeDetermined,
			mode: event2.LoginModeExistingSession, want: dError.ErrConflict},
		{name: "недостающее начало", kind: event2.LoginStarted, want: scenario2.ErrScenarioExpired},
		{name: "поздний исход", kind: event2.LoginSucceeded, want: scenario2.ErrScenarioExpired},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, memory.Limits{MaxScenarios: 1, MaxEventsPerScenario: 2})
			process(t, f, event(1, 100, event2.LoginModeDetermined, f.base), nil)
			f.clock.Set(f.base.Add(15 * time.Minute))
			e := event(2, 100, tt.kind, f.clock.Now())
			if e.AuthMode != nil {
				*e.AuthMode = tt.mode
			}
			process(t, f, e, tt.want)
			if tt.want != nil {
				if hasEvent(t, f, 2) {
					t.Fatal("отклонённый факт сохранён")
				}
			} else {
				// Смысловое подтверждение исчерпало последнее место.
				e.EventID = testID(3)
				process(t, f, e, dError.ErrCapacityExceeded)
			}
			wantUnknown := int64(1)
			if errors.Is(tt.want, dError.ErrConflict) {
				wantUnknown = 0
			}
			if got := snapshot(t, f).Completed[scenario2.OutcomeUnknown]; got != wantUnknown {
				t.Fatalf("нарушен приоритет конфликта и закрытия: %d", got)
			}
		})
	}
}

func TestProcessDefaultEventLimit(t *testing.T) {
	f := newFixture(t, memory.DefaultLimits())
	for id := 1; id <= 32; id++ {
		process(t, f, event(id, 100, event2.LoginFailed, f.base), nil)
	}
	process(t, f, event(33, 100, event2.LoginFailed, f.base), dError.ErrCapacityExceeded)
	process(t, f, event(1, 100, event2.LoginFailed, f.base), nil)
	if hasEvent(t, f, 33) || snapshot(t, f).Completed[scenario2.OutcomeTechnicalError] != 1 {
		t.Fatal("полный лимит принял новый ID или повторил завершение")
	}
}

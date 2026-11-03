package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/vendelev/sre-metrics/service/user-auth/internal/domain"
)

// BR-04, AC-17: сравнивается момент времени, первое начало неизменяемо.
func TestScenarioStartConfirmationAndConflict(t *testing.T) {
	startedAt := time.Date(2026, time.November, 2, 12, 0, 0, 0, time.UTC)
	scenario := domain.NewScenario("scenario-1")
	if err := scenario.Start(startedAt, startedAt); err != nil {
		t.Fatalf("подготовка начала: %v", err)
	}

	sameInstant := startedAt.In(time.FixedZone("UTC+3", 3*60*60))
	if err := scenario.Start(sameInstant, startedAt); err != nil {
		t.Fatalf("подтверждение начала: %v", err)
	}
	if got, ok := scenario.StartedAt(); !ok || !got.Equal(startedAt) || got.Location() != startedAt.Location() {
		t.Errorf("подтверждение заменило первоначальное начало: получено (%v, %t)", got, ok)
	}

	if err := scenario.Start(startedAt.Add(time.Second), startedAt); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("другое начало: получена ошибка %v, ожидалась ErrConflict", err)
	}
	if got, ok := scenario.StartedAt(); !ok || !got.Equal(startedAt) || got.Location() != startedAt.Location() {
		t.Errorf("конфликт заменил первоначальное начало: получено (%v, %t)", got, ok)
	}
}

// BR-05, AC-22: первый вариант сохраняется при подтверждении и конфликте.
func TestScenarioDetermineModeConfirmationAndConflict(t *testing.T) {
	now := time.Date(2026, time.November, 2, 12, 0, 0, 0, time.UTC)
	scenario := domain.NewScenario("scenario-1")
	if err := scenario.DetermineMode(domain.LoginModeCredentials, now); err != nil {
		t.Fatalf("подготовка варианта: %v", err)
	}
	if err := scenario.DetermineMode(domain.LoginModeCredentials, now); err != nil {
		t.Fatalf("подтверждение варианта: %v", err)
	}
	if got, ok := scenario.Mode(); !ok || got != domain.LoginModeCredentials {
		t.Errorf("подтверждение изменило вариант: получено (%q, %t)", got, ok)
	}

	if err := scenario.DetermineMode(domain.LoginModeExistingSession, now); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("другой вариант: получена ошибка %v, ожидалась ErrConflict", err)
	}
	if got, ok := scenario.Mode(); !ok || got != domain.LoginModeCredentials {
		t.Errorf("конфликт изменил вариант: получено (%q, %t)", got, ok)
	}
}

// BR-14, AC-06: каждый исход подтверждается, остальные исходы ему противоречат.
func TestScenarioOutcomeConfirmationsAndConflicts(t *testing.T) {
	startedAt := time.Date(2026, time.November, 2, 12, 0, 0, 0, time.UTC)
	succeededAt := startedAt.Add(4 * time.Second)
	outcomes := []struct {
		name    string
		outcome domain.Outcome
		apply   func(*domain.Scenario) error
	}{
		{
			name:    "успех",
			outcome: domain.OutcomeSuccess,
			apply: func(s *domain.Scenario) error {
				return s.Succeed(succeededAt, succeededAt)
			},
		},
		{
			name:    "техническая ошибка",
			outcome: domain.OutcomeTechnicalError,
			apply:   func(s *domain.Scenario) error { return s.Fail(succeededAt) },
		},
		{
			name:    "штатный отказ",
			outcome: domain.OutcomeRejected,
			apply:   func(s *domain.Scenario) error { return s.Reject(succeededAt) },
		},
	}

	for _, first := range outcomes {
		for _, next := range outcomes {
			t.Run(first.name+"/"+next.name, func(t *testing.T) {
				scenario := domain.NewScenario("scenario-1")
				if err := scenario.Start(startedAt, startedAt); err != nil {
					t.Fatalf("подготовка начала: %v", err)
				}
				if err := scenario.DetermineMode(domain.LoginModeCredentials, startedAt); err != nil {
					t.Fatalf("подготовка варианта: %v", err)
				}
				if err := first.apply(scenario); err != nil {
					t.Fatalf("первый исход: %v", err)
				}
				if got := scenario.Outcome(); got != first.outcome {
					t.Fatalf("первый исход: получено %q, ожидалось %q", got, first.outcome)
				}

				err := next.apply(scenario)
				if first.outcome == next.outcome {
					if err != nil {
						t.Errorf("подтверждение исхода: %v", err)
					}
				} else if !errors.Is(err, domain.ErrConflict) {
					t.Errorf("другой исход: получена ошибка %v, ожидалась ErrConflict", err)
				}

				if got := scenario.Outcome(); got != first.outcome {
					t.Errorf("исход: получено %q, ожидалось %q", got, first.outcome)
				}
				if got, ok := scenario.StartedAt(); !ok || !got.Equal(startedAt) {
					t.Errorf("начало: получено (%v, %t), ожидалось (%v, true)", got, ok, startedAt)
				}
				if got, ok := scenario.Mode(); !ok || got != domain.LoginModeCredentials {
					t.Errorf("вариант: получено (%q, %t), ожидалось (%q, true)", got, ok, domain.LoginModeCredentials)
				}
				if first.outcome == domain.OutcomeSuccess {
					if got, ok := scenario.SucceededAt(); !ok || !got.Equal(succeededAt) {
						t.Errorf("успех: получено (%v, %t), ожидалось (%v, true)", got, ok, succeededAt)
					}
					if got, ok := scenario.SuccessfulDuration(); !ok || got != 4*time.Second {
						t.Errorf("длительность: получено (%v, %t), ожидалось (4s, true)", got, ok)
					}
				} else {
					if got, ok := scenario.SucceededAt(); ok {
						t.Errorf("при ошибке или отказе время успеха недоступно: получено (%v, true)", got)
					}
					if got, ok := scenario.SuccessfulDuration(); ok {
						t.Errorf("при ошибке или отказе длительность недоступна: получено (%v, true)", got)
					}
				}
			})
		}
	}
}

func TestScenarioSucceedConfirmationPreservesTime(t *testing.T) {
	startedAt := time.Date(2026, time.November, 2, 12, 0, 0, 0, time.UTC)
	succeededAt := startedAt.Add(4 * time.Second)
	scenario := domain.NewScenario("scenario-1")
	if err := scenario.Start(startedAt, startedAt); err != nil {
		t.Fatalf("подготовка начала: %v", err)
	}
	if err := scenario.Succeed(succeededAt, succeededAt); err != nil {
		t.Fatalf("подготовка успеха: %v", err)
	}
	if err := scenario.Succeed(succeededAt.Add(time.Second), succeededAt.Add(time.Second)); err != nil {
		t.Fatalf("подтверждение успеха с другим временем: %v", err)
	}
	if got, ok := scenario.SucceededAt(); !ok || !got.Equal(succeededAt) {
		t.Errorf("успех: получено (%v, %t), ожидалось (%v, true)", got, ok, succeededAt)
	}
	if got, ok := scenario.SuccessfulDuration(); !ok || got != 4*time.Second {
		t.Errorf("длительность: получено (%v, %t), ожидалось (4s, true)", got, ok)
	}
}

// BR-05, BR-18, AC-07: вариант сообщается явно, длительность берётся из исходных времён.
func TestScenarioSuccessfulPath(t *testing.T) {
	startedAt := time.Date(2026, time.November, 2, 12, 0, 0, 0, time.UTC)
	succeededAt := startedAt.Add(4 * time.Second)
	scenario := domain.NewScenario("scenario-1")

	if err := scenario.Start(startedAt, startedAt.Add(time.Second)); err != nil {
		t.Fatalf("подготовка начала: %v", err)
	}
	if err := scenario.DetermineMode(domain.LoginModeCredentials, startedAt.Add(2*time.Second)); err != nil {
		t.Fatalf("подготовка варианта: %v", err)
	}
	if err := scenario.Succeed(succeededAt, startedAt.Add(6*time.Second)); err != nil {
		t.Fatalf("подготовка успеха: %v", err)
	}

	if got := scenario.ID(); got != "scenario-1" {
		t.Errorf("идентификатор: получено %q, ожидалось %q", got, "scenario-1")
	}
	if got, ok := scenario.StartedAt(); !ok || !got.Equal(startedAt) {
		t.Errorf("начало: получено (%v, %t), ожидалось (%v, true)", got, ok, startedAt)
	}
	if got, ok := scenario.Mode(); !ok || got != domain.LoginModeCredentials {
		t.Errorf("вариант: получено (%q, %t), ожидалось (%q, true)", got, ok, domain.LoginModeCredentials)
	}
	if got := scenario.Outcome(); got != domain.OutcomeSuccess {
		t.Errorf("исход: получено %q, ожидалось %q", got, domain.OutcomeSuccess)
	}
	if got, ok := scenario.SucceededAt(); !ok || !got.Equal(succeededAt) {
		t.Errorf("успех: получено (%v, %t), ожидалось (%v, true)", got, ok, succeededAt)
	}
	if got, ok := scenario.SuccessfulDuration(); !ok || got != 4*time.Second {
		t.Errorf("длительность: получено (%v, %t), ожидалось (4s, true)", got, ok)
	}
}

func TestScenarioSuccessfulDurationUnavailableBeforeSuccess(t *testing.T) {
	scenario := domain.NewScenario("scenario-1")
	if got, ok := scenario.SuccessfulDuration(); ok {
		t.Fatalf("до начала длительность недоступна: получено (%v, true)", got)
	}

	startedAt := time.Date(2026, time.November, 2, 12, 0, 0, 0, time.UTC)
	if err := scenario.Start(startedAt, startedAt); err != nil {
		t.Fatalf("подготовка начала: %v", err)
	}
	if got, ok := scenario.SuccessfulDuration(); ok {
		t.Fatalf("после начала без успеха длительность недоступна: получено (%v, true)", got)
	}

	if err := scenario.DetermineMode(domain.LoginModeCredentials, startedAt); err != nil {
		t.Fatalf("подготовка варианта: %v", err)
	}
	if got, ok := scenario.SuccessfulDuration(); ok {
		t.Errorf("после определения варианта без успеха длительность недоступна: получено (%v, true)", got)
	}
}

// BR-38: нулевая длительность допустима, отрицательная не отменяет успех.
func TestScenarioSuccessfulDurationBoundaries(t *testing.T) {
	tests := []struct {
		name      string
		interval  time.Duration
		expected  time.Duration
		available bool
	}{
		{
			name:      "нулевая длительность",
			interval:  0,
			expected:  0,
			available: true,
		},
		{
			name:      "отрицательная длительность",
			interval:  -2 * time.Second,
			expected:  0,
			available: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			startedAt := time.Date(2026, time.November, 2, 12, 0, 5, 0, time.UTC)
			succeededAt := startedAt.Add(tt.interval)
			scenario := domain.NewScenario("scenario-1")
			if err := scenario.Start(startedAt, startedAt); err != nil {
				t.Fatalf("подготовка начала: %v", err)
			}
			if err := scenario.DetermineMode(domain.LoginModeCredentials, startedAt); err != nil {
				t.Fatalf("подготовка варианта: %v", err)
			}
			if err := scenario.Succeed(succeededAt, startedAt); err != nil {
				t.Fatalf("подготовка успеха: %v", err)
			}

			if got, ok := scenario.SuccessfulDuration(); ok != tt.available || got != tt.expected {
				t.Errorf("длительность: получено (%v, %t), ожидалось (%v, %t)", got, ok, tt.expected, tt.available)
			}
			if got := scenario.Outcome(); got != domain.OutcomeSuccess {
				t.Errorf("исход: получено %q, ожидалось %q", got, domain.OutcomeSuccess)
			}
			if got, ok := scenario.SucceededAt(); !ok || !got.Equal(succeededAt) {
				t.Errorf("успех: получено (%v, %t), ожидалось (%v, true)", got, ok, succeededAt)
			}
		})
	}
}

// BR-12, BR-33: до границы ожидание активно, равенство уже закрывает его.
func TestScenarioWaitingBoundaries(t *testing.T) {
	base := scenarioTime()
	for _, withStart := range []bool{false, true} {
		for _, offset := range []time.Duration{-time.Millisecond, 0, time.Millisecond} {
			name := "без начала/" + offset.String()
			if withStart {
				name = "с началом/" + offset.String()
			}
			t.Run(name, func(t *testing.T) {
				s := domain.NewScenario("scenario-1")
				var deadline time.Time
				if withStart {
					mustAccept(t, s.Start(base.Add(-time.Minute), base))
					deadline = base.Add(14 * time.Minute)
				} else {
					mustAccept(t, s.DetermineMode(domain.LoginModeCredentials, base))
					deadline = base.Add(15 * time.Minute)
				}
				assertMoment(t, "первое получение", s.FirstReceivedAt, base)
				assertMoment(t, "deadline", s.Deadline, deadline)
				now := deadline.Add(offset)
				wantExpired := offset >= 0
				if got := s.WaitingExpired(now); got != wantExpired {
					t.Errorf("истечение ожидания: %t, ожидалось %t", got, wantExpired)
				}
				if got := s.CloseIfExpired(now); got != wantExpired {
					t.Fatalf("закрытие: %t, ожидалось %t", got, wantExpired)
				}
				if !wantExpired {
					if s.Outcome() != "" {
						t.Fatal("до deadline исхода нет")
					}
					return
				}
				if s.Outcome() != domain.OutcomeUnknown {
					t.Fatal("ожидался unknown")
				}
				assertMoment(t, "закрытие", s.ClosedAt, now)
				if s.CloseIfExpired(now.Add(time.Minute)) {
					t.Error("повторное закрытие не является переходом")
				}
				assertMoment(t, "прежнее закрытие", s.ClosedAt, now)
			})
		}
	}
}

func TestScenarioEmptyLifecycle(t *testing.T) {
	s := domain.NewScenario("scenario-1")
	getters := []struct {
		name string
		get  func() (time.Time, bool)
	}{
		{name: "первое получение", get: s.FirstReceivedAt},
		{name: "deadline", get: s.Deadline},
		{name: "закрытие", get: s.ClosedAt},
		{name: "retention", get: s.RetentionDeadline},
	}
	for _, tt := range getters {
		if got, ok := tt.get(); ok || !got.IsZero() {
			t.Errorf("%s пустой сущности: (%v, %t)", tt.name, got, ok)
		}
	}
	if s.CloseIfExpired(scenarioTime()) || s.RetentionExpired(scenarioTime()) {
		t.Error("пустая сущность не имеет срока ожидания или хранения")
	}
}

// BR-33, BR-52: начало заменяет срок, но не спасает уже истёкшее прежнее ожидание.
func TestScenarioLateStartDeadline(t *testing.T) {
	base := scenarioTime()
	tests := []struct {
		name        string
		startedAt   time.Time
		receivedAt  time.Time
		wantOutcome domain.Outcome
		wantError   error
	}{
		{
			name:      "раннее начало сокращает срок",
			startedAt: base.Add(-5 * time.Minute), receivedAt: base.Add(time.Minute),
		},
		{
			name:      "позднее начало заменяет срок более поздним",
			startedAt: base.Add(5 * time.Minute), receivedAt: base.Add(5 * time.Minute),
		},
		{
			name:      "новый deadline ровно сейчас",
			startedAt: base.Add(-10 * time.Minute), receivedAt: base.Add(5 * time.Minute),
			wantOutcome: domain.OutcomeUnknown,
		},
		{
			name:      "новый deadline уже прошёл",
			startedAt: base.Add(-20 * time.Minute), receivedAt: base.Add(time.Minute),
			wantOutcome: domain.OutcomeUnknown,
		},
		{
			name:      "прежний срок наступил",
			startedAt: base.Add(14 * time.Minute), receivedAt: base.Add(15 * time.Minute),
			wantOutcome: domain.OutcomeUnknown, wantError: domain.ErrScenarioExpired,
		},
		{
			name:      "прежний срок прошёл",
			startedAt: base.Add(14 * time.Minute), receivedAt: base.Add(16 * time.Minute),
			wantOutcome: domain.OutcomeUnknown, wantError: domain.ErrScenarioExpired,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := domain.NewScenario("scenario-1")
			mustAccept(t, s.DetermineMode(domain.LoginModeCredentials, base))
			mustAccept(t, s.DetermineMode(domain.LoginModeCredentials, base.Add(30*time.Second)))
			assertMoment(t, "срок после подтверждения", s.Deadline, base.Add(15*time.Minute))
			err := s.Start(tt.startedAt, tt.receivedAt)
			if !errors.Is(err, tt.wantError) {
				t.Fatalf("начало: %v, ожидалось %v", err, tt.wantError)
			}
			if s.Outcome() != tt.wantOutcome {
				t.Errorf("исход: %q, ожидалось %q", s.Outcome(), tt.wantOutcome)
			}
			assertMoment(t, "первое получение не заменено", s.FirstReceivedAt, base)
			if tt.wantError != nil {
				if _, ok := s.StartedAt(); ok {
					t.Error("отклонённое начало не должно сохраниться")
				}
				assertMoment(t, "старый срок", s.Deadline, base.Add(15*time.Minute))
			} else {
				assertMoment(t, "новый срок", s.Deadline, tt.startedAt.Add(15*time.Minute))
				mustAccept(t, s.Start(tt.startedAt, tt.receivedAt.Add(time.Second)))
				assertMoment(t, "неизменный срок", s.Deadline, tt.startedAt.Add(15*time.Minute))
			}
			if tt.wantOutcome == domain.OutcomeUnknown {
				assertMoment(t, "закрытие", s.ClosedAt, tt.receivedAt)
			}
		})
	}
	for _, age := range []time.Duration{15 * time.Minute, 20 * time.Minute} {
		t.Run("первое начало/"+age.String(), func(t *testing.T) {
			s := domain.NewScenario("scenario-1")
			mustAccept(t, s.Start(base.Add(-age), base))
			if s.Outcome() != domain.OutcomeUnknown {
				t.Fatal("просроченное первое начало должно сразу дать unknown")
			}
			assertMoment(t, "закрытие", s.ClosedAt, base)
			assertMoment(t, "retention", s.RetentionDeadline, base.Add(15*time.Minute))
		})
	}
}

// BR-28, BR-29, BR-33: исходы приложения дополняются, закрытие не переносится.
func TestScenarioApplicationOutcomeLateFactsAndRetention(t *testing.T) {
	base := scenarioTime()
	for _, outcome := range []domain.Outcome{
		domain.OutcomeSuccess, domain.OutcomeTechnicalError, domain.OutcomeRejected,
	} {
		t.Run(string(outcome), func(t *testing.T) {
			s := domain.NewScenario("scenario-1")
			mustAccept(t, applyOutcome(s, outcome, base.Add(-time.Minute), base))
			assertMoment(t, "первое получение", s.FirstReceivedAt, base)
			assertMoment(t, "закрытие без начала", s.ClosedAt, base)
			late := base.Add(14 * time.Minute)
			mustAccept(t, s.Start(base.Add(-20*time.Minute), late))
			mustAccept(t, s.DetermineMode(domain.LoginModeExistingSession, late))
			mustAccept(t, applyOutcome(s, outcome, late, late))
			if s.Outcome() != outcome || s.CloseIfExpired(late) {
				t.Fatal("позднее начало не может заменить исход приложения")
			}
			assertMoment(t, "закрытие после дополнения", s.ClosedAt, base)
			assertMoment(t, "retention после дополнения", s.RetentionDeadline, base.Add(15*time.Minute))
			if outcome == domain.OutcomeSuccess {
				assertMoment(t, "первый успех", s.SucceededAt, base.Add(-time.Minute))
			}
			for _, offset := range []time.Duration{-time.Millisecond, 0, time.Millisecond} {
				now := base.Add(15*time.Minute + offset)
				if got := s.RetentionExpired(now); got != (offset >= 0) {
					t.Errorf("retention при %s: %t", offset, got)
				}
			}
		})
	}
}

func TestScenarioUnknownFacts(t *testing.T) {
	base := scenarioTime()
	type factCase struct {
		name      string
		withStart bool
		withMode  bool
		apply     func(*domain.Scenario, time.Time) error
		wantError error
	}
	tests := []factCase{
		{name: "подтверждение начала", withStart: true,
			apply: func(s *domain.Scenario, now time.Time) error { return s.Start(base, now) }},
		{name: "противоречие началу", withStart: true, wantError: domain.ErrConflict,
			apply: func(s *domain.Scenario, now time.Time) error { return s.Start(base.Add(time.Second), now) }},
		{name: "недостающее начало", withMode: true, wantError: domain.ErrScenarioExpired,
			apply: func(s *domain.Scenario, now time.Time) error { return s.Start(base, now) }},
		{name: "подтверждение варианта", withMode: true,
			apply: func(s *domain.Scenario, now time.Time) error {
				return s.DetermineMode(domain.LoginModeCredentials, now)
			}},
		{name: "противоречие варианту", withMode: true, wantError: domain.ErrConflict,
			apply: func(s *domain.Scenario, now time.Time) error {
				return s.DetermineMode(domain.LoginModeExistingSession, now)
			}},
		{name: "недостающий вариант", withStart: true, wantError: domain.ErrScenarioExpired,
			apply: func(s *domain.Scenario, now time.Time) error {
				return s.DetermineMode(domain.LoginModeCredentials, now)
			}},
	}
	for _, outcome := range []domain.Outcome{
		domain.OutcomeSuccess, domain.OutcomeTechnicalError, domain.OutcomeRejected,
	} {
		tests = append(tests, factCase{
			name: string(outcome), withStart: true, wantError: domain.ErrScenarioExpired,
			apply: func(s *domain.Scenario, now time.Time) error {
				return applyOutcome(s, outcome, base.Add(time.Second), now)
			},
		})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := domain.NewScenario("scenario-1")
			if tt.withStart {
				mustAccept(t, s.Start(base, base))
			}
			if tt.withMode {
				mustAccept(t, s.DetermineMode(domain.LoginModeCredentials, base))
			}
			closedAt := base.Add(16 * time.Minute)
			s.CloseIfExpired(closedAt)
			before := *s
			if err := tt.apply(s, closedAt.Add(time.Second)); !errors.Is(err, tt.wantError) {
				t.Errorf("факт после unknown: %v, ожидалось %v", err, tt.wantError)
			}
			if *s != before {
				t.Error("подтверждение или отказ после unknown изменили состояние")
			}
			assertMoment(t, "retention от фактического закрытия", s.RetentionDeadline, closedAt.Add(15*time.Minute))
			for _, offset := range []time.Duration{-time.Millisecond, 0, time.Millisecond} {
				if got := s.RetentionExpired(closedAt.Add(15*time.Minute + offset)); got != (offset >= 0) {
					t.Errorf("retention unknown при %s: %t", offset, got)
				}
			}
		})
	}
}

// BR-50, BR-55: чистая проверка, самостоятельное закрытие и приём не слиты.
func TestScenarioChecksAndIndependentExpiry(t *testing.T) {
	base := scenarioTime()
	s := domain.NewScenario("scenario-1")
	mustAccept(t, s.Start(base, base))
	mustAccept(t, s.DetermineMode(domain.LoginModeCredentials, base))
	now := base.Add(15 * time.Minute)
	before := *s
	checks := []struct {
		name      string
		check     func() error
		wantError error
	}{
		{name: "конфликт начала", wantError: domain.ErrConflict,
			check: func() error { return s.CheckStart(base.Add(time.Second), now) }},
		{name: "подтверждение начала", check: func() error { return s.CheckStart(base, now) }},
		{name: "конфликт варианта", wantError: domain.ErrConflict,
			check: func() error { return s.CheckMode(domain.LoginModeExistingSession, now) }},
		{name: "подтверждение варианта", check: func() error { return s.CheckMode(domain.LoginModeCredentials, now) }},
		{name: "исход после deadline", wantError: domain.ErrScenarioExpired,
			check: func() error { return s.CheckOutcome(domain.OutcomeSuccess, now) }},
	}
	for _, tt := range checks {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.check(); !errors.Is(err, tt.wantError) {
				t.Errorf("проверка: %v, ожидалось %v", err, tt.wantError)
			}
			if *s != before {
				t.Fatal("чистая проверка изменила сценарий")
			}
		})
	}
	if err := s.Start(base.Add(time.Second), now); !errors.Is(err, domain.ErrConflict) || *s != before {
		t.Fatal("конфликт должен иметь приоритет перед закрытием")
	}
	if err := s.DetermineMode(
		domain.LoginModeExistingSession,
		now,
	); !errors.Is(err, domain.ErrConflict) ||
		*s != before {
		t.Fatal("конфликт варианта должен иметь приоритет перед закрытием")
	}
	if !s.CloseIfExpired(now) {
		t.Fatal("самостоятельное закрытие должно быть доступно до проверки вместимости")
	}
	// Входящий факт не принимается: будущая application может отказать по вместимости.
	if got := s.TakeMeasurements().Completion; got != domain.OutcomeUnknown {
		t.Errorf("самостоятельное закрытие без нового факта: %q", got)
	}
	assertMoment(t, "закрытие", s.ClosedAt, now)
}

func TestScenarioFinishAtDeadline(t *testing.T) {
	base := scenarioTime()
	for _, outcome := range []domain.Outcome{
		domain.OutcomeSuccess, domain.OutcomeTechnicalError, domain.OutcomeRejected,
	} {
		for _, offset := range []time.Duration{-time.Millisecond, 0, time.Millisecond} {
			t.Run(string(outcome)+"/"+offset.String(), func(t *testing.T) {
				s := domain.NewScenario("scenario-1")
				mustAccept(t, s.Start(base, base))
				now := base.Add(15*time.Minute + offset)
				err := applyOutcome(s, outcome, base.Add(time.Second), now)
				wantOutcome := outcome
				var wantError error
				if offset >= 0 {
					wantOutcome = domain.OutcomeUnknown
					wantError = domain.ErrScenarioExpired
				}
				if !errors.Is(err, wantError) || s.Outcome() != wantOutcome {
					t.Fatalf("завершение: (%q, %v), ожидалось (%q, %v)", s.Outcome(), err, wantOutcome, wantError)
				}
				assertMoment(t, "закрытие Go", s.ClosedAt, now)
				if offset >= 0 {
					if _, ok := s.SucceededAt(); ok {
						t.Error("отклонённый успех не сохраняется")
					}
				}
			})
		}
	}
}

func scenarioTime() time.Time {
	return time.Date(2026, time.November, 2, 12, 0, 0, 0, time.UTC)
}

func mustAccept(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("приём факта: %v", err)
	}
}

func assertMoment(t *testing.T, name string, get func() (time.Time, bool), expected time.Time) {
	t.Helper()
	if got, ok := get(); !ok || !got.Equal(expected) {
		t.Errorf("%s: (%v, %t), ожидалось (%v, true)", name, got, ok, expected)
	}
}

func applyOutcome(s *domain.Scenario, outcome domain.Outcome, occurredAt, now time.Time) error {
	switch outcome {
	case domain.OutcomeSuccess:
		return s.Succeed(occurredAt, now)
	case domain.OutcomeTechnicalError:
		return s.Fail(now)
	case domain.OutcomeRejected:
		return s.Reject(now)
	default:
		panic("неподдерживаемый исход в тесте")
	}
}

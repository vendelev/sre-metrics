package domain_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vendelev/sre-metrics/user-login/internal/domain"
)

// BR-25–BR-28, BR-34, AC-20–AC-21: измерение доступно, когда известны все нужные факты.
func TestScenarioMeasurementsFactOrders(t *testing.T) {
	orders := []string{"smf", "sfm", "msf", "mfs", "fsm", "fms"}
	base := scenarioTime()
	for _, outcome := range []domain.Outcome{
		domain.OutcomeSuccess, domain.OutcomeTechnicalError, domain.OutcomeRejected,
	} {
		for _, mode := range []domain.OAuthMode{domain.LoginModeCredentials, domain.LoginModeExistingSession} {
			for _, order := range orders {
				t.Run(string(outcome)+"/"+string(mode)+"/"+order, func(t *testing.T) {
					s := domain.NewScenario(uuid.New())
					if got := s.TakeMeasurements(); got != (domain.Measurements{}) {
						t.Fatal("пустой сценарий не даёт измерений")
					}
					var hasStart, hasMode, hasFinish bool
					var completionCount, errorCount, durationCount int
					for i, fact := range order {
						now := base.Add(time.Duration(i+10) * time.Second)
						switch fact {
						case 's':
							mustAccept(t, s.Start(base, now))
							hasStart = true
						case 'm':
							mustAccept(t, s.DetermineMode(mode, now))
							hasMode = true
						case 'f':
							mustAccept(t, applyOutcome(s, outcome, base.Add(4*time.Second), now))
							hasFinish = true
						}
						got := s.TakeMeasurements()
						wantCompletion := hasFinish && completionCount == 0
						if (got.Completion != "") != wantCompletion {
							t.Fatalf("завершение на шаге %c: %+v", fact, got)
						}
						if got.Completion != "" {
							completionCount++
							if got.Completion != outcome {
								t.Errorf("измерение другого исхода: %q", got.Completion)
							}
						}
						wantError := hasFinish && hasMode && outcome == domain.OutcomeTechnicalError && errorCount == 0
						if (got.TechnicalErrorMode != "") != wantError {
							t.Fatalf("техническая ошибка на шаге %c: %+v", fact, got)
						}
						if got.TechnicalErrorMode != "" {
							errorCount++
							if got.TechnicalErrorMode != mode {
								t.Errorf("вариант ошибки: %q", got.TechnicalErrorMode)
							}
						}
						wantDuration := hasStart && hasMode && hasFinish && outcome == domain.OutcomeSuccess &&
							durationCount == 0
						if (got.SuccessfulDuration != nil) != wantDuration {
							t.Fatalf("длительность на шаге %c: %+v", fact, got)
						}
						if got.SuccessfulDuration != nil {
							durationCount++
							want := domain.DurationObservation{Mode: mode, Duration: 4 * time.Second}
							if *got.SuccessfulDuration != want {
								t.Errorf("наблюдение: %+v, ожидалось %+v", got.SuccessfulDuration, want)
							}
							// Возвращённое значение не должно открывать доступ к состоянию сущности.
							got.SuccessfulDuration.Duration = time.Hour
						}
						if next := s.TakeMeasurements(); next != (domain.Measurements{}) {
							t.Fatalf("повторные измерения: %+v", next)
						}
					}
					mustAccept(t, s.Start(base, base.Add(time.Minute)))
					mustAccept(t, s.DetermineMode(mode, base.Add(time.Minute)))
					mustAccept(t, applyOutcome(s, outcome, base.Add(time.Minute), base.Add(time.Minute)))
					if got := s.TakeMeasurements(); got != (domain.Measurements{}) {
						t.Errorf("подтверждения повторили измерения: %+v", got)
					}
					if completionCount != 1 {
						t.Errorf("число завершений: %d", completionCount)
					}
				})
			}
		}
	}
}

// BR-38, AC-27: дефект не зависит от порядка доставки, ноль — настоящее наблюдение.
func TestScenarioDurationMeasurementBoundaries(t *testing.T) {
	base := scenarioTime()
	for _, interval := range []time.Duration{-time.Millisecond, 0} {
		for _, order := range []string{"smf", "sfm", "msf", "mfs", "fsm", "fms"} {
			t.Run(interval.String()+"/"+order, func(t *testing.T) {
				s := domain.NewScenario(uuid.New())
				if s.HasNegativeDuration() {
					t.Fatal("без фактов нет отрицательной длительности")
				}
				var completions, observations int
				for _, fact := range order {
					switch fact {
					case 's':
						mustAccept(t, s.Start(base, base.Add(time.Second)))
					case 'm':
						mustAccept(t, s.DetermineMode(domain.LoginModeCredentials, base.Add(time.Second)))
					case 'f':
						mustAccept(t, s.Succeed(base.Add(interval), base.Add(time.Second)))
					}
					got := s.TakeMeasurements()
					if got.Completion != "" {
						completions++
					}
					if got.SuccessfulDuration != nil {
						observations++
						if got.SuccessfulDuration.Duration != 0 {
							t.Errorf("ожидалась нулевая длительность: %+v", got.SuccessfulDuration)
						}
					}
				}
				wantObservations := 1
				if interval < 0 {
					wantObservations = 0
				}
				if completions != 1 || observations != wantObservations || s.Outcome() != domain.OutcomeSuccess {
					t.Errorf("итог: завершений=%d, наблюдений=%d, исход=%q", completions, observations, s.Outcome())
				}
				if got := s.HasNegativeDuration(); got != (interval < 0) {
					t.Errorf("признак отрицательной длительности: %t", got)
				}
				if got := s.TakeMeasurements(); got != (domain.Measurements{}) {
					t.Errorf("повторные измерения: %+v", got)
				}
			})
		}
	}
}

func TestScenarioUnknownMeasurementOnce(t *testing.T) {
	base := scenarioTime()
	s := domain.NewScenario(uuid.New())
	mustAccept(t, s.Start(base, base))
	mustAccept(t, s.DetermineMode(domain.LoginModeCredentials, base))
	now := base.Add(15 * time.Minute)
	// Даже подтверждение при обработке нового ID закрывает прежнее ожидание.
	mustAccept(t, s.DetermineMode(domain.LoginModeCredentials, now))
	if got := s.TakeMeasurements(); got != (domain.Measurements{Completion: domain.OutcomeUnknown}) {
		t.Fatalf("измерения unknown: %+v", got)
	}
	mustAccept(t, s.Start(base, now.Add(time.Second)))
	s.CloseIfExpired(now.Add(time.Second))
	if got := s.TakeMeasurements(); got != (domain.Measurements{}) {
		t.Errorf("unknown учтён повторно: %+v", got)
	}
}

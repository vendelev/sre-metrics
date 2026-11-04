package memory_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/vendelev/sre-metrics/service/user-login/internal/domain"
	"github.com/vendelev/sre-metrics/service/user-login/internal/infrastructure/memory"
	"github.com/vendelev/sre-metrics/service/user-login/internal/infrastructure/otel/oteltest"
)

func TestSDKBaselineAndIndependence(t *testing.T) {
	f := newFixture(t, memory.DefaultLimits())
	a := snapshot(t, f)
	if len(a.Completed) != 4 || len(a.TechnicalErrors) != 2 || len(a.Durations) != 0 {
		t.Fatalf("не все согласованные комбинации доступны: %+v", a)
	}
	for _, value := range a.Completed {
		if value != 0 {
			t.Fatal("baseline создал завершение")
		}
	}
	for _, value := range a.TechnicalErrors {
		if value != 0 {
			t.Fatal("baseline создал ошибку")
		}
	}
	for _, histogram := range a.Durations {
		if histogram != (oteltest.Duration{}) {
			t.Fatal("baseline создал фиктивное наблюдение")
		}
	}
	before := snapshot(t, f)
	a.Completed[domain.OutcomeSuccess] = 999
	a.TechnicalErrors[domain.LoginModeCredentials] = 999
	a.Durations[domain.LoginModeCredentials] = oteltest.Duration{Count: 999}
	delete(a.Completed, domain.OutcomeUnknown)
	if !reflect.DeepEqual(before, snapshot(t, f)) {
		t.Fatal("снимок открыл доступ к внутренним значениям")
	}
	if scenario(t, f, 100) != nil {
		t.Fatal("снимок создал запись")
	}
}

// BR-34: поздний факт добавляет только недостающее измерение.
func TestProcessLateMeasurementsOnce(t *testing.T) {
	for _, kind := range []domain.EventType{domain.EventTypeLoginSucceeded, domain.EventTypeLoginFailed} {
		t.Run(string(kind), func(t *testing.T) {
			f := newFixture(t, memory.DefaultLimits())
			finish := event(1, 100, kind, f.base)
			process(t, f, finish, nil)
			a := snapshot(t, f)
			if a.Durations[domain.LoginModeCredentials].Count != 0 ||
				a.TechnicalErrors[domain.LoginModeCredentials] != 0 {
				t.Fatal("частичные данные угадали вариант или начало")
			}
			process(t, f, event(2, 100, domain.EventTypeLoginModeDetermined, f.base), nil)
			classified := snapshot(t, f)
			if kind == domain.EventTypeLoginFailed && classified.TechnicalErrors[domain.LoginModeCredentials] != 1 {
				t.Fatal("классифицированная ошибка не должна требовать начала")
			}
			if classified.Durations[domain.LoginModeCredentials].Count != 0 {
				t.Fatal("известного варианта недостаточно для длительности без начала")
			}
			process(t, f, event(3, 100, domain.EventTypeLoginStarted, f.base.Add(-4*time.Second)), nil)
			process(t, f, finish, nil)
			process(t, f, event(4, 100, kind, f.base.Add(time.Second)), nil)
			process(t, f, event(5, 100, domain.EventTypeLoginModeDetermined, f.base), nil)
			a = snapshot(t, f)
			if kind == domain.EventTypeLoginSucceeded {
				if a.Completed[domain.OutcomeSuccess] != 1 {
					t.Fatal("успех посчитан не один раз")
				}
				want := oteltest.Duration{BucketCounts: [4]uint64{1, 0, 0, 0}, Count: 1, SumSeconds: 4}
				if a.Durations[domain.LoginModeCredentials] != want {
					t.Fatalf("длительность заменена или повторена: %+v", a)
				}
			} else if a.Completed[domain.OutcomeTechnicalError] != 1 || a.TechnicalErrors[domain.LoginModeCredentials] != 1 {
				t.Fatalf("поздний вариант ошибки повторно увеличил счётчик: %+v", a)
			}
			closedAt, _ := scenario(t, f, 100).ClosedAt()
			if !closedAt.Equal(f.base) {
				t.Fatal("дополнение перенесло время закрытия")
			}
			f.clock.Set(f.base.Add(15 * time.Minute))
			maintain(t, f)
			if !reflect.DeepEqual(a, snapshot(t, f)) {
				t.Fatal("удаление сценария изменило агрегаты")
			}
		})
	}
}

// BR-38: диагностика по переходу признака, вне критического участка и без события.
func TestProcessNegativeDurationDiagnostics(t *testing.T) {
	for _, order := range []string{"начало первым", "успех первым"} {
		t.Run(order, func(t *testing.T) {
			f := newFixture(t, memory.DefaultLimits())
			started := event(1, 100, domain.EventTypeLoginStarted, f.base)
			finished := event(2, 100, domain.EventTypeLoginSucceeded, f.base.Add(-time.Second))
			first, second := started, finished
			if order == "успех первым" {
				first, second = finished, started
			}
			if got := process(t, f, first, nil); got.NegativeDurationScenarioID != "" {
				t.Fatal("недостаток фактов не является отрицательной длительностью")
			}
			got := process(t, f, second, nil)
			if got.NegativeDurationScenarioID != uuid(100) {
				t.Fatalf("отсутствует безопасная диагностика: %+v", got)
			}
			// Диагностика возвращена наружу после освобождения блокировки, снимок доступен.
			if snapshot(t, f).Completed[domain.OutcomeSuccess] != 1 {
				t.Fatal("диагностика изменила успех")
			}
			process(t, f, event(3, 100, domain.EventTypeLoginModeDetermined, f.base), nil)
			if result := process(t, f, second, nil); result.NegativeDurationScenarioID != "" {
				t.Fatal("повтор повторил диагностику")
			}
			if result := process(
				t,
				f,
				event(4, 100, domain.EventTypeLoginSucceeded, f.base),
				nil,
			); result.NegativeDurationScenarioID != "" {
				t.Fatal("смысловое подтверждение повторило диагностику")
			}
			if snapshot(t, f).Durations[domain.LoginModeCredentials].Count != 0 {
				t.Fatal("отрицательная длительность превратилась в наблюдение")
			}
		})
	}
}

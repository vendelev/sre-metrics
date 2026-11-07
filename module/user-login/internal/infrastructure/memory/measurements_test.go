package memory_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	event2 "github.com/vendelev/sre-metrics/user-login/internal/domain/event"
	scenario2 "github.com/vendelev/sre-metrics/user-login/internal/domain/scenario"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/memory"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/otel/oteltest"
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
	a.Completed[scenario2.OutcomeSuccess] = 999
	a.TechnicalErrors[event2.LoginModeCredentials] = 999
	a.Durations[event2.LoginModeCredentials] = oteltest.Duration{Count: 999}
	delete(a.Completed, scenario2.OutcomeUnknown)
	if !reflect.DeepEqual(before, snapshot(t, f)) {
		t.Fatal("снимок открыл доступ к внутренним значениям")
	}
	if scenario(t, f, 100) != nil {
		t.Fatal("снимок создал запись")
	}
}

// BR-34: поздний факт добавляет только недостающее измерение.
func TestProcessLateMeasurementsOnce(t *testing.T) {
	for _, kind := range []event2.Type{event2.LoginSucceeded, event2.LoginFailed} {
		t.Run(string(kind), func(t *testing.T) {
			f := newFixture(t, memory.DefaultLimits())
			finish := event(1, 100, kind, f.base)
			process(t, f, finish, nil)
			a := snapshot(t, f)
			if a.Durations[event2.LoginModeCredentials].Count != 0 ||
				a.TechnicalErrors[event2.LoginModeCredentials] != 0 {
				t.Fatal("частичные данные угадали вариант или начало")
			}
			process(t, f, event(2, 100, event2.LoginModeDetermined, f.base), nil)
			classified := snapshot(t, f)
			if kind == event2.LoginFailed && classified.TechnicalErrors[event2.LoginModeCredentials] != 1 {
				t.Fatal("классифицированная ошибка не должна требовать начала")
			}
			if classified.Durations[event2.LoginModeCredentials].Count != 0 {
				t.Fatal("известного варианта недостаточно для длительности без начала")
			}
			process(t, f, event(3, 100, event2.LoginStarted, f.base.Add(-4*time.Second)), nil)
			process(t, f, finish, nil)
			process(t, f, event(4, 100, kind, f.base.Add(time.Second)), nil)
			process(t, f, event(5, 100, event2.LoginModeDetermined, f.base), nil)
			a = snapshot(t, f)
			if kind == event2.LoginSucceeded {
				if a.Completed[scenario2.OutcomeSuccess] != 1 {
					t.Fatal("успех посчитан не один раз")
				}
				want := oteltest.Duration{BucketCounts: [4]uint64{1, 0, 0, 0}, Count: 1, SumSeconds: 4}
				if a.Durations[event2.LoginModeCredentials] != want {
					t.Fatalf("длительность заменена или повторена: %+v", a)
				}
			} else if a.Completed[scenario2.OutcomeTechnicalError] != 1 || a.TechnicalErrors[event2.LoginModeCredentials] != 1 {
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
			started := event(1, 100, event2.LoginStarted, f.base)
			finished := event(2, 100, event2.LoginSucceeded, f.base.Add(-time.Second))
			first, second := started, finished
			if order == "успех первым" {
				first, second = finished, started
			}
			if got := process(t, f, first, nil); got.NegativeDurationScenarioID != uuid.Nil {
				t.Fatal("недостаток фактов не является отрицательной длительностью")
			}
			got := process(t, f, second, nil)
			if got.NegativeDurationScenarioID != testID(100) {
				t.Fatalf("отсутствует безопасная диагностика: %+v", got)
			}
			// Диагностика возвращена наружу после освобождения блокировки, снимок доступен.
			if snapshot(t, f).Completed[scenario2.OutcomeSuccess] != 1 {
				t.Fatal("диагностика изменила успех")
			}
			process(t, f, event(3, 100, event2.LoginModeDetermined, f.base), nil)
			if result := process(t, f, second, nil); result.NegativeDurationScenarioID != uuid.Nil {
				t.Fatal("повтор повторил диагностику")
			}
			if result := process(
				t,
				f,
				event(4, 100, event2.LoginSucceeded, f.base),
				nil,
			); result.NegativeDurationScenarioID != uuid.Nil {
				t.Fatal("смысловое подтверждение повторило диагностику")
			}
			if snapshot(t, f).Durations[event2.LoginModeCredentials].Count != 0 {
				t.Fatal("отрицательная длительность превратилась в наблюдение")
			}
		})
	}
}

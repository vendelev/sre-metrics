package memory_test

import (
	"testing"
	"time"

	"github.com/vendelev/sre-metrics/user-login/internal/application"
	event2 "github.com/vendelev/sre-metrics/user-login/internal/domain/event"
	scenario2 "github.com/vendelev/sre-metrics/user-login/internal/domain/scenario"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/memory"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// BR-55, BR-56: обе последовательности событий и обслуживания дают один результат.
// Конкурентные отправители проверяются через очередь и единственного воркера.
func TestProcessMaintenanceOrder(t *testing.T) {
	for _, maintenanceFirst := range []bool{false, true} {
		name := "сначала событие"
		if maintenanceFirst {
			name = "сначала обслуживание"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, memory.Limits{MaxScenarios: 1, MaxEventsPerScenario: 2})
			process(t, f, event(1, 100, event2.LoginStarted, f.base), nil)
			process(t, f, event(2, 100, event2.LoginModeDetermined, f.base), nil)
			f.clock.Set(f.base.Add(15 * time.Minute))
			if maintenanceFirst {
				maintain(t, f)
			}
			process(t, f, event(3, 100, event2.LoginSucceeded, f.clock.Now()), scenario2.ErrScenarioExpired)
			maintain(t, f)
			if snapshot(t, f).Completed[scenario2.OutcomeUnknown] != 1 || hasEvent(t, f, 3) {
				t.Fatal("порядок обслуживания изменил однократное закрытие или сохранил отклонённый ID")
			}
			f.clock.Set(f.base.Add(30 * time.Minute))
			if maintenanceFirst {
				maintain(t, f)
			}
			process(t, f, event(1, 101, event2.LoginSucceeded, f.clock.Now()), nil)
			maintain(t, f)
			if scenario(t, f, 100) != nil || scenario(t, f, 101) == nil ||
				snapshot(t, f).Completed[scenario2.OutcomeUnknown] != 1 {
				t.Fatal("порядок очистки изменил запись, глобальный ID или накопление")
			}
		})
	}
}

// SDK собирается независимо от состояния сценариев и допускает конкурентную регистрацию.
func TestSDKCollectConcurrentCommit(t *testing.T) {
	f := newFixture(t, memory.DefaultLimits())
	process(t, f, event(1, 100, event2.LoginStarted, f.base), nil)
	process(t, f, event(2, 100, event2.LoginModeDetermined, f.base), nil)
	start := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		<-start
		var data metricdata.ResourceMetrics
		// Не требуем согласованного снимка разных инструментов во время Commit.
		done <- f.recorder.Reader().Collect(t.Context(), &data)
	}()
	close(start)
	process(t, f, event(3, 100, event2.LoginSucceeded, f.base), nil)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("сбор SDK не завершился")
	}
	a := snapshot(t, f)
	if a.Completed[scenario2.OutcomeSuccess] != 1 || a.Durations[event2.LoginModeCredentials].Count != 1 {
		t.Fatalf("сбор после завершения потерял измерения: %+v", a)
	}
}

type countedClock struct {
	clock *controlledClock
	calls int
}

func (c *countedClock) Now() time.Time {
	c.calls++
	return c.clock.Now()
}

func TestOperationUsesSingleCurrentTime(t *testing.T) {
	f := newFixture(t, memory.DefaultLimits())
	process(t, f, event(1, 100, event2.LoginStarted, f.base), nil)
	c := &countedClock{clock: f.clock}
	observer, err := application.NewLoginObserver(f.store, c)
	if err != nil {
		t.Fatal(err)
	}
	f.observer = observer
	f.clock.Set(f.base.Add(15 * time.Minute))
	process(t, f, event(2, 100, event2.LoginSucceeded, f.base), scenario2.ErrScenarioExpired)
	if c.calls != 1 || snapshot(t, f).Completed[scenario2.OutcomeUnknown] != 1 {
		t.Fatal("операция должна использовать текущее время один раз")
	}
	_ = snapshot(t, f)
	if c.calls != 1 {
		t.Fatal("сбор SDK не должен получать время обработки или выполнять обслуживание")
	}
	f.clock.Set(f.base.Add(30 * time.Minute))
	maintain(t, f)
	if c.calls != 2 || scenario(t, f, 100) != nil {
		t.Fatal("обслуживание должно использовать одно текущее время для очистки")
	}
}

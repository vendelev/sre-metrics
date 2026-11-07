package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vendelev/sre-metrics/user-login/internal/application"
	"github.com/vendelev/sre-metrics/user-login/internal/domain/contract"
	event2 "github.com/vendelev/sre-metrics/user-login/internal/domain/event"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/memory"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/otel"
)

// BenchmarkProcess измеряет обработку с настоящими memory и локальным OTel SDK, без HTTP и сети.
func BenchmarkProcess(b *testing.B) {
	for _, kind := range []event2.Type{event2.LoginStarted, event2.LoginModeDetermined} {
		b.Run(string(kind), func(b *testing.B) {
			for _, duplicate := range []bool{false, true} {
				name := "приём_и_удаление"
				if duplicate {
					name = "повтор"
				}
				b.Run(name, func(b *testing.B) {
					recorder, err := otel.New(otel.ResourceSettings{NodeName: "bench-node", Environment: "test"})
					if err != nil {
						b.Fatal(err)
					}
					b.Cleanup(func() {
						if err := recorder.Shutdown(context.Background()); err != nil {
							b.Error(err)
						}
					})
					store, err := memory.NewStore(memory.DefaultLimits(), recorder)
					if err != nil {
						b.Fatal(err)
					}
					now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
					observer, err := application.NewLoginObserver(store, &controlledClock{now: now})
					if err != nil {
						b.Fatal(err)
					}
					e := event2.Request{
						EventID: uuid.UUID{1}, ScenarioID: uuid.UUID{2},
						Type: kind, Source: event2.SourceMIS, OccurredAt: now,
					}
					if kind == event2.LoginModeDetermined {
						mode := event2.LoginModeCredentials
						e.Source, e.AuthMode = event2.SourceAuth, &mode
					}
					if duplicate {
						if _, err := observer.Process(&e); err != nil {
							b.Fatal(err)
						}
					}
					remove := func(state contract.State) error { return state.DeleteScenario(e.ScenarioID) }
					b.ReportAllocs()
					for b.Loop() {
						if _, err := observer.Process(&e); err != nil {
							b.Fatal(err)
						}
						// Удаление входит в измерение и ограничивает размер хранилища между итерациями.
						if !duplicate {
							if err := store.Within(remove); err != nil {
								b.Fatal(err)
							}
						}
					}
				})
			}
		})
	}
}

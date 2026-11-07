package event_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vendelev/sre-metrics/user-login/internal/domain/event"
)

func TestRequestClone(t *testing.T) {
	for _, withMode := range []bool{false, true} {
		name := "без варианта"
		if withMode {
			name = "с вариантом"
		}
		t.Run(name, func(t *testing.T) {
			original := event.Request{
				EventID: uuid.UUID{1}, ScenarioID: uuid.UUID{2},
				OccurredAt: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC),
				Type:       event.LoginStarted, Source: event.SourceMIS,
			}
			if withMode {
				mode := event.LoginModeCredentials
				original.Type, original.Source, original.AuthMode = event.LoginModeDetermined, event.SourceAuth, &mode
			}
			copy := original.Clone()
			if !reflect.DeepEqual(original, copy) {
				t.Fatal("копия отличается от исходного события")
			}
			copy.EventID = uuid.UUID{3}
			if original.EventID != (uuid.UUID{1}) {
				t.Fatal("изменение копии изменило исходный ID")
			}
			if withMode {
				*copy.AuthMode = event.LoginModeExistingSession
				if *original.AuthMode != event.LoginModeCredentials {
					t.Fatal("копия сохранила общий указатель на вариант")
				}
			}
		})
	}
}

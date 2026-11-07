package scenario_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vendelev/sre-metrics/user-login/internal/domain/event"
	"github.com/vendelev/sre-metrics/user-login/internal/domain/scenario"
)

func TestScenarioCloneIndependence(t *testing.T) {
	base := scenarioTime()
	original := scenario.New(uuid.New())
	mustAccept(t, original.Start(base, base))
	mustAccept(t, original.DetermineMode(event.LoginModeCredentials, base))
	copy := original.Clone()
	mustAccept(t, copy.Succeed(base.Add(time.Second), base))
	copy.TakeMeasurements()
	if original.Outcome() != "" || original.TakeMeasurements() != (scenario.Measurements{}) {
		t.Fatal("изменение копии изменило исходную сущность")
	}
	mustAccept(t, original.Succeed(base.Add(2*time.Second), base))
	measurements := original.TakeMeasurements()
	if measurements.Completion != scenario.OutcomeSuccess || measurements.SuccessfulDuration.Duration != 2*time.Second {
		t.Fatal("копия изменила отметки выдачи или исходное время успеха")
	}
	copy = original.Clone()
	if copy.TakeMeasurements() != (scenario.Measurements{}) {
		t.Fatal("клонирование потеряло отметки уже выданных измерений")
	}
}

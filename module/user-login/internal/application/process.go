package application

import (
	"errors"
	"fmt"
	"time"

	dContract "github.com/vendelev/sre-metrics/user-login/internal/domain/contract"
	dError "github.com/vendelev/sre-metrics/user-login/internal/domain/error"
	dEvent "github.com/vendelev/sre-metrics/user-login/internal/domain/event"
	dScenario "github.com/vendelev/sre-metrics/user-login/internal/domain/scenario"
)

// Process обрабатывает предметное событие после транспортной валидации.
func (o *LoginObserver) Process(event dEvent.Request) (ProcessResult, error) {
	var result ProcessResult
	err := o.store.Within(func(state dContract.State) error {
		now := o.clock.Now()
		var err error
		result, err = processEvent(state, event, now)
		return err
	})
	return result, operationError("обработка события", err)
}

func equalEvents(a, b dEvent.Request) bool {
	sameIDs := a.EventID == b.EventID && a.ScenarioID == b.ScenarioID
	sameFact := a.Source == b.Source && a.Type == b.Type && a.OccurredAt.Equal(b.OccurredAt)
	if !sameIDs || !sameFact {
		return false
	}
	if a.AuthMode == nil || b.AuthMode == nil {
		return a.AuthMode == nil && b.AuthMode == nil
	}
	return *a.AuthMode == *b.AuthMode
}

func processEvent(state dContract.State, event dEvent.Request, now time.Time) (ProcessResult, error) {
	if err := removeRetained(state, now); err != nil {
		return ProcessResult{}, err
	}
	if stored, ok := state.FindEvent(event.EventID); ok {
		if equalEvents(stored, event) {
			return ProcessResult{}, nil
		}
		return ProcessResult{}, dError.ErrConflict
	}
	if err := dError.CheckEventTime(event.OccurredAt, now); err != nil {
		return ProcessResult{}, fmt.Errorf("%w: %w", dError.ErrInvalidEvent, err)
	}
	loginScenario, exists := state.FindScenario(event.ScenarioID)
	if !exists {
		loginScenario = dScenario.New(event.ScenarioID)
	}
	checkErr := checkFact(loginScenario, event, now)
	if errors.Is(checkErr, dError.ErrConflict) {
		return ProcessResult{}, checkErr
	}
	if exists && loginScenario.CloseIfExpired(now) {
		if err := state.Commit(loginScenario, nil, loginScenario.TakeMeasurements()); err != nil {
			return ProcessResult{}, err
		}
	}
	if checkErr != nil {
		return ProcessResult{}, checkErr
	}
	if err := state.CheckCapacity(event.ScenarioID); err != nil {
		return ProcessResult{}, err
	}
	hadNegativeDuration := loginScenario.HasNegativeDuration()
	if err := acceptFact(loginScenario, event, now); err != nil {
		return ProcessResult{}, internalError("приём проверенного факта", err)
	}
	measurements := loginScenario.TakeMeasurements()
	if err := state.Commit(loginScenario, &event, measurements); err != nil {
		return ProcessResult{}, err
	}
	if !hadNegativeDuration && loginScenario.HasNegativeDuration() {
		return ProcessResult{NegativeDurationScenarioID: loginScenario.ID()}, nil
	}
	return ProcessResult{}, nil
}

func checkFact(loginScenario *dScenario.Scenario, event dEvent.Request, now time.Time) error {
	switch event.Type {
	case dEvent.LoginStarted:
		return loginScenario.CheckStart(event.OccurredAt, now)
	case dEvent.LoginModeDetermined:
		return loginScenario.CheckMode(*event.AuthMode, now)
	case dEvent.LoginSucceeded:
		return loginScenario.CheckOutcome(dScenario.OutcomeSuccess, now)
	case dEvent.LoginFailed:
		return loginScenario.CheckOutcome(dScenario.OutcomeTechnicalError, now)
	case dEvent.LoginRejected:
		return loginScenario.CheckOutcome(dScenario.OutcomeRejected, now)
	default:
		return dError.ErrInternal
	}
}

func acceptFact(scenario *dScenario.Scenario, event dEvent.Request, now time.Time) error {
	switch event.Type {
	case dEvent.LoginStarted:
		return scenario.Start(event.OccurredAt, now)
	case dEvent.LoginModeDetermined:
		return scenario.DetermineMode(*event.AuthMode, now)
	case dEvent.LoginSucceeded:
		return scenario.Succeed(event.OccurredAt, now)
	case dEvent.LoginFailed:
		return scenario.Fail(now)
	case dEvent.LoginRejected:
		return scenario.Reject(now)
	default:
		return dError.ErrInternal
	}
}

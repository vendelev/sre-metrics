package application

import (
	"errors"
	"fmt"
	"time"

	"github.com/vendelev/sre-metrics/user-login/internal/domain"
)

// Process обрабатывает предметное событие после транспортной валидации версии 1.
// Защитная проверка прямого вызова выполняется до доступа к состоянию; её отказ
// ничего не меняет. Ошибка последующей обработки не означает общий rollback:
// удаление по retention и уже зафиксированное самостоятельное закрытие сохраняются.
func (o *LoginObserver) Process(event domain.Event) (ProcessResult, error) {
	event, err := prepareEvent(event)
	if err != nil {
		return ProcessResult{}, err
	}
	var result ProcessResult
	err = o.store.Within(func(state domain.State) error {
		now := o.clock.Now()
		var err error
		result, err = processEvent(state, event, now)
		return err
	})
	return result, operationError("обработка события", err)
}

func processEvent(state domain.State, event domain.Event, now time.Time) (ProcessResult, error) {
	if err := removeRetained(state, now); err != nil {
		return ProcessResult{}, err
	}
	if stored, ok := state.FindEvent(event.EventID); ok {
		if equalEvents(stored, event) {
			return ProcessResult{}, nil
		}
		return ProcessResult{}, domain.ErrConflict
	}
	if err := domain.CheckEventTime(event.OccurredAt, now); err != nil {
		return ProcessResult{}, fmt.Errorf("%w: %w", domain.ErrInvalidEvent, err)
	}
	scenario, exists := state.FindScenario(event.ScenarioID)
	if !exists {
		scenario = domain.NewScenario(event.ScenarioID)
	}
	checkErr := checkFact(scenario, event, now)
	if errors.Is(checkErr, domain.ErrConflict) {
		return ProcessResult{}, checkErr
	}
	if exists && scenario.CloseIfExpired(now) {
		if err := state.Commit(scenario, nil, scenario.TakeMeasurements()); err != nil {
			return ProcessResult{}, err
		}
	}
	if checkErr != nil {
		return ProcessResult{}, checkErr
	}
	if err := state.CheckCapacity(event.ScenarioID); err != nil {
		return ProcessResult{}, err
	}
	hadNegativeDuration := scenario.HasNegativeDuration()
	if err := acceptFact(scenario, event, now); err != nil {
		return ProcessResult{}, internalError("приём проверенного факта", err)
	}
	measurements := scenario.TakeMeasurements()
	if err := state.Commit(scenario, &event, measurements); err != nil {
		return ProcessResult{}, err
	}
	if !hadNegativeDuration && scenario.HasNegativeDuration() {
		return ProcessResult{NegativeDurationScenarioID: scenario.ID()}, nil
	}
	return ProcessResult{}, nil
}

func checkFact(scenario *domain.Scenario, event domain.Event, now time.Time) error {
	switch event.Type {
	case domain.EventTypeLoginStarted:
		return scenario.CheckStart(event.OccurredAt, now)
	case domain.EventTypeLoginModeDetermined:
		return scenario.CheckMode(*event.Mode, now)
	case domain.EventTypeLoginSucceeded:
		return scenario.CheckOutcome(domain.OutcomeSuccess, now)
	case domain.EventTypeLoginFailed:
		return scenario.CheckOutcome(domain.OutcomeTechnicalError, now)
	case domain.EventTypeLoginRejected:
		return scenario.CheckOutcome(domain.OutcomeRejected, now)
	default:
		return domain.ErrInternal
	}
}

func acceptFact(scenario *domain.Scenario, event domain.Event, now time.Time) error {
	switch event.Type {
	case domain.EventTypeLoginStarted:
		return scenario.Start(event.OccurredAt, now)
	case domain.EventTypeLoginModeDetermined:
		return scenario.DetermineMode(*event.Mode, now)
	case domain.EventTypeLoginSucceeded:
		return scenario.Succeed(event.OccurredAt, now)
	case domain.EventTypeLoginFailed:
		return scenario.Fail(now)
	case domain.EventTypeLoginRejected:
		return scenario.Reject(now)
	default:
		return domain.ErrInternal
	}
}

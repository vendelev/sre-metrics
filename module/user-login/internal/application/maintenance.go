package application

import (
	"time"

	dContract "github.com/vendelev/sre-metrics/user-login/internal/domain/contract"
	dScenario "github.com/vendelev/sre-metrics/user-login/internal/domain/scenario"
)

// Maintain закрывает истёкшие ожидания и удаляет записи с истёкшим retention.
// Не запускает планировщик; повторный вызов безопасен и не увеличивает счётчики повторно.
func (o *LoginObserver) Maintain() error {
	err := o.store.Within(func(state dContract.State) error {
		now := o.clock.Now()
		if err := removeRetained(state, now); err != nil {
			return err
		}
		return state.VisitScenarios(func(scenario *dScenario.Scenario) error {
			if !scenario.CloseIfExpired(now) {
				return nil
			}
			return state.Commit(scenario, nil, scenario.TakeMeasurements())
		})
	})
	if err != nil {
		return internalError("обслуживание состояния", err)
	}
	return nil
}

func removeRetained(state dContract.State, now time.Time) error {
	return state.VisitScenarios(func(scenario *dScenario.Scenario) error {
		if scenario.RetentionExpired(now) {
			return state.DeleteScenario(scenario.ID())
		}
		return nil
	})
}

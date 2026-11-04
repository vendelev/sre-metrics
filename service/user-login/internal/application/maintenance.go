package application

import (
	"time"

	"github.com/vendelev/sre-metrics/service/user-login/internal/domain"
)

// Maintain закрывает истёкшие ожидания и удаляет записи с истёкшим retention.
// Не запускает планировщик; повторный вызов безопасен и не увеличивает счётчики повторно.
func (o *LoginObserver) Maintain() error {
	err := o.store.Within(func(state domain.State) error {
		now := o.clock.Now()
		if err := removeRetained(state, now); err != nil {
			return err
		}
		return state.VisitScenarios(func(scenario *domain.Scenario) error {
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

func removeRetained(state domain.State, now time.Time) error {
	return state.VisitScenarios(func(scenario *domain.Scenario) error {
		if scenario.RetentionExpired(now) {
			return state.DeleteScenario(scenario.ID())
		}
		return nil
	})
}

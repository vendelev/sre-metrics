package application

import (
	"fmt"

	"github.com/google/uuid"
	dContract "github.com/vendelev/sre-metrics/user-login/internal/domain/contract"
	dError "github.com/vendelev/sre-metrics/user-login/internal/domain/error"
)

// LoginObserver координирует наблюдение за сценариями входа.
// Все операции одного экземпляра должны использовать одно хранилище процесса.
type LoginObserver struct {
	store dContract.Store
	clock dContract.Clock
}

// NewLoginObserver принимает явно заданные хранилище и часы.
// Зависимости должны быть ненулевыми, в том числе не содержать типизированный nil.
func NewLoginObserver(store dContract.Store, clock dContract.Clock) (*LoginObserver, error) {
	if store == nil || clock == nil {
		return nil, fmt.Errorf("создание наблюдателя: %w: отсутствует хранилище или часы", dError.ErrInternal)
	}
	return &LoginObserver{store: store, clock: clock}, nil
}

// Maintain закрывает истёкшие ожидания и удаляет записи с истёкшим retention.
// Не запускает планировщик; повторный вызов безопасен и не увеличивает счётчики повторно.
func (o *LoginObserver) Maintain() error {
	err := o.store.Within(func(state dContract.State) error {
		now := o.clock.Now()
		if err := state.RemoveRetained(now); err != nil {
			return err
		}
		for scenario := range state.Scenarios() {
			if !scenario.CloseIfExpired(now) {
				continue
			}
			if err := state.Commit(scenario, nil, scenario.TakeMeasurements()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return internalError("обслуживание состояния", err)
	}
	return nil
}

// ProcessResult содержит только независимые данные безопасной диагностики.
// При непустом NegativeDurationScenarioID вызывающий код должен записать диагностический
// лог с этим ID без полного события. Process уже освободил защищённый участок.
type ProcessResult struct {
	NegativeDurationScenarioID uuid.UUID
}

func internalError(operation string, err error) error {
	return fmt.Errorf("%s: %w: %w", operation, dError.ErrInternal, err)
}

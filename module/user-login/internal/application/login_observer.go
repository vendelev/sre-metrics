package application

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
	dContract "github.com/vendelev/sre-metrics/user-login/internal/domain/contract"
	dError "github.com/vendelev/sre-metrics/user-login/internal/domain/error"
	dScenario "github.com/vendelev/sre-metrics/user-login/internal/domain/scenario"
)

// LoginObserver координирует наблюдение за сценариями входа, не выполняя авторизацию.
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

// ProcessResult содержит только независимые данные безопасной диагностики.
// При непустом NegativeDurationScenarioID вызывающий код должен записать диагностический
// лог с этим ID без полного события. Process уже освободил защищённый участок.
type ProcessResult struct {
	NegativeDurationScenarioID uuid.UUID
}

func operationError(operation string, err error) error {
	if err == nil {
		return nil
	}
	for _, expected := range []error{
		dError.ErrInvalidEvent, dError.ErrConflict, dScenario.ErrScenarioExpired, dError.ErrCapacityExceeded,
	} {
		if errors.Is(err, expected) {
			return fmt.Errorf("%s: %w", operation, err)
		}
	}
	return internalError(operation, err)
}

func internalError(operation string, err error) error {
	return fmt.Errorf("%s: %w: %w", operation, dError.ErrInternal, err)
}

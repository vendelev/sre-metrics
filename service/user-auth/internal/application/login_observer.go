package application

import (
	"errors"
	"fmt"

	"github.com/vendelev/sre-metrics/service/user-auth/internal/domain"
)

// LoginObserver координирует наблюдение за сценариями входа, не выполняя авторизацию.
// Все операции одного экземпляра должны использовать одно хранилище процесса.
type LoginObserver struct {
	store domain.Store
	clock domain.Clock
}

// NewLoginObserver принимает явно заданные хранилище и часы.
// Зависимости должны быть ненулевыми, в том числе не содержать типизированный nil.
func NewLoginObserver(store domain.Store, clock domain.Clock) (*LoginObserver, error) {
	if store == nil || clock == nil {
		return nil, fmt.Errorf("создание наблюдателя: %w: отсутствует хранилище или часы", domain.ErrInternal)
	}
	return &LoginObserver{store: store, clock: clock}, nil
}

// ProcessResult содержит только независимые данные безопасной диагностики.
// При непустом NegativeDurationScenarioID вызывающий код должен записать диагностический
// лог с этим ID без полного события. Process уже освободил защищённый участок.
type ProcessResult struct {
	NegativeDurationScenarioID string
}

func operationError(operation string, err error) error {
	if err == nil {
		return nil
	}
	for _, expected := range []error{
		domain.ErrInvalidEvent, domain.ErrConflict, domain.ErrScenarioExpired, domain.ErrCapacityExceeded,
	} {
		if errors.Is(err, expected) {
			return fmt.Errorf("%s: %w", operation, err)
		}
	}
	return internalError(operation, err)
}

func internalError(operation string, err error) error {
	return fmt.Errorf("%s: %w: %w", operation, domain.ErrInternal, err)
}

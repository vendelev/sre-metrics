// Package listener связывает файловый вход с приёмом событий user-login.
package listener

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	pkgApi "github.com/vendelev/sre-metrics/pkg/fileinbox/api"
	aFactory "github.com/vendelev/sre-metrics/user-login/internal/application/factory"
	dError "github.com/vendelev/sre-metrics/user-login/internal/domain/error"
	dEvent "github.com/vendelev/sre-metrics/user-login/internal/domain/event"
	iQueue "github.com/vendelev/sre-metrics/user-login/internal/infrastructure/queue"
)

// NewInboxHandler создаёт обработчик содержимого одного файла. Он проверяет событие
// на время текущей попытки и передаёт его готовой неблокирующей операции enqueue.
// Операция должна быть ненулевой.
func NewInboxHandler(enqueue func(*dEvent.Request) error) pkgApi.Handle {
	return func(contents []byte, now time.Time) error {
		event, err := aFactory.DecodeEvent(bytes.NewReader(contents), now)
		if err != nil {
			return fmt.Errorf("разбор события из файла: %w", err)
		}

		err = enqueue(&event)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, iQueue.ErrClosed), errors.Is(err, dError.ErrOverloaded):
			return pkgApi.ErrRetry
		default:
			// Решение о повторе принимает адаптер, а не переданная операция.
			return errors.New("не удалось поставить событие в очередь")
		}
	}
}

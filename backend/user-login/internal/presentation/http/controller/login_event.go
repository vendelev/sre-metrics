package controller

import (
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"

	"github.com/vendelev/sre-metrics/pkg/clock"
	"github.com/vendelev/sre-metrics/user-login/internal/application/factory"
	dError "github.com/vendelev/sre-metrics/user-login/internal/domain/error"
	dEvent "github.com/vendelev/sre-metrics/user-login/internal/domain/event"
)

// LoginEvent обрабатывает транспортное представление приёма событий:
// проверяет метод представления, Content-Type, JSON и возраст, преобразует
// запрос в предметное событие и передаёт его операции приёма. Регистрацию
// маршрутов выполняет bootstrap модуля.
type LoginEvent struct {
	enqueue func(*dEvent.Request) error
	logger  *slog.Logger
	clock   clock.Clock
}

// NewLoginEvent принимает готовые операцию приёма, логгер и часы. Зависимости должны
// быть ненулевыми. Контроллер не создаёт зависимости и не запускает сервер.
func NewLoginEvent(
	enqueue func(*dEvent.Request) error,
	logger *slog.Logger,
	clock clock.Clock,
) (*LoginEvent, error) {
	if enqueue == nil {
		return nil, errors.New("не задана операция приёма событий")
	}
	if logger == nil {
		return nil, errors.New("не задан логгер HTTP")
	}
	if clock == nil {
		return nil, errors.New("не заданы часы HTTP")
	}
	return &LoginEvent{enqueue: enqueue, logger: logger, clock: clock}, nil
}

// AcceptEvent обрабатывает POST /metrics/b/user-login/events и формирует согласованные ответы.
// accept неблокирующе ставит проверенное событие в очередь. После успеха
// accept может удерживать событие; контроллер больше его не использует.
func (c *LoginEvent) AcceptEvent(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return
	}
	event, err := factory.DecodeEvent(r.Body, c.clock.Now())
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_event")
		return
	}
	err = c.enqueue(&event)
	if err != nil {
		status, code := acceptError(err)
		if status == http.StatusInternalServerError {
			c.logger.ErrorContext(r.Context(), "не удалось принять событие", slog.Any("error", err))
		}
		writeError(w, r, status, code)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// MethodNotAllowed отвечает на метод пути /metrics/b/user-login/events, отличный от POST,
// согласованным статусом, заголовком Allow и телом ошибки.
func (c *LoginEvent) MethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", http.MethodPost)
	writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
}

func acceptError(err error) (int, string) {
	switch {
	case errors.Is(err, dError.ErrInvalidEvent):
		return http.StatusBadRequest, "invalid_event"
	case errors.Is(err, dError.ErrOverloaded):
		return http.StatusServiceUnavailable, "overloaded"
	default:
		return http.StatusInternalServerError, "internal_error"
	}
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	// code выбирается только из внутренних констант. После ошибки записи другого ответа уже нет.
	if _, err := fmt.Fprintf(w, "{\"error\":\"%s\"}\n", code); err != nil {
		return
	}
}

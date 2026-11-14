package httpapi

import (
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/vendelev/sre-metrics/user-login/internal/domain/contract"
	dError "github.com/vendelev/sre-metrics/user-login/internal/domain/error"
	dEvent "github.com/vendelev/sre-metrics/user-login/internal/domain/event"
)

// NewRouter создаёт роутер chi с POST /events и согласованными HTTP-ответами.
// Он получает часы для проверки возраста, но не создаёт зависимости и не запускает сервер.
// accept неблокирующе ставит проверенное событие в очередь.
// После успеха accept может удерживать событие; адаптер больше его не использует.
func NewRouter(
	enqueue func(*dEvent.Request) error,
	logger *slog.Logger,
	clock contract.Clock,
) (http.Handler, error) {
	if enqueue == nil {
		return nil, errors.New("не задана операция приёма событий")
	}
	if logger == nil {
		return nil, errors.New("не задан логгер HTTP")
	}
	if clock == nil {
		return nil, errors.New("не заданы часы HTTP")
	}
	router := chi.NewRouter()
	router.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
	})
	router.Post("/events", func(w http.ResponseWriter, r *http.Request) {
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			writeError(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type")
			return
		}
		event, err := decodeEvent(r.Body, clock.Now())
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_event")
			return
		}
		err = enqueue(&event)
		if err != nil {
			status, code := acceptError(err)
			if status == http.StatusInternalServerError {
				logger.ErrorContext(r.Context(), "не удалось принять событие", slog.Any("error", err))
			}
			writeError(w, r, status, code)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	return router, nil
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

package httpapi

import (
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/vendelev/sre-metrics/user-login/internal/application"
	dError "github.com/vendelev/sre-metrics/user-login/internal/domain/error"
	dEvent "github.com/vendelev/sre-metrics/user-login/internal/domain/event"
	dScenario "github.com/vendelev/sre-metrics/user-login/internal/domain/scenario"
)

// NewHandler создаёт роутер chi с POST /events и согласованными HTTP-ответами.
// Он не создаёт наблюдатель, хранилище или часы и не запускает сервер.
// Операция вызывается синхронно и не должна изменять или удерживать переданное событие.
func NewHandler(
	process func(*dEvent.Request) (application.ProcessResult, error),
	logger *slog.Logger,
) (http.Handler, error) {
	if process == nil || logger == nil {
		return nil, errors.New("для HTTP-адаптера необходимы операция обработки и логгер")
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
		event, err := decodeEvent(r.Body)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_event")
			return
		}
		result, err := process(&event)
		if err != nil {
			status, code := processError(err)
			if status == http.StatusInternalServerError {
				logger.ErrorContext(r.Context(), "не удалось обработать событие", slog.Any("error", err))
			}
			writeError(w, r, status, code)
			return
		}
		if result.NegativeDurationScenarioID != uuid.Nil {
			logger.WarnContext(r.Context(), "исходный момент успеха раньше начала: длительность не учитывается",
				slog.String("scenario_id", result.NegativeDurationScenarioID.String()))
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return router, nil
}

func processError(err error) (int, string) {
	switch {
	case errors.Is(err, dError.ErrInvalidEvent):
		return http.StatusBadRequest, "invalid_event"
	case errors.Is(err, dError.ErrConflict):
		return http.StatusConflict, "event_conflict"
	case errors.Is(err, dScenario.ErrScenarioExpired):
		return http.StatusGone, "scenario_expired"
	case errors.Is(err, dError.ErrCapacityExceeded):
		return http.StatusServiceUnavailable, "capacity_exceeded"
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

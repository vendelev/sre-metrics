package httpapi

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"

	"github.com/google/uuid"
	"github.com/vendelev/sre-metrics/user-login/internal/application"
	dError "github.com/vendelev/sre-metrics/user-login/internal/domain/error"
	dEvent "github.com/vendelev/sre-metrics/user-login/internal/domain/event"
	dScenario "github.com/vendelev/sre-metrics/user-login/internal/domain/scenario"
)

// Settings задаёт ограничения HTTP-адаптера без чтения внешней конфигурации.
type Settings struct {
	MaxBodyBytes int64
}

// DefaultSettings возвращает начальный лимит тела по BR-36.
func DefaultSettings() Settings { return Settings{MaxBodyBytes: 4096} }

// Handler принимает события на пути /events. После создания его нельзя изменять.
// Путь регистрируется через mux.Handle("/events", handler), без ограничения метода в mux.
type Handler struct {
	process      func(*dEvent.Request) (application.ProcessResult, error)
	logger       *slog.Logger
	maxBodyBytes int64
}

// NewHandler принимает готовую операцию обработки, логгер и положительный лимит тела.
// Он не создаёт наблюдатель, хранилище или часы и не запускает сервер.
// Операция вызывается синхронно и не должна изменять или удерживать переданное событие.
func NewHandler(
	process func(*dEvent.Request) (application.ProcessResult, error),
	logger *slog.Logger,
	settings Settings,
) (*Handler, error) {
	if process == nil || logger == nil {
		return nil, errors.New("для HTTP-адаптера необходимы операция обработки и логгер")
	}
	if settings.MaxBodyBytes <= 0 {
		return nil, errors.New("лимит HTTP-тела должен быть положительным")
	}
	return &Handler{process: process, logger: logger, maxBodyBytes: settings.MaxBodyBytes}, nil
}

// ServeHTTP проверяет транспортный контракт до обращения к состоянию приложения.
// Маршрутизация неизвестных путей остаётся ответственностью вызывающего кода.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return
	}
	// Читаем всё ограниченное тело до JSON: превышение даже после объекта важнее синтаксиса.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, r, http.StatusRequestEntityTooLarge, "payload_too_large")
		} else {
			writeError(w, r, http.StatusBadRequest, "invalid_event")
		}
		return
	}
	event, err := decodeEvent(body)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_event")
		return
	}
	result, err := h.process(&event)
	if err != nil {
		status, code := processError(err)
		if status == http.StatusInternalServerError {
			h.logger.ErrorContext(r.Context(), "не удалось обработать событие", slog.Any("error", err))
		}
		writeError(w, r, status, code)
		return
	}
	if result.NegativeDurationScenarioID != uuid.Nil {
		h.logger.WarnContext(r.Context(), "исходный момент успеха раньше начала: длительность не учитывается",
			slog.String("scenario_id", result.NegativeDurationScenarioID.String()))
	}
	w.WriteHeader(http.StatusNoContent)
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

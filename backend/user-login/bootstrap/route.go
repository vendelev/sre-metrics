package bootstrap

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/vendelev/sre-metrics/pkg/clock"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/queue"
	"github.com/vendelev/sre-metrics/user-login/internal/presentation/http/controller"
)

// routeOf собирает HTTP-обработчик приёма событий: bootstrap создаёт
// Controller, регистрирует маршруты и возвращает приложению готовый handler.
// Журнал обработчика отделён по компоненту от сервисного логгера.
func routeOf(
	q *queue.EventQueue,
	logger *slog.Logger,
	clock clock.Clock,
) (http.Handler, error) {
	eventsController, err := controller.NewLoginEvent(q.Enqueue, logger.With("component", "http"), clock)
	if err != nil {
		return nil, err
	}
	router := chi.NewRouter()
	router.MethodNotAllowed(eventsController.MethodNotAllowed)
	router.Post("/metrics/b/user-login/events", eventsController.AcceptEvent)
	return router, nil
}

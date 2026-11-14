package bootstrap

import (
	"errors"
	"log/slog"
	"net/http"

	"go.uber.org/dig"

	"github.com/vendelev/sre-metrics/user-login/config"
	"github.com/vendelev/sre-metrics/user-login/internal/application"
	"github.com/vendelev/sre-metrics/user-login/internal/domain/contract"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/memory"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/otel"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/queue"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/worker"
	"github.com/vendelev/sre-metrics/user-login/internal/presentation/httpapi"
)

// Provide регистрирует конструкторы компонентов модуля в общем контейнере
// зависимостей приложения. Значения вызывающего кода — типизированные
// настройки модуля, корневой журнал и часы — приложение добавляет в контейнер
// само. Контейнер только собирает объекты; запуск и остановку фоновых задач
// модуля задаёт Module, фазовый протокол координирует bootstrap приложения.
func Provide(container *dig.Container) error {
	return errors.Join(
		container.Provide(otel.Prepare),
		container.Provide(queueOf),
		container.Provide(storeOf),
		container.Provide(application.NewLoginObserver),
		container.Provide(routerOf),
		container.Provide(workerOf),
		container.Provide(NewModule),
	)
}

// queueOf собирает ограниченную очередь приёма из проверенной конфигурации модуля.
func queueOf(cfg *config.Config) *queue.EventQueue {
	return queue.NewEventQueue(cfg.EventQueueSize)
}

// storeOf собирает хранилище сценариев с лимитами конфигурации; конкретный
// регистратор измерений модуля удовлетворяет предметному контракту хранилища.
func storeOf(
	cfg *config.Config,
	recorder *otel.Recorder,
) (contract.Store, error) {
	return memory.NewStore(memory.Limits{
		MaxScenarios:         cfg.MaxScenarios,
		MaxEventsPerScenario: cfg.MaxEventsPerScenario,
	}, recorder)
}

// routerOf собирает предметный HTTP-обработчик приёма событий; журнал
// обработчика отделён по компоненту от сервисного логгера.
func routerOf(
	q *queue.EventQueue,
	logger *slog.Logger,
	clock Clock,
) (http.Handler, error) {
	return httpapi.NewRouter(q.Enqueue, logger.With("component", "http"), clock)
}

// workerOf собирает последовательную обработку очереди с интервалом
// обслуживания из конфигурации модуля.
func workerOf(
	o *application.LoginObserver,
	q *queue.EventQueue,
	cfg *config.Config,
	logger *slog.Logger,
) (*worker.Worker, error) {
	return worker.New(o, q, cfg.MaintenanceInterval, logger)
}

// NewModule собирает модуль из уже собранных компонентов. Сборка не запускает
// фоновых задач и не активирует инструменты: активацию выполняет Start в meter
// запущенного pipeline core, задачу воркера — StartWorker.
func NewModule(
	recorder *otel.Recorder,
	w *worker.Worker,
	q *queue.EventQueue,
	handler http.Handler,
) *Module {
	return &Module{
		recorder: recorder,
		worker:   w,
		queue:    q,
		handler:  handler,
		failed:   make(chan struct{}, 1),
	}
}

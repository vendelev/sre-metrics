package bootstrap

import (
	"errors"
	"log/slog"

	"go.uber.org/dig"

	"github.com/vendelev/sre-metrics/pkg/clock"
	"github.com/vendelev/sre-metrics/pkg/httproute"
	"github.com/vendelev/sre-metrics/user-login/config"
	"github.com/vendelev/sre-metrics/user-login/internal/application/usecase"
	"github.com/vendelev/sre-metrics/user-login/internal/domain/contract"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/memory"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/otel"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/queue"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/worker"
)

// Provide регистрирует конструкторы компонентов модуля в общем контейнере
// зависимостей приложения. Значения вызывающего кода — типизированные
// настройки модуля, корневой журнал и часы — приложение добавляет в контейнер
// само. Контейнер только собирает объекты; запуск и остановку фоновых задач
// модуля задаёт Module, фазовый протокол координирует bootstrap приложения.
// Публичные маршруты модуля публикуются в группу http-routes приложения.
func Provide(container *dig.Container) error {
	return errors.Join(
		container.Provide(otel.Prepare),
		container.Provide(queueOf),
		container.Provide(storeOf),
		container.Provide(usecase.NewLoginObserver),
		container.Provide(workerOf),
		container.Provide(NewModule),
		container.Provide(routesOf, dig.Group("http-routes,flatten")),
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

// workerOf собирает последовательную обработку очереди с интервалом
// обслуживания из конфигурации модуля.
func workerOf(
	o *usecase.LoginObserver,
	q *queue.EventQueue,
	cfg *config.Config,
	logger *slog.Logger,
) (*worker.Worker, error) {
	return worker.New(o, q.Events(), q.Close, cfg.MaintenanceInterval, logger)
}

// NewModule собирает модуль из уже собранных компонентов, включая предметный
// HTTP-обработчик. Сборка не запускает фоновых задач и не активирует
// инструменты: активацию выполняет Start в meter запущенного pipeline core,
// задачу воркера — StartWorker.
func NewModule(
	recorder *otel.Recorder,
	w *worker.Worker,
	q *queue.EventQueue,
	logger *slog.Logger,
	clock clock.Clock,
) (*Module, error) {
	handler, err := routeOf(q, logger, clock)
	if err != nil {
		return nil, err
	}
	return &Module{
		recorder: recorder,
		worker:   w,
		queue:    q,
		handler:  handler,
		failed:   make(chan struct{}, 1),
	}, nil
}

// routesOf публикует маршруты модуля для корневой сборки приложения.
// Приложение выбирает внешний префикс и не знает внутренние пакеты модуля.
func routesOf(m *Module) []httproute.Route {
	return []httproute.Route{{
		Prefix:  "/",
		Handler: m.Handler(),
	}}
}

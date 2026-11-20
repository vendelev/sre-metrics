package bootstrap

import (
	"errors"
	"fmt"
	"log/slog"

	"go.uber.org/dig"

	"github.com/vendelev/sre-metrics/pkg/clock"
	"github.com/vendelev/sre-metrics/pkg/fileinbox"
	"github.com/vendelev/sre-metrics/pkg/httproute"
	"github.com/vendelev/sre-metrics/user-login/config"
	"github.com/vendelev/sre-metrics/user-login/internal/application/usecase"
	"github.com/vendelev/sre-metrics/user-login/internal/domain/contract"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/memory"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/otel"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/queue"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/worker"
	"github.com/vendelev/sre-metrics/user-login/internal/presentation/listener"
)

// Provide регистрирует конструкторы компонентов модуля в общем контейнере
// зависимостей приложения. Значения вызывающего кода — настройки обработки и
// файлового входа, корневой журнал и часы — приложение добавляет в контейнер
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
		container.Provide(inboxOf),
		container.Provide(NewModule),
		container.Provide(routesOf, dig.Group("http-routes,flatten")),
	)
}

// queueOf собирает ограниченную очередь приёма из проверенной конфигурации модуля.
func queueOf(cfg config.Processing) *queue.EventQueue {
	return queue.NewEventQueue(cfg.EventQueueSize)
}

// storeOf собирает хранилище сценариев с лимитами конфигурации; конкретный
// регистратор измерений модуля удовлетворяет предметному контракту хранилища.
func storeOf(
	cfg config.Processing,
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
	cfg config.Processing,
	logger *slog.Logger,
) (*worker.Worker, error) {
	return worker.New(o, q.Events(), q.Close, cfg.MaintenanceInterval, logger)
}

func inboxOf(
	cfg config.Inbox,
	q *queue.EventQueue,
	logger *slog.Logger,
	clk clock.Clock,
) (*fileinbox.Inbox, error) {
	input, err := fileinbox.New(fileinbox.Config{
		Dir:             cfg.Dir,
		ScanInterval:    cfg.ScanInterval,
		MaxFilesPerScan: cfg.MaxFilesPerScan,
		MaxFileBytes:    cfg.MaxFileBytes,
		MaxAttempts:     cfg.MaxAttempts,
	}, listener.NewInboxHandler(q.Enqueue), logger, clk)
	if err != nil {
		return nil, fmt.Errorf("сборка файлового входа: %w", err)
	}
	return input, nil
}

// NewModule собирает модуль из уже собранных компонентов, включая предметный
// HTTP-обработчик. Сборка не запускает фоновых задач и не активирует инструменты.
func NewModule(
	recorder *otel.Recorder,
	w *worker.Worker,
	input *fileinbox.Inbox,
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
		inbox:    input,
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

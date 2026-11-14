// Package userlogin предназначен для сборки и запуска сервиса метрик входа.
package userlogin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"

	"go.uber.org/dig"

	"github.com/vendelev/sre-metrics/core/bootstrap"
	coreconfig "github.com/vendelev/sre-metrics/core/config"
	"github.com/vendelev/sre-metrics/user-login/internal/application"
	"github.com/vendelev/sre-metrics/user-login/internal/domain/contract"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/clock"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/memory"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/otel"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/queue"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/worker"
	"github.com/vendelev/sre-metrics/user-login/internal/presentation/config"
	"github.com/vendelev/sre-metrics/user-login/internal/presentation/httpapi"
)

// Run собирает и запускает сервис до отмены ctx или ошибки компонента.
// Успешная остановка дожидается запросов и обработки очереди; сигналы обрабатывает cmd.
func Run(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("конфигурация user-login: %w", err)
	}
	settings, err := coreconfig.Load()
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("service", "user-login")
	app, err := newService(&cfg, settings, logger, clock.RealTime{})
	if err != nil {
		return err
	}
	return app.run(ctx, nil)
}

// newService собирает контейнер зависимостей: значения вызывающего кода,
// технические компоненты core и компоненты модуля. Контейнер только собирает
// объекты; открытие ресурсов и запуск фоновых задач задаёт runtime.go.
func newService(
	cfg *config.Config,
	settings coreconfig.HTTP,
	logger *slog.Logger,
	clock contract.Clock,
) (*service, error) {
	container := dig.New()
	if err := errors.Join(
		// Значения вызывающего кода.
		container.Provide(func() *config.Config { return cfg }),
		container.Provide(func() coreconfig.HTTP { return settings }),
		container.Provide(func() *slog.Logger { return logger }),
		container.Provide(func() contract.Clock { return clock }),
		// Технические компоненты core.
		bootstrap.Provide(container),
		container.Provide(func() bootstrap.Views { return bootstrap.Views{otel.HistogramView()} }),
		// Компоненты модуля.
		container.Provide(otel.Prepare),
		container.Provide(queueOf),
		container.Provide(storeOf),
		container.Provide(application.NewLoginObserver),
		container.Provide(routerOf),
		container.Provide(workerOf),
	); err != nil {
		return nil, err
	}
	var app *service
	if err := container.Invoke(func(
		httpServer *bootstrap.Server,
		w *worker.Worker,
		q *queue.EventQueue,
		r *otel.Recorder,
		m *bootstrap.Metrics,
	) {
		app = &service{
			httpServer: httpServer,
			worker:     w,
			eventQueue: q,
			recorder:   r,
			metrics:    m,
			cfg:        *cfg,
			logger:     logger,
		}
	}); err != nil {
		return nil, err
	}
	return app, nil
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

// routerOf собирает предметный HTTP-обработчик приёма событий; журнал обработчика
// отделён по компоненту от сервисного логгера.
func routerOf(
	q *queue.EventQueue,
	logger *slog.Logger,
	clock contract.Clock,
) (http.Handler, error) {
	return httpapi.NewRouter(q.Enqueue, logger.With("component", "http"), clock)
}

// workerOf собирает последовательную обработку очереди с интервалом обслуживания
// из конфигурации модуля.
func workerOf(
	o *application.LoginObserver,
	q *queue.EventQueue,
	cfg *config.Config,
	logger *slog.Logger,
) (*worker.Worker, error) {
	return worker.New(o, q, cfg.MaintenanceInterval, logger)
}

func (s *service) run(ctx context.Context, ready func(net.Addr)) error {
	s.ready = ready
	s.failed = make(chan struct{}, 1)
	// Неуспешный запуск сам выполняет очистку и возвращает ошибку без разбора
	// её состава; специальная нормализация отмены запуска не выполняется.
	startErr := s.start(ctx)
	var stopErr error
	if startErr == nil {
		select {
		case <-ctx.Done():
		case <-s.failed:
		case <-s.httpServer.Failed():
		}
		stopErr = s.stop(ctx)
	}
	result := errors.Join(startErr, stopErr)
	if result != nil {
		s.logger.Error("не удалось штатно остановить сервис", slog.Any("error", result))
	} else {
		s.logger.Info("сервис user-login штатно остановлен")
	}
	return result
}

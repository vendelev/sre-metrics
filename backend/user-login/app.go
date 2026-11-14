// Package userlogin предназначен для сборки и запуска сервиса метрик входа.
package userlogin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"

	"github.com/vendelev/sre-metrics/user-login/internal/application"
	"github.com/vendelev/sre-metrics/user-login/internal/domain/contract"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/clock"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/httpserver"
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
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("service", "user-login")
	app, err := newService(&cfg, logger, clock.RealTime{})
	if err != nil {
		return err
	}
	return app.run(ctx, nil)
}

// newService только собирает объекты: ресурсы и фоновые задачи открывает
// протокол запуска в runtime.go.
func newService(cfg *config.Config, logger *slog.Logger, clock contract.Clock) (*service, error) {
	recorder, err := otel.Prepare()
	if err != nil {
		return nil, err
	}

	store, err := memory.NewStore(memory.Limits{
		MaxScenarios:         cfg.MaxScenarios,
		MaxEventsPerScenario: cfg.MaxEventsPerScenario,
	}, recorder)
	if err != nil {
		return nil, err
	}

	observer, err := application.NewLoginObserver(store, clock)
	if err != nil {
		return nil, err
	}

	eventQueue := queue.NewEventQueue(cfg.EventQueueSize)
	handler, err := httpapi.NewRouter(eventQueue.Enqueue, logger.With("component", "http"), clock)
	if err != nil {
		return nil, err
	}

	httpServer := httpserver.New(httpserver.Config{
		Addr:              cfg.ListenAddr,
		ReadHeaderTimeout: cfg.HTTPReadHeaderTimeout,
		ReadTimeout:       cfg.HTTPReadTimeout,
		WriteTimeout:      cfg.HTTPWriteTimeout,
		IdleTimeout:       cfg.HTTPIdleTimeout,
	}, handler, slog.NewLogLogger(logger.With("component", "http").Handler(), slog.LevelError))

	worker, err := worker.New(observer, eventQueue, cfg.MaintenanceInterval, logger)
	if err != nil {
		return nil, err
	}

	// Сервис сохраняет собственный снимок, а не изменяемую конфигурацию вызывающего кода.
	return &service{
		httpServer: httpServer,
		worker:     worker,
		eventQueue: eventQueue,
		recorder:   recorder,
		cfg:        *cfg,
		logger:     logger,
	}, nil
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

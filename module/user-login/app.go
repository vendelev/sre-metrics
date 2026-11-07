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
	"sync"

	"github.com/vendelev/sre-metrics/user-login/internal/application"
	"github.com/vendelev/sre-metrics/user-login/internal/domain/contract"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/clock"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/memory"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/otel"
	"github.com/vendelev/sre-metrics/user-login/internal/presentation/config"
	"github.com/vendelev/sre-metrics/user-login/internal/presentation/httpapi"
)

// Run собирает и запускает сервис до отмены ctx или ошибки компонента.
// Успешная остановка дожидается запросов и обслуживания; сигналы обрабатывает cmd.
func Run(ctx context.Context) error {
	cfg, err := config.Load(os.LookupEnv)
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

// service владеет компонентами и ресурсами одного запуска. Сборка выполняется
// явной цепочкой конструкторов; контейнер зависимостей и его lifecycle
// не используются.
type service struct {
	server   *http.Server
	observer *application.LoginObserver
	recorder *otel.Recorder
	cfg      config.Config
	logger   *slog.Logger
	pipeline *otel.Pipeline

	ready           func(net.Addr)
	listener        *ownedListener
	stopMaintenance context.CancelFunc
	serverTask      *backgroundTask
	maintenanceTask *backgroundTask
	failed          chan struct{}
	stopOnce        sync.Once
	stopErr         error // Читается после stopOnce.Do, включая конкурентный повторный вызов.
}

// newService только собирает объекты: listener и фоновые задачи запускает start.
func newService(cfg *config.Config, logger *slog.Logger, clock contract.Clock) (*service, error) {
	recorder, err := otel.Prepare(otel.ResourceSettings{NodeName: cfg.NodeName, Environment: cfg.Environment})
	if err != nil {
		return nil, err
	}
	store, err := memory.NewStore(memory.Limits{
		MaxScenarios: cfg.MaxScenarios, MaxEventsPerScenario: cfg.MaxEventsPerScenario,
	}, recorder)
	if err != nil {
		return nil, err
	}
	observer, err := application.NewLoginObserver(store, clock)
	if err != nil {
		return nil, err
	}
	handler, err := httpapi.NewHandler(observer.Process, logger.With("component", "http"),
		httpapi.Settings{MaxBodyBytes: cfg.MaxBodyBytes})
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/events", handler)
	server := &http.Server{
		Addr: cfg.ListenAddr, Handler: mux,
		ReadHeaderTimeout: cfg.HTTPReadHeaderTimeout, ReadTimeout: cfg.HTTPReadTimeout,
		WriteTimeout: cfg.HTTPWriteTimeout, IdleTimeout: cfg.HTTPIdleTimeout,
		ErrorLog: slog.NewLogLogger(logger.With("component", "http").Handler(), slog.LevelError),
	}
	// Сервис сохраняет собственный снимок, а не изменяемую конфигурацию вызывающего кода.
	return &service{server: server, observer: observer, recorder: recorder, cfg: *cfg, logger: logger}, nil
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

package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"

	"go.uber.org/dig"

	appconfig "github.com/vendelev/sre-metrics/cmd/user-login-httpserver/config"
	"github.com/vendelev/sre-metrics/pkg/clock"
	pkgHttpServerApi "github.com/vendelev/sre-metrics/pkg/httpserver/api"
	pkgHttpServerBootstrap "github.com/vendelev/sre-metrics/pkg/httpserver/bootstrap"
	pkgHttpServerConfig "github.com/vendelev/sre-metrics/pkg/httpserver/config"
	pkgOtlpApi "github.com/vendelev/sre-metrics/pkg/otlpmetrics/api"
	pkgOtlpBootstrap "github.com/vendelev/sre-metrics/pkg/otlpmetrics/bootstrap"
	pkgTask "github.com/vendelev/sre-metrics/pkg/task"
	userlogin "github.com/vendelev/sre-metrics/user-login/bootstrap"
	userloginConfig "github.com/vendelev/sre-metrics/user-login/config"
)

// Run загружает настройки процесса, функционального модуля и HTTP-сервера, собирает
// приложение и запускает его до отмены ctx или ошибки компонента. Успешная
// остановка дожидается запросов и обработки очереди; сигналы обрабатывает main.
func Run(ctx context.Context) error {
	process, err := appconfig.Load()
	if err != nil {
		return fmt.Errorf("конфигурация процесса: %w", err)
	}
	moduleCfg, err := userloginConfig.Load()
	if err != nil {
		return fmt.Errorf("конфигурация user-login: %w", err)
	}
	settings, err := pkgHttpServerConfig.Load()
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("service", "user-login")
	app, err := newApp(process, &moduleCfg, settings, logger, clock.RealTime{})
	if err != nil {
		return err
	}
	return app.run(ctx, nil)
}

// App владеет собранным приложением одного запуска: сервер и pipeline метрик,
// модуль user-login и настройки процесса. Сборка выполняется явной цепочкой
// конструкторов в контейнере; контейнер только собирает объекты. Поля после
// пустой строки — состояние протокола одного запуска: этот файл задаёт его
// порядок и очистку.
type App struct {
	server  pkgHttpServerApi.Server
	metrics pkgOtlpApi.Pipeline
	module  *userlogin.Module
	cfg     appconfig.Config
	logger  *slog.Logger

	serve    *pkgTask.Task
	failed   chan struct{}
	ready    func(net.Addr)
	stopOnce sync.Once
	stopErr  error // Читается после stopOnce.Do, включая конкурентный повторный вызов.
}

// newApp собирает контейнер зависимостей: значения вызывающего кода,
// технические компоненты, компоненты модуля и соединение handler модуля
// с общим сервером.
func newApp(
	process appconfig.Config,
	moduleCfg *userloginConfig.Config,
	settings pkgHttpServerConfig.HTTP,
	logger *slog.Logger,
	domainClock clock.Clock,
) (*App, error) {
	container := dig.New()
	if err := errors.Join(
		// Значения вызывающего кода.
		container.Provide(func() appconfig.Config { return process }),
		container.Provide(func() userloginConfig.Processing { return moduleCfg.Processing }),
		container.Provide(func() userloginConfig.Inbox { return moduleCfg.Inbox }),
		container.Provide(func() pkgHttpServerConfig.HTTP { return settings }),
		container.Provide(func() *slog.Logger { return logger }),
		container.Provide(func() clock.Clock { return domainClock }),
		// Технический pipeline с View модуля-потребителя.
		container.Provide(func(logger *slog.Logger) pkgOtlpApi.Pipeline {
			return pkgOtlpBootstrap.New(logger.With("component", "otel"), userlogin.HistogramView())
		}),
		// Компоненты модуля: Module публикует свои маршруты в группу приложения,
		// rootHandler агрегирует их в единственный http.Handler сервера.
		userlogin.Provide(container),
		container.Provide(rootHandler),
		container.Provide(func(cfg pkgHttpServerConfig.HTTP, handler http.Handler, logger *slog.Logger) pkgHttpServerApi.Server {
			return pkgHttpServerBootstrap.New(cfg, handler,
				slog.NewLogLogger(logger.With("component", "http").Handler(), slog.LevelError))
		}),
	); err != nil {
		return nil, err
	}
	var app *App
	if err := container.Invoke(func(
		server pkgHttpServerApi.Server,
		metrics pkgOtlpApi.Pipeline,
		module *userlogin.Module,
	) {
		app = &App{
			server:  server,
			failed:  make(chan struct{}, 1),
			metrics: metrics,
			module:  module,
			cfg:     process,
			logger:  logger,
		}
	}); err != nil {
		return nil, err
	}
	return app, nil
}

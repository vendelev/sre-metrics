package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"

	"go.uber.org/dig"

	appconfig "github.com/vendelev/sre-metrics/cmd/user-login-httpserver/config"
	"github.com/vendelev/sre-metrics/core/bootstrap"
	coreconfig "github.com/vendelev/sre-metrics/core/config"
	userlogin "github.com/vendelev/sre-metrics/user-login/bootstrap"
	userloginconfig "github.com/vendelev/sre-metrics/user-login/config"
)

// Run загружает настройки процесса, функционального модуля и core, собирает
// приложение и запускает его до отмены ctx или ошибки компонента. Успешная
// остановка дожидается запросов и обработки очереди; сигналы обрабатывает main.
func Run(ctx context.Context) error {
	process, err := appconfig.Load()
	if err != nil {
		return fmt.Errorf("конфигурация процесса: %w", err)
	}
	moduleCfg, err := userloginconfig.Load()
	if err != nil {
		return fmt.Errorf("конфигурация user-login: %w", err)
	}
	settings, err := coreconfig.Load()
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("service", "user-login")
	app, err := newApp(process, &moduleCfg, settings, logger, userlogin.RealTime{})
	if err != nil {
		return err
	}
	return app.run(ctx, nil)
}

// App владеет собранным приложением одного запуска: сервер и pipeline core,
// модуль user-login и настройки процесса. Сборка выполняется явной цепочкой
// конструкторов в контейнере; контейнер только собирает объекты. Поля после
// пустой строки — состояние протокола одного запуска: этот файл задаёт его
// порядок и очистку.
type App struct {
	server  *bootstrap.Server
	metrics *bootstrap.Metrics
	module  *userlogin.Module
	cfg     appconfig.Config
	logger  *slog.Logger

	ready    func(net.Addr)
	stopOnce sync.Once
	stopErr  error // Читается после stopOnce.Do, включая конкурентный повторный вызов.
}

// newApp собирает контейнер зависимостей: значения вызывающего кода,
// технические компоненты core, компоненты модуля и соединение handler модуля
// с общим сервером.
func newApp(
	process appconfig.Config,
	moduleCfg *userloginconfig.Config,
	settings coreconfig.HTTP,
	logger *slog.Logger,
	clock userlogin.Clock,
) (*App, error) {
	container := dig.New()
	if err := errors.Join(
		// Значения вызывающего кода.
		container.Provide(func() appconfig.Config { return process }),
		container.Provide(func() *userloginconfig.Config { return moduleCfg }),
		container.Provide(func() coreconfig.HTTP { return settings }),
		container.Provide(func() *slog.Logger { return logger }),
		container.Provide(func() userlogin.Clock { return clock }),
		// Технические компоненты core.
		bootstrap.Provide(container),
		container.Provide(func() bootstrap.Views {
			return bootstrap.Views{userlogin.HistogramView()}
		}),
		// Компоненты модуля: routerOf модуля предоставляет http.Handler
		// для общего сервера core.
		userlogin.Provide(container),
	); err != nil {
		return nil, err
	}
	var app *App
	if err := container.Invoke(func(
		server *bootstrap.Server,
		metrics *bootstrap.Metrics,
		module *userlogin.Module,
	) {
		app = &App{
			server:  server,
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

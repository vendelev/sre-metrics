package bootstrap

import (
	"errors"
	"log/slog"
	"net/http"

	"go.uber.org/dig"

	"github.com/vendelev/sre-metrics/core/config"
)

// Provide регистрирует конструкторы технических компонентов core в контейнере
// зависимостей. Модуль-потребитель добавляет в контейнер свои типизированные
// настройки, готовый handler, корневой logger и Views своих инструментов.
// Контейнер только собирает объекты: запуск и остановку ресурсов выполняет
// протокол запуска модуля, контейнер фоновых задач не запускает.
func Provide(container *dig.Container) error {
	return errors.Join(
		container.Provide(func(settings config.HTTP, handler http.Handler, logger *slog.Logger) *Server {
			// Журнал http.Server получает только технические ошибки компонента.
			return NewServer(settings, handler,
				slog.NewLogLogger(logger.With("component", "http").Handler(), slog.LevelError))
		}),
		container.Provide(func(logger *slog.Logger, views Views) *Metrics {
			return NewMetrics(logger.With("component", "otel"), views...)
		}),
	)
}

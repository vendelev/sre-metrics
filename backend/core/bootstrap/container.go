package bootstrap

import (
	"log/slog"

	"go.uber.org/dig"
)

// Provide регистрирует конструктор метрик core в контейнере зависимостей.
// Потребитель добавляет корневой logger и Views своих инструментов.
// Контейнер только собирает объекты: запуск и остановку ресурсов выполняет
// протокол запуска модуля, контейнер фоновых задач не запускает.
func Provide(container *dig.Container) error {
	return container.Provide(func(logger *slog.Logger, views Views) *Metrics {
		return NewMetrics(logger.With("component", "otel"), views...)
	})
}

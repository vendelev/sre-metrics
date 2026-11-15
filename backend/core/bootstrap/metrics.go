package bootstrap

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/vendelev/sre-metrics/core/internal/infrastructure/otel"
)

// Metrics владеет одним OTel pipeline процесса: provider, PeriodicReader
// и OTLP exporter. Функциональный модуль регистрирует собственные инструменты
// в Meter общего provider и сохраняет контроль над именами, атрибутами
// и cardinality своих метрик; глобальный provider не устанавливается.
type Metrics struct {
	pipeline *otel.Pipeline
}

// Views — набор View инструментов модулей-потребителей для общего provider.
// View применяются при создании provider.
type Views []sdkmetric.View

// NewMetrics собирает pipeline с View модулей-потребителей. View применяются
// при создании provider, поэтому передаются при сборке; ресурсы и фоновый
// сбор не открываются до Start.
func NewMetrics(logger *slog.Logger, views ...sdkmetric.View) *Metrics {
	return &Metrics{pipeline: otel.NewPipeline(views, logger)}
}

// Start создаёт OTLP exporter и provider с PeriodicReader. Resource и настройки
// экспорта SDK читает из стандартного OTEL-окружения.
func (m *Metrics) Start(ctx context.Context) error {
	return m.pipeline.Start(ctx)
}

// Meter возвращает meter общего provider для регистрации инструментов
// функционального модуля. До успешного Start возвращает nil.
func (m *Metrics) Meter(name string) metric.Meter {
	return m.pipeline.Meter(name)
}

// Shutdown останавливает reader, выполняет его последний экспорт
// и завершает exporter в пределах переданного бюджета. Отдельный резерв
// не создаётся; завершение обработки обеспечивает вызывающий. Повторный
// вызов возвращает результат первого, без новой отправки.
func (m *Metrics) Shutdown(ctx context.Context) error {
	return m.pipeline.Shutdown(ctx)
}

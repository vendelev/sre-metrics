// Package bootstrap собирает OTLP pipeline метрик без запуска ресурсов.
package bootstrap

import (
	"log/slog"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	pkgApi "github.com/vendelev/sre-metrics/pkg/otlpmetrics/api"
	iOtel "github.com/vendelev/sre-metrics/pkg/otlpmetrics/internal/infrastructure/otel"
)

// New собирает pipeline с View потребителя; запуск ресурсов выполняется в Start.
func New(logger *slog.Logger, views ...sdkmetric.View) pkgApi.Pipeline {
	return iOtel.NewPipeline(views, logger)
}

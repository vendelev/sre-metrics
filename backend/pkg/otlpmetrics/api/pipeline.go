// Package api определяет контракт жизненного цикла OTLP pipeline метрик.
package api

import (
	"context"

	"go.opentelemetry.io/otel/metric"
)

// Pipeline управляет сбором и экспортом метрик.
type Pipeline interface {
	Start(ctx context.Context) error
	Meter(name string) metric.Meter
	Shutdown(ctx context.Context) error
}

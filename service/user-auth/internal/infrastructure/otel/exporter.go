package otel

import (
	"context"
	"log/slog"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// sanitizingExporter скрывает потенциально чувствительный текст ошибки получателя,
// сохраняя причинную цепочку для errors.Is/As. Числовые данные не проверяются:
// контракт инструментов и корректность регистрации проверяются тестами
// (ревизия 2026-11-04 технического дополнения).
type sanitizingExporter struct {
	sdkmetric.Exporter
	logger *slog.Logger
}

func (e *sanitizingExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	if err := e.Exporter.Export(ctx, rm); err != nil {
		// Ответ получателя может содержать секреты. Не логируем body и сырую ошибку;
		// SDK получает безопасную ошибку, сохраняя причинную цепочку.
		e.logger.ErrorContext(ctx, "не удалось отправить OTLP метрики")
		return exportError{cause: err}
	}
	return nil
}

func (e *sanitizingExporter) Shutdown(ctx context.Context) error {
	if err := e.Exporter.Shutdown(ctx); err != nil {
		e.logger.ErrorContext(ctx, "не удалось завершить OTLP exporter")
		return exportError{cause: err}
	}
	return nil
}

type exportError struct{ cause error }

func (e exportError) Error() string { return "ошибка OTLP exporter" }
func (e exportError) Unwrap() error { return e.cause }

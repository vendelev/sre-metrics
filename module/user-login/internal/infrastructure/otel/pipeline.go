package otel

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// Pipeline владеет одним SDK и периодическим reader; HTTP-клиентом управляет exporter.
// Создание запускает goroutine; вызывающий обязан вызвать Shutdown.
type Pipeline struct {
	Recorder *Recorder
	once     sync.Once
	err      error
}

// StartPipeline активирует подготовленный Recorder, сохраняя Resource из окружения.
// Вызывается после bind, до первого обращения HTTP или обслуживания к Recorder.
// Владелец проверяет отмену после возврата и очищает результат в общем бюджете rollback.
func StartPipeline(
	ctx context.Context,
	prepared *Recorder,
	logger *slog.Logger,
) (*Pipeline, error) {
	if logger == nil {
		return nil, fmt.Errorf("создание OTLP pipeline: отсутствует логгер")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := prepared.checkPrepared(); err != nil {
		return nil, err
	}
	// Настройки экспорта, включая temporality, SDK читает из стандартного OTEL-окружения.
	exporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("создание OTLP exporter: %w", err)
	}
	checked := &sanitizingExporter{Exporter: exporter, logger: logger}
	reader := sdkmetric.NewPeriodicReader(checked)
	recorder, err := recorderWithReader(prepared.resource, reader)
	if err != nil {
		// recorderWithReader уже завершил provider и принадлежащий ему reader.
		return nil, err
	}
	*prepared = *recorder
	p := &Pipeline{Recorder: prepared}
	return p, nil
}

// Shutdown останавливает reader, выполняет его последний экспорт и завершает exporter.
// Бюджет задаёт вызывающий через контекст; отдельный резерв не создаётся.
// Завершение обработки обеспечивает вызывающий; без него пакет не окончательный.
// Повторный вызов возвращает результат первого, без новой отправки.
// Вызов с уже истёкшим контекстом передаёт команду освобождения ресурсов,
// но не гарантирует ни последней отправки, ни завершения внутренней очистки.
func (p *Pipeline) Shutdown(ctx context.Context) error {
	p.once.Do(func() {
		p.err = p.Recorder.Shutdown(ctx)
	})
	return p.err
}

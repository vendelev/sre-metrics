package otel

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
	"go.opentelemetry.io/otel/sdk/resource"
)

// Pipeline владеет одним SDK и периодическим reader; HTTP-клиентом управляет
// exporter. Конструктор только собирает объекты: Start создаёт exporter,
// provider и запускает goroutine периодического сбора. Вызывающий обязан
// вызвать Shutdown.
type Pipeline struct {
	views    []sdkmetric.View
	logger   *slog.Logger
	provider *sdkmetric.MeterProvider
	once     sync.Once
	err      error
}

// NewPipeline собирает pipeline с View модулей-потребителей. View применяются
// при создании provider, поэтому передаются до Start; ресурсы не открываются.
func NewPipeline(views []sdkmetric.View, logger *slog.Logger) *Pipeline {
	return &Pipeline{views: views, logger: logger}
}

// Start создаёт OTLP exporter и provider с PeriodicReader. Resource и настройки
// экспорта, включая temporality, SDK читает из стандартного OTEL-окружения;
// глобальный provider не устанавливается. Создание reader уже запускает
// goroutine: с успешного возврата владельцем pipeline является вызывающий.
// Повторный вызов после успешного запуска возвращает ошибку.
func (p *Pipeline) Start(ctx context.Context) error {
	if p.logger == nil {
		return fmt.Errorf("создание OTLP pipeline: отсутствует логгер")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.provider != nil {
		return fmt.Errorf("запуск OTLP pipeline: pipeline уже запущен")
	}
	exporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return fmt.Errorf("создание OTLP exporter: %w", err)
	}
	checked := &sanitizingExporter{Exporter: exporter, logger: p.logger}
	reader := sdkmetric.NewPeriodicReader(checked)
	p.provider = newProvider(resource.Environment(), reader, p.views)
	return ctx.Err()
}

// Meter возвращает meter общего provider для регистрации инструментов
// функционального модуля. До успешного Start возвращает nil.
func (p *Pipeline) Meter(name string) metric.Meter {
	if p.provider == nil {
		return nil
	}
	return p.provider.Meter(name)
}

// Shutdown останавливает reader, выполняет его последний экспорт и завершает exporter.
// Бюджет задаёт вызывающий через контекст; отдельный резерв не создаётся.
// Завершение обработки обеспечивает вызывающий; без него пакет не окончательный.
// Повторный вызов возвращает результат первого, без новой отправки.
// Вызов с уже истёкшим контекстом передаёт команду освобождения ресурсов,
// но не гарантирует ни последней отправки, ни завершения внутренней очистки.
func (p *Pipeline) Shutdown(ctx context.Context) error {
	p.once.Do(func() {
		if p.provider != nil {
			p.err = p.provider.Shutdown(ctx)
		}
	})
	return p.err
}

// newProvider создаёт provider с Resource окружения, переданным reader
// и View потребителей. Exemplars отключены общим механизмом: они могут
// переносить атрибуты запроса, а контроль над атрибутами метрик остаётся
// у функциональных модулей.
func newProvider(
	res *resource.Resource,
	reader sdkmetric.Reader,
	views []sdkmetric.View,
) *sdkmetric.MeterProvider {
	options := []sdkmetric.Option{
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(reader),
		sdkmetric.WithExemplarFilter(exemplar.AlwaysOffFilter),
	}
	for _, view := range views {
		options = append(options, sdkmetric.WithView(view))
	}
	return sdkmetric.NewMeterProvider(options...)
}

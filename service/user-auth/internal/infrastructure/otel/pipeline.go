package otel

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// PipelineSettings задаёт настройки экспорта; Resource уже задан в Recorder.
type PipelineSettings struct {
	Endpoint      string
	Interval      time.Duration
	ExportTimeout time.Duration
}

// Pipeline владеет одним SDK, периодическим reader и HTTP transport.
// Создание запускает goroutine; вызывающий обязан вызвать Shutdown.
type Pipeline struct {
	Recorder  *Recorder
	transport *http.Transport
	once      sync.Once
	err       error
}

// StartPipeline активирует подготовленный Recorder, сохраняя Resource и UUID.
// Вызывается после bind, до первого обращения HTTP или обслуживания к Recorder.
// Владелец проверяет отмену после возврата и очищает результат в общем бюджете rollback.
func StartPipeline(
	ctx context.Context,
	prepared *Recorder,
	settings PipelineSettings,
	logger *slog.Logger,
) (*Pipeline, error) {
	if logger == nil {
		return nil, fmt.Errorf("создание OTLP pipeline: отсутствует логгер")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ValidateEndpoint(settings.Endpoint); err != nil {
		return nil, err
	}
	if settings.Interval <= 0 || settings.ExportTimeout <= 0 {
		return nil, fmt.Errorf("создание OTLP pipeline: длительности должны быть положительными")
	}
	if err := prepared.checkPrepared(); err != nil {
		return nil, err
	}
	// Собственный transport позволяет освободить keep-alive соединения: Shutdown
	// официального exporter v1.47.0 лишь удаляет ссылки на HTTP-клиент.
	transport := &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		DialContext:       (&net.Dialer{Timeout: settings.ExportTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true, MaxIdleConns: 10, IdleConnTimeout: 90 * time.Second,
		TLSHandshakeTimeout: settings.ExportTimeout, ExpectContinueTimeout: time.Second,
	}
	client := &http.Client{Transport: transport, Timeout: settings.ExportTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	exporter, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpointURL(settings.Endpoint),
		otlpmetrichttp.WithHTTPClient(client),
		otlpmetrichttp.WithTimeout(settings.ExportTimeout),
		otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{Enabled: false}),
		otlpmetrichttp.WithTemporalitySelector(sdkmetric.CumulativeTemporalitySelector),
		otlpmetrichttp.WithAggregationSelector(sdkmetric.DefaultAggregationSelector),
	)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, fmt.Errorf("создание OTLP exporter: %w", err)
	}
	checked := &sanitizingExporter{Exporter: exporter, logger: logger}
	reader := sdkmetric.NewPeriodicReader(checked,
		sdkmetric.WithInterval(settings.Interval), sdkmetric.WithTimeout(settings.ExportTimeout))
	recorder, err := recorderWithReader(prepared.resource, reader)
	if err != nil {
		// recorderWithReader уже завершил provider и принадлежащий ему reader.
		transport.CloseIdleConnections()
		return nil, err
	}
	*prepared = *recorder
	p := &Pipeline{Recorder: prepared, transport: transport}
	return p, nil
}

// Shutdown останавливает reader, выполняет его последний экспорт и освобождает HTTP.
// Бюджет задаёт вызывающий через контекст; отдельный резерв не создаётся.
// Завершение обработки обеспечивает вызывающий; без него пакет не окончательный.
// Повторный вызов возвращает результат первого, без новой отправки.
// Вызов с уже истёкшим контекстом передаёт команду освобождения ресурсов,
// но не гарантирует ни последней отправки, ни завершения внутренней очистки.
func (p *Pipeline) Shutdown(ctx context.Context) error {
	p.once.Do(func() {
		p.err = p.Recorder.Shutdown(ctx)
		p.transport.CloseIdleConnections()
	})
	return p.err
}

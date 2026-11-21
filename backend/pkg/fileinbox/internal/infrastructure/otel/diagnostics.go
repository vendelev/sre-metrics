// Package otel реализует диагностические инструменты файлового входа.
package otel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	dContract "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/domain/contract"
	dDto "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/domain/dto"
)

const (
	eventsName      = "file.inbox.events"
	filesName       = "file.inbox.files"
	sizeName        = "file.inbox.size"
	oldestAgeName   = "file.inbox.oldest_file.age"
	volumeUsageName = "file.inbox.volume.usage"
)

// Diagnostics записывает исходы обработки и состояние каталога через OpenTelemetry.
type Diagnostics struct {
	events      metric.Int64Counter
	files       metric.Int64Gauge
	size        metric.Int64Gauge
	oldestAge   metric.Float64Gauge
	volumeUsage metric.Float64Gauge
}

var _ dContract.Diagnostics = (*Diagnostics)(nil)

// New регистрирует инструменты и пять нулевых исходов до запуска обработки.
func New(ctx context.Context, meter metric.Meter) (*Diagnostics, error) {
	if meter == nil {
		return nil, errors.New("для диагностики файлового входа требуется meter")
	}
	d, err := newDiagnostics(meter)
	if err != nil {
		return nil, err
	}
	for _, value := range []dContract.Outcome{
		dContract.Accepted,
		dContract.InvalidEvent,
		dContract.IOError,
		dContract.UnlinkFailed,
		dContract.Retry,
	} {
		d.Add(ctx, value, 0)
	}
	return d, nil
}

func newDiagnostics(meter metric.Meter) (*Diagnostics, error) {
	events, err := meter.Int64Counter(
		eventsName, metric.WithUnit("1"),
		metric.WithDescription("Исходы обработки публикаций и сбои операций файлового входа"),
	)
	if err != nil {
		return nil, fmt.Errorf("создание счётчика исходов файлового входа: %w", err)
	}
	files, err := meter.Int64Gauge(
		filesName, metric.WithUnit("{file}"),
		metric.WithDescription("Число обычных файлов .json в каталоге входа до обработки пачки"),
	)
	if err != nil {
		return nil, fmt.Errorf("создание метрики числа файлов входа: %w", err)
	}
	size, err := meter.Int64Gauge(
		sizeName, metric.WithUnit("By"),
		metric.WithDescription("Сумма логических размеров обычных файлов .json в каталоге входа"),
	)
	if err != nil {
		return nil, fmt.Errorf("создание метрики размера файлов входа: %w", err)
	}
	oldestAge, err := meter.Float64Gauge(
		oldestAgeName, metric.WithUnit("s"),
		metric.WithDescription("Возраст самого старого обычного файла .json по mtime"),
	)
	if err != nil {
		return nil, fmt.Errorf("создание метрики возраста файлов входа: %w", err)
	}
	volumeUsage, err := meter.Float64Gauge(
		volumeUsageName, metric.WithUnit("1"),
		metric.WithDescription("Доля занятых блоков всей файловой системы тома, а не только каталога входа"),
	)
	if err != nil {
		return nil, fmt.Errorf("создание метрики заполнения тома файлового входа: %w", err)
	}
	return &Diagnostics{
		events: events, files: files, size: size, oldestAge: oldestAge,
		volumeUsage: volumeUsage,
	}, nil
}

// Add записывает один вид исхода в общий счётчик диагностики.
func (d *Diagnostics) Add(ctx context.Context, value dContract.Outcome, count int64) {
	d.events.Add(ctx, count, metric.WithAttributes(attribute.String("outcome", string(value))))
}

// ObserveDirectory записывает состояние обычных публикаций до обработки пачки.
func (d *Diagnostics) ObserveDirectory(ctx context.Context, snapshot dDto.DirectorySnapshot) {
	d.files.Record(ctx, snapshot.Files)
	d.size.Record(ctx, snapshot.Bytes)
	var age float64
	if snapshot.Files != 0 {
		// mtime задаёт файловая система; сравниваем с системным временем,
		// независимо от предметных часов Handle.
		age = max(0, time.Since(snapshot.OldestTime).Seconds())
	}
	d.oldestAge.Record(ctx, age)
}

// ObserveVolume записывает долю занятых блоков тома.
func (d *Diagnostics) ObserveVolume(ctx context.Context, volume float64) {
	d.volumeUsage.Record(ctx, volume)
}

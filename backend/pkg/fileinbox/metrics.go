package fileinbox

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type outcome string

const (
	accepted     outcome = "accepted"
	invalidEvent outcome = "invalid_event"
	ioError      outcome = "io_error"
	unlinkFailed outcome = "unlink_failed"
	retry        outcome = "retry"
)

const (
	eventsName      = "file.inbox.events"
	filesName       = "file.inbox.files"
	sizeName        = "file.inbox.size"
	oldestAgeName   = "file.inbox.oldest_file.age"
	volumeUsageName = "file.inbox.volume.usage"
)

type diagnostics struct {
	events      metric.Int64Counter
	files       metric.Int64Gauge
	size        metric.Int64Gauge
	oldestAge   metric.Float64Gauge
	volumeUsage metric.Float64Gauge
}

// ActivateMetrics регистрирует диагностику в meter владельца. Вызывается
// однократно, до Run; без вызова файловый вход работает без метрик.
func (i *Inbox) ActivateMetrics(ctx context.Context, meter metric.Meter) error {
	if meter == nil {
		return errors.New("для диагностики файлового входа требуется meter")
	}
	if i.scanner.diagnostics != nil || i.started.Load() {
		return errors.New("диагностику файлового входа можно активировать один раз до запуска")
	}
	d, err := newDiagnostics(meter)
	if err != nil {
		return err
	}
	for _, value := range []outcome{accepted, invalidEvent, ioError, unlinkFailed, retry} {
		d.add(ctx, value, 0)
	}
	i.scanner.diagnostics = d
	return nil
}

func newDiagnostics(meter metric.Meter) (*diagnostics, error) {
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
	volumeUsage, err := newVolumeGauge(meter)
	if err != nil {
		return nil, fmt.Errorf("создание метрики заполнения тома файлового входа: %w", err)
	}
	return &diagnostics{events: events, files: files, size: size, oldestAge: oldestAge, volumeUsage: volumeUsage}, nil
}

func (d *diagnostics) add(ctx context.Context, value outcome, count int64) {
	if d != nil {
		d.events.Add(ctx, count, metric.WithAttributes(attribute.String("outcome", string(value))))
	}
}

type directorySnapshot struct {
	files  int64
	bytes  int64
	oldest time.Time
}

func (s *directorySnapshot) include(info os.FileInfo) {
	if s.files == 0 || info.ModTime().Before(s.oldest) {
		s.oldest = info.ModTime()
	}
	s.files++
	// Логический размер разреженных файлов может превышать ёмкость ФС.
	s.bytes += min(info.Size(), math.MaxInt64-s.bytes)
}

func (d *diagnostics) observeDirectory(ctx context.Context, snapshot directorySnapshot) {
	d.files.Record(ctx, snapshot.files)
	d.size.Record(ctx, snapshot.bytes)
	var age float64
	if snapshot.files != 0 {
		// mtime задаёт файловая система; сравниваем с системным временем,
		// независимо от предметных часов Handle.
		age = max(0, time.Since(snapshot.oldest).Seconds())
	}
	d.oldestAge.Record(ctx, age)
}

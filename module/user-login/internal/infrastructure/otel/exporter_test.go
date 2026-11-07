package otel

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
)

type controlledExporter struct {
	exports   atomic.Int64
	active    atomic.Int64
	maxActive atomic.Int64
	closed    atomic.Bool
	gate      <-chan struct{}
	err       error
}

func (*controlledExporter) Temporality(sdkmetric.InstrumentKind) metricdata.Temporality {
	return metricdata.CumulativeTemporality
}
func (*controlledExporter) Aggregation(k sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	return sdkmetric.DefaultAggregationSelector(k)
}
func (e *controlledExporter) Export(ctx context.Context, _ *metricdata.ResourceMetrics) error {
	e.exports.Add(1)
	active := e.active.Add(1)
	for previous := e.maxActive.Load(); active > previous; previous = e.maxActive.Load() {
		if e.maxActive.CompareAndSwap(previous, active) {
			break
		}
	}
	defer e.active.Add(-1)
	if e.gate != nil {
		select {
		case <-e.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return e.err
}
func (*controlledExporter) ForceFlush(ctx context.Context) error { return ctx.Err() }
func (e *controlledExporter) Shutdown(ctx context.Context) error {
	e.closed.Store(true)
	return errors.Join(e.err, ctx.Err())
}

func TestSanitizingExporterSafeErrors(t *testing.T) {
	cause := errors.New("synthetic-secret")
	var logs bytes.Buffer
	e := sanitizingExporter{
		Exporter: &controlledExporter{err: cause},
		logger:   slog.New(slog.NewTextHandler(&logs, nil)),
	}
	for _, err := range []error{e.Export(t.Context(), &metricdata.ResourceMetrics{}), e.Shutdown(t.Context())} {
		if !errors.Is(err, cause) || strings.Contains(err.Error(), cause.Error()) {
			t.Fatalf("небезопасная ошибка: %v", err)
		}
	}
	if strings.Contains(logs.String(), cause.Error()) {
		t.Fatal("секрет в диагностике")
	}
}

func TestPeriodicReaderOwnership(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		exporter := &controlledExporter{gate: gate}
		reader := sdkmetric.NewPeriodicReader(
			&sanitizingExporter{Exporter: exporter, logger: testLogger()},
			sdkmetric.WithInterval(time.Second),
			sdkmetric.WithTimeout(10*time.Second),
		)
		cleanupReader(t, reader)
		res := resource.Environment()
		r, err := recorderWithReader(res, reader)
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if exporter.exports.Load() != 0 {
			t.Fatal("экспорт до первого интервала")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if exporter.exports.Load() != 1 || exporter.active.Load() != 1 {
			t.Fatal("первый период не запущен")
		}
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if exporter.exports.Load() != 1 || exporter.maxActive.Load() != 1 {
			t.Fatal("перекрывающиеся отправки")
		}
		close(gate)
		synctest.Wait()
		before := exporter.exports.Load()
		time.Sleep(time.Second)
		synctest.Wait()
		if exporter.exports.Load() <= before || exporter.maxActive.Load() != 1 {
			t.Fatal("периодичность не восстановилась")
		}
		before = exporter.exports.Load()
		if err := r.Shutdown(t.Context()); err != nil {
			t.Fatal(err)
		}
		if exporter.exports.Load() != before+1 || !exporter.closed.Load() || exporter.active.Load() != 0 {
			t.Fatal("нет финального экспорта/очистки")
		}
		before = exporter.exports.Load()
		time.Sleep(time.Hour)
		synctest.Wait()
		if exporter.exports.Load() != before {
			t.Fatal("экспорт после shutdown")
		}
		if err := reader.Collect(
			t.Context(),
			new(metricdata.ResourceMetrics),
		); !errors.Is(
			err,
			sdkmetric.ErrReaderShutdown,
		) {
			t.Fatalf("reader не остановлен: %v", err)
		}
	})
}

func TestPeriodicReaderSDKEnvironment(t *testing.T) {
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "1000")
	t.Setenv("OTEL_METRIC_EXPORT_TIMEOUT", "200")
	synctest.Test(t, func(t *testing.T) {
		exporter := &controlledExporter{gate: make(chan struct{})}
		reader := sdkmetric.NewPeriodicReader(exporter)
		cleanupReader(t, reader)
		res := resource.Environment()
		if _, err := recorderWithReader(res, reader); err != nil {
			t.Fatal(err)
		}
		time.Sleep(999 * time.Millisecond)
		synctest.Wait()
		if exporter.exports.Load() != 0 {
			t.Fatal("экспорт раньше настроенного интервала")
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if exporter.exports.Load() != 1 || exporter.active.Load() != 1 {
			t.Fatal("интервал из окружения SDK не применён")
		}
		time.Sleep(199 * time.Millisecond)
		synctest.Wait()
		if exporter.active.Load() != 1 {
			t.Fatal("экспорт отменён раньше timeout")
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if exporter.active.Load() != 0 {
			t.Fatal("timeout из окружения SDK не применён")
		}
	})
}

func TestPeriodicReaderShutdownCancelsInflight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exporter := &controlledExporter{gate: make(chan struct{})}
		reader := sdkmetric.NewPeriodicReader(
			&sanitizingExporter{Exporter: exporter, logger: testLogger()},
			sdkmetric.WithInterval(time.Second),
			sdkmetric.WithTimeout(time.Minute),
		)
		cleanupReader(t, reader)
		res := resource.Environment()
		r, err := recorderWithReader(res, reader)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if exporter.active.Load() != 1 {
			t.Fatal("нет текущего экспорта")
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := r.Shutdown(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("ошибка завершения: %v", err)
		}
		if !exporter.closed.Load() || exporter.active.Load() != 0 || exporter.exports.Load() != 1 {
			t.Fatal("текущий экспорт не отменён или запущен новый")
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		if exporter.exports.Load() != 1 {
			t.Fatal("экспорт после завершения")
		}
	})
}

func cleanupReader(t *testing.T, reader *sdkmetric.PeriodicReader) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// Страховка при Fatal до штатного Shutdown: без нового финального экспорта.
		if err := reader.Shutdown(
			ctx,
		); err != nil && !errors.Is(err, context.Canceled) &&
			!errors.Is(err, sdkmetric.ErrReaderShutdown) {
			t.Error(err)
		}
	})
}

func TestPipelineShutdownInflight(t *testing.T) {
	// Проверяем вызовы Export, а не число серверных HTTP-handlers: после отмены
	// клиентского вызова получатель ещё может завершать свой обработчик.
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		exporter := &controlledExporter{gate: gate}
		checked := &sanitizingExporter{Exporter: exporter, logger: testLogger()}
		reader := sdkmetric.NewPeriodicReader(
			checked,
			sdkmetric.WithInterval(time.Second),
			sdkmetric.WithTimeout(10*time.Second),
		)
		cleanupReader(t, reader)
		res := resource.Environment()
		r, err := recorderWithReader(res, reader)
		if err != nil {
			t.Fatal(err)
		}
		p := &Pipeline{Recorder: r}
		time.Sleep(time.Second)
		synctest.Wait()
		if exporter.exports.Load() != 1 || exporter.active.Load() != 1 {
			t.Fatal("нет текущей периодической отправки")
		}
		done := make(chan error, 1)
		go func() { done <- p.Shutdown(t.Context()) }()
		synctest.Wait()
		if exporter.exports.Load() != 2 || exporter.active.Load() != 1 {
			t.Fatal("последний экспорт не начался после отмены периодического")
		}
		close(gate)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if exporter.maxActive.Load() != 1 || !exporter.closed.Load() || exporter.active.Load() != 0 {
			t.Fatal("перекрытие или незавершённый exporter")
		}
		before := exporter.exports.Load()
		if err := p.Shutdown(t.Context()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		if exporter.exports.Load() != before {
			t.Fatal("отправки после закрытия pipeline")
		}
	})
}

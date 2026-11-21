//go:build linux

package otel_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	pkgClock "github.com/vendelev/sre-metrics/pkg/clock"
	pkgApi "github.com/vendelev/sre-metrics/pkg/fileinbox/api"
	aUsecase "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/application/usecase"
	iFilesystem "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/infrastructure/filesystem"
	iOtel "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/infrastructure/otel"
)

const (
	fileA           = "00000000-0000-4000-8000-00000000000a.json"
	fileB           = "00000000-0000-4000-8000-00000000000b.json"
	fileC           = "00000000-0000-4000-8000-00000000000c.json"
	eventsName      = "file.inbox.events"
	filesName       = "file.inbox.files"
	sizeName        = "file.inbox.size"
	oldestAgeName   = "file.inbox.oldest_file.age"
	volumeUsageName = "file.inbox.volume.usage"
)

func TestDiagnosticsRecordsOutcomesAndDirectoryBeforeBatch(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t.TempDir())
	cfg.MaxFilesPerScan = 1
	cfg.MaxFileBytes = 8
	attempt := 0
	s := newTestScanner(cfg, func(contents []byte, _ time.Time) error {
		switch string(contents) {
		case "retry":
			attempt++
			if attempt == 1 {
				return fmt.Errorf("синтетическая задержка: %w", pkgApi.ErrRetry)
			}
			return nil
		case "bad":
			return errors.New("синтетический секрет в ошибке обработчика")
		default:
			t.Fatalf("лишний файл передан обработчику: %q", contents)
			return nil
		}
	})
	reader := attachDiagnostics(t, s)
	assertOutcomes(t, collectDiagnostics(t, reader), nil)

	publish(t, cfg.Dir, fileA, "retry")
	publish(t, cfg.Dir, fileB, "bad")
	publish(t, cfg.Dir, fileC, "too-large")
	publish(t, cfg.Dir, "synthetic-private.json", "str")
	writeFile(t, filepath.Join(cfg.Dir, "ignored.tmp"), "не входит в снимок")
	if err := os.Mkdir(filepath.Join(cfg.Dir, "dir.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fileA, filepath.Join(cfg.Dir, "link.json")); err != nil {
		t.Fatal(err)
	}
	oldest := time.Now().Truncate(time.Millisecond).Add(-2 * time.Minute)
	setMtime(t, filepath.Join(cfg.Dir, fileA), oldest)
	setMtime(t, filepath.Join(cfg.Dir, fileB), oldest.Add(time.Minute))
	setMtime(t, filepath.Join(cfg.Dir, fileC), oldest.Add(3*time.Minute))
	setMtime(t, filepath.Join(cfg.Dir, "synthetic-private.json"), oldest.Add(4*time.Minute))

	earliest := time.Since(oldest).Seconds()
	scanDir(t, s)
	m := collectDiagnostics(t, reader)
	assertOutcomes(t, m, map[string]int64{"retry": 1})
	assertDirectoryTotals(t, m, 4, 20)
	if age := gaugeValue[float64](t, m[oldestAgeName], "s"); age < earliest || age > time.Since(oldest).Seconds() {
		t.Errorf("лаг самого старого файла: %v с", age)
	}
	requirePresent(t, filepath.Join(cfg.Dir, fileA))

	for range 4 {
		scanDir(t, s)
	}
	m = collectDiagnostics(t, reader)
	assertOutcomes(t, m, map[string]int64{"retry": 1, "accepted": 1, "invalid_event": 3})
	assertDirectoryTotals(t, m, 1, 3)
	if age := gaugeValue[float64](t, m[oldestAgeName], "s"); age != 0 {
		t.Errorf("mtime в будущем: лаг %v вместо 0", age)
	}

	scanDir(t, s)
	m = collectDiagnostics(t, reader)
	assertOutcomes(t, m, map[string]int64{"retry": 1, "accepted": 1, "invalid_event": 3})
	assertDirectoryTotals(t, m, 0, 0)
	if age := gaugeValue[float64](t, m[oldestAgeName], "s"); age != 0 {
		t.Errorf("пустой каталог: лаг %v вместо 0", age)
	}
}

func TestDiagnosticsVolumeIncludesTmpFilesAndSiblingDirectories(t *testing.T) {
	var stat syscall.Statfs_t
	const tmpfsMagic = 0x01021994
	const chunkSize = 4 << 20
	if err := syscall.Statfs("/dev/shm", &stat); err != nil || stat.Type != tmpfsMagic {
		t.Skip("проверка заполнения требует tmpfs в /dev/shm")
	}
	if stat.Bsize < 1 || stat.Bavail < uint64(2*chunkSize)/uint64(stat.Bsize)+2 {
		t.Skip("в тестовом tmpfs недостаточно свободного места")
	}
	root, err := os.MkdirTemp("/dev/shm", "fileinbox-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cleanupErr := os.RemoveAll(root); cleanupErr != nil {
			t.Error(cleanupErr)
		}
	})
	cfg := testConfig(filepath.Join(root, "inbox"))
	sibling := filepath.Join(root, "sibling")
	for _, dir := range []string{cfg.Dir, sibling} {
		if mkdirErr := os.Mkdir(dir, 0o700); mkdirErr != nil {
			t.Fatal(mkdirErr)
		}
	}
	s := newTestScanner(cfg, func([]byte, time.Time) error { return nil })
	reader := attachDiagnostics(t, s)
	measure := func() float64 {
		scanDir(t, s)
		m := collectDiagnostics(t, reader)
		assertDirectoryTotals(t, m, 0, 0)
		return gaugeValue[float64](t, m[volumeUsageName], "1")
	}
	before := measure()
	data := bytes.Repeat([]byte("x"), chunkSize)
	for _, dir := range []string{cfg.Dir, sibling} {
		path := filepath.Join(dir, "writing.tmp")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		after := measure()
		if after <= before || after > 1 {
			t.Fatalf("запись вне логического inbox не увеличила заполнение тома: %v → %v", before, after)
		}
		before = after
	}
}

func newTestScanner(cfg scannerConfig, handle pkgApi.Handle) *aUsecase.Scanner {
	logger := slog.New(slog.DiscardHandler)
	return aUsecase.New(iFilesystem.New(cfg.Dir), handle, cfg.Limits, pkgClock.RealTime{}, logger)
}

func attachDiagnostics(t *testing.T, s *aUsecase.Scanner) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	diagnostics, err := iOtel.New(t.Context(), provider.Meter("fileinbox-test"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetDiagnostics(diagnostics)
	return reader
}

func scanDir(t *testing.T, s *aUsecase.Scanner) {
	t.Helper()
	entries, err := s.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s.Scan(t.Context(), entries)
}

type scannerConfig struct {
	Dir string
	aUsecase.Limits
}

func testConfig(dir string) scannerConfig {
	return scannerConfig{
		Dir:    dir,
		Limits: aUsecase.Limits{MaxFilesPerScan: 12, MaxFileBytes: 1024, MaxAttempts: 2},
	}
}

func publish(t *testing.T, dir, name, contents string) {
	t.Helper()
	tmp := filepath.Join(dir, name+".tmp")
	writeFile(t, tmp, contents)
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func requirePresent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("ожидалось наличие %q: %v", path, err)
	}
}

func setMtime(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func collectDiagnostics(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var data metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.ScopeMetrics) != 1 {
		t.Fatalf("ожидалась одна область диагностики: %v", data.ScopeMetrics)
	}
	metrics := make(map[string]metricdata.Metrics, len(data.ScopeMetrics[0].Metrics))
	for _, metric := range data.ScopeMetrics[0].Metrics {
		metrics[metric.Name] = metric
	}
	return metrics
}

func assertOutcomes(t *testing.T, metrics map[string]metricdata.Metrics, want map[string]int64) {
	t.Helper()
	m := metrics[eventsName]
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok || !sum.IsMonotonic || sum.Temporality != metricdata.CumulativeTemporality || m.Unit != "1" {
		t.Fatalf("неверный контракт счётчика %s: %+v", eventsName, m)
	}
	remaining := map[string]bool{
		"accepted":      true,
		"invalid_event": true,
		"io_error":      true,
		"unlink_failed": true,
		"retry":         true,
	}
	for _, point := range sum.DataPoints {
		attr, found := point.Attributes.Value("outcome")
		value := attr.AsString()
		if point.Attributes.Len() != 1 || !found || !remaining[value] || len(point.Exemplars) != 0 {
			t.Fatalf("неожиданный ряд счётчика: %+v", point)
		}
		delete(remaining, value)
		if point.Value != want[value] {
			t.Errorf("исход %s: %d, ожидалось %d", value, point.Value, want[value])
		}
	}
	if len(remaining) != 0 {
		t.Errorf("нет рядов для исходов: %v", remaining)
	}
}

func assertDirectoryTotals(t *testing.T, m map[string]metricdata.Metrics, files, size int64) {
	t.Helper()
	if got := gaugeValue[int64](t, m[filesName], "{file}"); got != files {
		t.Errorf("число файлов: %d, ожидалось %d", got, files)
	}
	if got := gaugeValue[int64](t, m[sizeName], "By"); got != size {
		t.Errorf("размер файлов: %d, ожидалось %d", got, size)
	}
}

func gaugeValue[N int64 | float64](t *testing.T, m metricdata.Metrics, unit string) N {
	t.Helper()
	gauge, ok := m.Data.(metricdata.Gauge[N])
	if !ok || m.Unit != unit || len(gauge.DataPoints) != 1 {
		t.Fatalf("неверный контракт Gauge: %+v", m)
	}
	point := gauge.DataPoints[0]
	if point.Attributes.Len() != 0 || len(point.Exemplars) != 0 {
		t.Fatalf("неожиданные атрибуты Gauge: %+v", point)
	}
	return point.Value
}

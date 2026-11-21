//go:build linux

package bootstrap_test

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	pkgClock "github.com/vendelev/sre-metrics/pkg/clock"
	pkgApi "github.com/vendelev/sre-metrics/pkg/fileinbox/api"
	pkgBootstrap "github.com/vendelev/sre-metrics/pkg/fileinbox/bootstrap"
	pkgConfig "github.com/vendelev/sre-metrics/pkg/fileinbox/config"
	pkgTask "github.com/vendelev/sre-metrics/pkg/task"
)

const (
	fileA       = "00000000-0000-4000-8000-00000000000a.json"
	fileB       = "00000000-0000-4000-8000-00000000000b.json"
	testTimeout = 3 * time.Second
)

func TestNewRejectsInvalidInputs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(*pkgConfig.Config)
	}{
		{name: "каталог", edit: func(cfg *pkgConfig.Config) { cfg.Dir = "  " }},
		{name: "период", edit: func(cfg *pkgConfig.Config) { cfg.ScanInterval = 0 }},
		{name: "число файлов", edit: func(cfg *pkgConfig.Config) { cfg.MaxFilesPerScan = 0 }},
		{name: "размер", edit: func(cfg *pkgConfig.Config) { cfg.MaxFileBytes = -1 }},
		{name: "попытки", edit: func(cfg *pkgConfig.Config) { cfg.MaxAttempts = 0 }},
	}
	handle := func([]byte, time.Time) error { return nil }
	logger := slog.New(slog.DiscardHandler)
	clk := pkgClock.RealTime{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig(t.TempDir())
			tt.edit(&cfg)
			if _, err := pkgBootstrap.New(cfg, handle, logger, clk); err == nil {
				t.Fatal("некорректная конфигурация принята")
			}
		})
	}
	cfg := testConfig(t.TempDir())
	if _, err := pkgBootstrap.New(cfg, nil, logger, clk); err == nil {
		t.Error("принят отсутствующий обработчик")
	}
	if _, err := pkgBootstrap.New(cfg, handle, nil, clk); err == nil {
		t.Error("принят отсутствующий журнал")
	}
	if _, err := pkgBootstrap.New(cfg, handle, logger, nil); err == nil {
		t.Error("приняты отсутствующие часы")
	}
}

func TestRunCreatesDirectoryAndWatchesRename(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "inbox")
	cfg := testConfig(dir)
	cfg.ScanInterval = time.Hour
	got := make(chan string, 1)
	inbox := newTestInbox(t, cfg, func(contents []byte, _ time.Time) error {
		select {
		case got <- string(contents):
		default:
		}
		return nil
	})

	requireMissing(t, dir)
	running := runTask(t, inbox)
	awaitReady(t, inbox, running)
	publish(t, dir, fileA, "синтетическая публикация")
	select {
	case contents := <-got:
		if contents != "синтетическая публикация" {
			t.Fatalf("неожиданное содержимое: %q", contents)
		}
	case <-time.After(testTimeout):
		t.Fatal("rename не вызвал обработку до первого периодического сканирования")
	}
	inbox.Stop()
	inbox.Stop()
	requireStopped(t, running)
	requireMissing(t, filepath.Join(dir, fileA))
	if err := inbox.Run(); err == nil {
		t.Fatal("повторный запуск не отклонён")
	}
}

func TestRunStartupFailureDoesNotSignalReady(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "not-a-directory")
	writeFile(t, path, "синтетический файл")
	i := newTestInbox(t, testConfig(path), nil)
	if err := i.Run(); err == nil {
		t.Fatal("не удалось обнаружить, что путь занят файлом")
	}
	requireNotReady(t, i)
}

func TestRunSignalsReadyBeforeHandlingStartupBatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := testConfig(dir)
	cfg.ScanInterval = time.Hour
	publish(t, dir, fileA, "публикация до запуска")
	entered := make(chan struct{})
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	i := newTestInbox(t, cfg, func([]byte, time.Time) error {
		close(entered)
		<-release
		return nil
	})
	running := runTask(t, i)
	select {
	case <-entered:
	case <-time.After(testTimeout):
		t.Fatal("стартовая публикация не передана обработчику")
	}
	select {
	case <-i.Ready():
	default:
		t.Fatal("обработчик вызван до объявления готовности")
	}
	unblock()
	requireEventuallyMissing(t, filepath.Join(dir, fileA))
	i.Stop()
	requireStopped(t, running)
}

func TestRunRejectsInaccessibleDirectory(t *testing.T) {
	t.Parallel()
	requireUnprivileged(t)
	dir := t.TempDir()
	setMode(t, dir, 0o500)
	t.Cleanup(func() { setMode(t, dir, 0o700) })
	i := newTestInbox(t, testConfig(dir), nil)
	if err := i.Run(); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("не обнаружен недоступный для удаления каталог: %v", err)
	}
	requireNotReady(t, i)
}

func TestRunFailsWhenDirectoryDisappearsAfterReady(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "inbox")
	cfg := testConfig(dir)
	cfg.ScanInterval = 10 * time.Millisecond
	i := newTestInbox(t, cfg, nil)
	running := pkgTask.New()
	go func() { running.Complete(i.Run()) }()
	t.Cleanup(func() {
		i.Stop()
		select {
		case <-running.Done():
		case <-time.After(testTimeout):
			t.Error("Run не завершился после Stop")
		}
	})
	awaitReady(t, i, running)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	select {
	case <-running.Done():
		if err := running.Err(); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Run не вернул ошибку исчезнувшего каталога: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Run не завершился после удаления каталога")
	}
}

func TestRunCanBeStoppedBeforeStart(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "inbox")
	i := newTestInbox(t, testConfig(dir), nil)
	i.Stop()
	if err := i.Run(); err != nil {
		t.Fatalf("ошибка штатной отмены: %v", err)
	}
	requireMissing(t, dir)
	requireNotReady(t, i)
}

func TestRunRescansPeriodically(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t.TempDir())
	cfg.ScanInterval = 10 * time.Millisecond
	cfg.MaxFilesPerScan = 1
	// Обе публикации появляются до наблюдения, удаление не запускает новый скан.
	publish(t, cfg.Dir, fileA, "синтетическая публикация")
	publish(t, cfg.Dir, fileB, "вторая синтетическая публикация")
	i := newTestInbox(t, cfg, nil)
	runTask(t, i)
	requireEventuallyMissing(t, filepath.Join(cfg.Dir, fileA))
	requireEventuallyMissing(t, filepath.Join(cfg.Dir, fileB))
}

func TestActivateMetricsInitializesPublicInstruments(t *testing.T) {
	t.Parallel()
	i := newTestInbox(t, testConfig(t.TempDir()), nil)
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		if err := provider.Shutdown(t.Context()); err != nil {
			t.Error(err)
		}
	})
	meter := provider.Meter("fileinbox-test")
	if err := i.ActivateMetrics(t.Context(), nil); err == nil {
		t.Fatal("отсутствующий meter не отклонён")
	}
	if err := i.ActivateMetrics(t.Context(), meter); err != nil {
		t.Fatal(err)
	}
	if err := i.ActivateMetrics(t.Context(), meter); err == nil {
		t.Fatal("повторная активация не отклонена")
	}
	var data metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.ScopeMetrics) != 1 {
		t.Fatalf("ожидалась одна область диагностики: %v", data.ScopeMetrics)
	}
	if len(data.ScopeMetrics[0].Metrics) != 1 || data.ScopeMetrics[0].Metrics[0].Name != "file.inbox.events" {
		t.Fatalf("не инициализирован счётчик исходов: %v", data.ScopeMetrics[0].Metrics)
	}
	i.Stop()
	if err := i.Run(); err != nil {
		t.Fatal(err)
	}
	if err := i.ActivateMetrics(t.Context(), meter); err == nil {
		t.Fatal("активация после запуска не отклонена")
	}
}

func testConfig(dir string) pkgConfig.Config {
	return pkgConfig.Config{
		Dir:             dir,
		ScanInterval:    time.Second,
		MaxFilesPerScan: 12,
		MaxFileBytes:    1024,
		MaxAttempts:     2,
	}
}

func newTestInbox(t *testing.T, cfg pkgConfig.Config, handle pkgApi.Handle) pkgApi.Inbox {
	t.Helper()
	if handle == nil {
		handle = func([]byte, time.Time) error { return nil }
	}
	logger := slog.New(slog.DiscardHandler)
	inbox, err := pkgBootstrap.New(cfg, handle, logger, pkgClock.RealTime{})
	if err != nil {
		t.Fatal(err)
	}
	return inbox
}

func runTask(t *testing.T, inbox pkgApi.Inbox) *pkgTask.Task {
	t.Helper()
	running := pkgTask.New()
	go func() { running.Complete(inbox.Run()) }()
	t.Cleanup(func() {
		inbox.Stop()
		requireStopped(t, running)
	})
	return running
}

func awaitReady(t *testing.T, inbox pkgApi.Inbox, running *pkgTask.Task) {
	t.Helper()
	select {
	case <-inbox.Ready():
	case <-running.Done():
		t.Fatalf("Run завершился до готовности: %v", running.Err())
	case <-time.After(testTimeout):
		t.Fatal("файловый вход не сообщил о готовности")
	}
}

func requireStopped(t *testing.T, running *pkgTask.Task) {
	t.Helper()
	select {
	case <-running.Done():
		if err := running.Err(); err != nil {
			t.Fatalf("файловый вход завершился с ошибкой: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("файловый вход не завершился после Stop")
	}
}

func requireNotReady(t *testing.T, inbox pkgApi.Inbox) {
	t.Helper()
	select {
	case <-inbox.Ready():
		t.Fatal("объявлена готовность незапущенного файлового входа")
	default:
	}
}

func requireUnprivileged(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("сценарий требует непривилегированного пользователя")
	}
}

func setMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
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

func requireMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ожидалось отсутствие %q, ошибка Lstat: %v", path, err)
	}
}

func requireEventuallyMissing(t *testing.T, path string) {
	t.Helper()
	deadline := time.NewTimer(testTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-deadline.C:
			t.Fatalf("не дождались удаления %q", path)
		case <-ticker.C:
		}
	}
}

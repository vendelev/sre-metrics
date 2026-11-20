//go:build linux

package fileinbox

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/vendelev/sre-metrics/pkg/clock"
	"github.com/vendelev/sre-metrics/pkg/task"
)

const (
	fileA       = "00000000-0000-4000-8000-00000000000a.json"
	fileB       = "00000000-0000-4000-8000-00000000000b.json"
	fileC       = "00000000-0000-4000-8000-00000000000c.json"
	testTimeout = 3 * time.Second
)

func TestNewRejectsInvalidInputs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(*Config)
	}{
		{name: "каталог", edit: func(cfg *Config) { cfg.Dir = "  " }},
		{name: "период", edit: func(cfg *Config) { cfg.ScanInterval = 0 }},
		{name: "число файлов", edit: func(cfg *Config) { cfg.MaxFilesPerScan = 0 }},
		{name: "размер", edit: func(cfg *Config) { cfg.MaxFileBytes = -1 }},
		{name: "попытки", edit: func(cfg *Config) { cfg.MaxAttempts = 0 }},
	}
	handle := func([]byte, time.Time) error { return nil }
	logger := slog.New(slog.DiscardHandler)
	clk := clock.RealTime{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig(t.TempDir())
			tt.edit(&cfg)
			if _, err := New(cfg, handle, logger, clk); err == nil {
				t.Fatal("некорректная конфигурация принята")
			}
		})
	}
	cfg := testConfig(t.TempDir())
	if _, err := New(cfg, nil, logger, clk); err == nil {
		t.Error("принят отсутствующий обработчик")
	}
	if _, err := New(cfg, handle, nil, clk); err == nil {
		t.Error("принят отсутствующий журнал")
	}
	if _, err := New(cfg, handle, logger, nil); err == nil {
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
	}, io.Discard, clock.RealTime{})

	requireMissing(t, dir)
	running := runTask(t, inbox, inbox.Run)
	select {
	case <-inbox.Ready():
	case <-running.Done():
		t.Fatalf("Run завершился до готовности: %v", running.Err())
	case <-time.After(testTimeout):
		t.Fatal("файловый вход не сообщил о готовности")
	}
	publish(t, dir, fileA, `{"event_id":"00000000-0000-4000-8000-00000000000b"}`)
	select {
	case contents := <-got:
		if contents != `{"event_id":"00000000-0000-4000-8000-00000000000b"}` {
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
	i := newTestInbox(t, testConfig(path), nil, io.Discard, clock.RealTime{})
	if err := i.Run(); err == nil {
		t.Fatal("не удалось обнаружить, что путь занят файлом")
	}
	requireNotReady(t, i)
}

func TestRunScansExistingPublicationsAtStartup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := testConfig(dir)
	cfg.ScanInterval = time.Hour
	publish(t, dir, fileA, "публикация до запуска")
	i := newTestInbox(t, cfg, nil, io.Discard, clock.RealTime{})
	runTask(t, i, i.Run)
	requireEventuallyMissing(t, filepath.Join(dir, fileA))
}

func TestRunRejectsInaccessibleDirectory(t *testing.T) {
	t.Parallel()
	requireUnprivileged(t)
	dir := t.TempDir()
	setMode(t, dir, 0o500)
	t.Cleanup(func() { setMode(t, dir, 0o700) })
	i := newTestInbox(t, testConfig(dir), nil, io.Discard, clock.RealTime{})
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
	i := newTestInbox(t, cfg, nil, io.Discard, clock.RealTime{})
	running := task.New()
	go func() { running.Complete(i.Run()) }()
	t.Cleanup(func() {
		i.Stop()
		select {
		case <-running.Done():
		case <-time.After(testTimeout):
			t.Error("Run не завершился после Stop")
		}
	})
	select {
	case <-i.Ready():
	case <-running.Done():
		t.Fatalf("Run завершился до готовности: %v", running.Err())
	case <-time.After(testTimeout):
		t.Fatal("файловый вход не сообщил о готовности")
	}

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
	i := newTestInbox(t, testConfig(dir), nil, io.Discard, clock.RealTime{})
	i.Stop()
	if err := i.Run(); err != nil {
		t.Fatalf("ошибка штатной отмены: %v", err)
	}
	requireMissing(t, dir)
	requireNotReady(t, i)
}

func TestLoopRescansWithoutNotifications(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t.TempDir())
	cfg.ScanInterval = 10 * time.Millisecond
	i := newTestInbox(t, cfg, nil, io.Discard, clock.RealTime{})
	runTask(t, i, func() error {
		return i.loop(i.ctx, nil)
	})
	publish(t, cfg.Dir, fileA, "синтетическая публикация")
	requireEventuallyMissing(t, filepath.Join(cfg.Dir, fileA))
}

func TestLoopRescansOnNotificationOverflow(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t.TempDir())
	cfg.ScanInterval = time.Hour
	i := newTestInbox(t, cfg, nil, io.Discard, clock.RealTime{})
	publish(t, cfg.Dir, fileA, "синтетическая публикация")
	// Переполнение зависит от лимита ядра; заменён только сигнал от watcher, не файловая система.
	watchErrors := make(chan error, 1)
	watchErrors <- fsnotify.ErrEventOverflow
	close(watchErrors)
	runTask(t, i, func() error {
		return i.loop(i.ctx, &fsnotify.Watcher{Errors: watchErrors})
	})
	requireEventuallyMissing(t, filepath.Join(cfg.Dir, fileA))
}

func testConfig(dir string) Config {
	return Config{
		Dir:             dir,
		ScanInterval:    time.Second,
		MaxFilesPerScan: 12,
		MaxFileBytes:    1024,
		MaxAttempts:     2,
	}
}

func newTestInbox(t *testing.T, cfg Config, handle Handle, out io.Writer, clk clock.Clock) *Inbox {
	t.Helper()
	if handle == nil {
		handle = func([]byte, time.Time) error { return nil }
	}
	inbox, err := New(cfg, handle, slog.New(slog.NewJSONHandler(out, nil)), clk)
	if err != nil {
		t.Fatal(err)
	}
	return inbox
}

func runTask(t *testing.T, inbox *Inbox, run func() error) *task.Task {
	t.Helper()
	running := task.New()
	go func() { running.Complete(run()) }()
	t.Cleanup(func() {
		inbox.Stop()
		requireStopped(t, running)
	})
	return running
}

func requireStopped(t *testing.T, running *task.Task) {
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

func requireNotReady(t *testing.T, inbox *Inbox) {
	t.Helper()
	select {
	case <-inbox.Ready():
		t.Fatal("объявлена готовность незапущенного файлового входа")
	default:
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

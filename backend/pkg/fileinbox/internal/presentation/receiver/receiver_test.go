//go:build linux

package receiver_test

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	pkgClock "github.com/vendelev/sre-metrics/pkg/clock"
	aUsecase "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/application/usecase"
	iFilesystem "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/infrastructure/filesystem"
	iWatcher "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/infrastructure/watcher"
	pReceiver "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/presentation/receiver"
	pkgTask "github.com/vendelev/sre-metrics/pkg/task"
)

type testWatcher struct {
	errors chan error
	closed bool
}

func (w *testWatcher) Events() <-chan fsnotify.Event { return nil }
func (w *testWatcher) Errors() <-chan error          { return w.errors }

func (w *testWatcher) Close() error {
	w.closed = true
	return nil
}

func TestReceiverRescansOnNotificationOverflow(t *testing.T) {
	t.Parallel()
	const timeout = 3 * time.Second
	dir := t.TempDir()
	logger := slog.New(slog.DiscardHandler)
	handled := make(chan string, 1)
	scanner := aUsecase.New(
		iFilesystem.New(dir),
		func(contents []byte, _ time.Time) error {
			select {
			case handled <- string(contents):
			default:
			}
			return nil
		},
		aUsecase.Limits{MaxFilesPerScan: 1, MaxFileBytes: 1024, MaxAttempts: 1},
		pkgClock.RealTime{},
		logger,
	)
	// Переполнение зависит от лимитов ядра: подменён только источник уведомлений.
	watcher := &testWatcher{errors: make(chan error, 1)}
	receiver := pReceiver.New(dir, time.Hour, scanner, func(string) (iWatcher.Watcher, error) {
		return watcher, nil
	}, logger)
	running := pkgTask.New()
	go func() { running.Complete(receiver.Run()) }()
	t.Cleanup(func() {
		receiver.Stop()
		select {
		case <-running.Done():
			if err := running.Err(); err != nil {
				t.Error(err)
			}
			if !watcher.closed {
				t.Error("наблюдение не закрыто после остановки")
			}
		case <-time.After(timeout):
			t.Error("приёмник не остановился")
		}
	})
	select {
	case <-receiver.Ready():
	case <-running.Done():
		t.Fatalf("приёмник завершился до готовности: %v", running.Err())
	case <-time.After(timeout):
		t.Fatal("готовность не наступила")
	}
	path := filepath.Join(dir, "00000000-0000-4000-8000-00000000000a.json")
	tmp := path + ".tmp"
	const body = "синтетическая публикация"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	watcher.errors <- fsnotify.ErrEventOverflow
	select {
	case got := <-handled:
		if got != body {
			t.Fatalf("неожиданное содержимое публикации: %q", got)
		}
	case <-time.After(timeout):
		t.Fatal("переполнение уведомлений не вызвало обработку публикации")
	}
	receiver.Stop()
	select {
	case <-running.Done():
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("обработанная публикация не удалена: %v", err)
		}
	case <-time.After(timeout):
		t.Fatal("приёмник не остановился")
	}
}

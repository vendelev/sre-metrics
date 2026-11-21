// Package receiver управляет жизненным циклом и планированием сканирования файлового входа.
package receiver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"go.opentelemetry.io/otel/metric"

	aUsecase "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/application/usecase"
	dPublication "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/domain/publication"
	iOtel "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/infrastructure/otel"
	iWatcher "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/infrastructure/watcher"
)

// Receiver связывает готовность и остановку с периодическим и событийным сканированием.
type Receiver struct {
	dir           string
	scanInterval  time.Duration
	scanner       *aUsecase.Scanner
	newWatcher    func(string) (iWatcher.Watcher, error)
	logger        *slog.Logger
	metricsActive bool
	ctx           context.Context
	stop          context.CancelFunc
	ready         chan struct{}
	started       atomic.Bool
}

// New собирает цикл без открытия каталога и наблюдения.
func New(
	dir string,
	scanInterval time.Duration,
	scanner *aUsecase.Scanner,
	newWatcher func(string) (iWatcher.Watcher, error),
	logger *slog.Logger,
) *Receiver {
	ctx, stop := context.WithCancel(context.Background())
	return &Receiver{
		dir:          dir,
		scanInterval: scanInterval,
		scanner:      scanner,
		newWatcher:   newWatcher,
		logger:       logger,
		ctx:          ctx,
		stop:         stop,
		ready:        make(chan struct{}),
	}
}

// Ready закрывается после чтения каталога, до обработки первой пачки.
func (r *Receiver) Ready() <-chan struct{} {
	return r.ready
}

// Run выполняет стартовый и последующие проходы до Stop или ошибки чтения каталога.
func (r *Receiver) Run() error {
	if !r.started.CompareAndSwap(false, true) {
		return errors.New("повторный запуск файлового входа")
	}
	defer r.stop()
	defer r.scanner.Clear()
	if r.ctx.Err() != nil {
		return nil
	}
	if err := r.scanner.Prepare(r.ctx); err != nil {
		return err
	}
	watcher := r.watch(r.dir)
	if watcher != nil {
		defer r.closeWatch(watcher)
	}
	entries, err := r.scanner.List(r.ctx)
	if err != nil {
		return fmt.Errorf("чтение каталога файлового входа при запуске: %w", err)
	}
	if r.ctx.Err() != nil {
		return nil
	}
	close(r.ready)
	r.scanner.Scan(r.ctx, entries)
	return r.loop(r.ctx, watcher)
}

// Stop прекращает планирование сканов; владелец ждёт завершения Run.
func (r *Receiver) Stop() {
	r.stop()
}

// ActivateMetrics активирует диагностику однократно до запуска приёма.
func (r *Receiver) ActivateMetrics(ctx context.Context, meter metric.Meter) error {
	if r.started.Load() || r.metricsActive {
		return errors.New("диагностику файлового входа можно активировать один раз до запуска")
	}
	diagnostics, err := iOtel.New(ctx, meter)
	if err != nil {
		return err
	}
	r.scanner.SetDiagnostics(diagnostics)
	r.metricsActive = true
	return nil
}

func (r *Receiver) watch(dir string) iWatcher.Watcher {
	watcher, err := r.newWatcher(dir)
	if err == nil {
		return watcher
	}
	// Сбой наблюдения не препятствует запуску, если каталог доступен для сканирования.
	r.logger.Warn("уведомления файлового входа недоступны, используется периодическое сканирование")
	return nil
}

func (r *Receiver) closeWatch(watcher iWatcher.Watcher) {
	if err := watcher.Close(); err != nil {
		r.logger.Warn("не удалось закрыть наблюдение за файловым входом")
	}
}

func (r *Receiver) loop(ctx context.Context, watcher iWatcher.Watcher) error {
	ticker := time.NewTicker(r.scanInterval)
	defer ticker.Stop()
	var events <-chan fsnotify.Event
	var watchErrors <-chan error
	if watcher != nil {
		events, watchErrors = watcher.Events(), watcher.Errors()
	}
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if event.Op&(fsnotify.Create|fsnotify.Write) == 0 || !strings.HasSuffix(event.Name, dPublication.Suffix) {
				continue
			}
		case err, ok := <-watchErrors:
			if !ok {
				watchErrors = nil
				continue
			}
			r.reportWatchError(err)
		}
		if ctx.Err() != nil {
			break
		}
		entries, err := r.scanner.List(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return fmt.Errorf("чтение каталога файлового входа при сканировании: %w", err)
		}
		r.scanner.Scan(ctx, entries)
	}
	return nil
}

func (r *Receiver) reportWatchError(err error) {
	if errors.Is(err, fsnotify.ErrEventOverflow) {
		r.logger.Warn("переполнение уведомлений файлового входа, каталог будет пересканирован")
		return
	}
	r.logger.Warn("сбой уведомлений файлового входа, каталог будет пересканирован")
}

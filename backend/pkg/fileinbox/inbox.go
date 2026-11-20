package fileinbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/vendelev/sre-metrics/pkg/clock"
)

const publishedSuffix = ".json"

// ErrRetry оставляет файл для повторного чтения без расходования лимита попыток.
var ErrRetry = errors.New("приём файла отложен")

// Handle принимает содержимое файла и время текущей попытки. Nil означает
// успешный приём, ErrRetry (в том числе обёрнутый) — повтор, другая ошибка —
// окончательный отказ. Handle выполняется последовательно и не должен блокироваться.
type Handle func(contents []byte, now time.Time) error

// Inbox последовательно обслуживает один каталог. Создаётся через New.
type Inbox struct {
	scanner scanner
	ctx     context.Context
	stop    context.CancelFunc
	ready   chan struct{}
	started atomic.Bool
}

// New проверяет конфигурацию и собирает файловый вход, не открывая ресурсов.
func New(cfg Config, handle Handle, logger *slog.Logger, clk clock.Clock) (*Inbox, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	switch {
	case handle == nil:
		return nil, errors.New("не задан обработчик файлов")
	case logger == nil:
		return nil, errors.New("не задан журнал файлового входа")
	case clk == nil:
		return nil, errors.New("не заданы часы файлового входа")
	}
	ctx, stop := context.WithCancel(context.WithoutCancel(context.Background()))
	return &Inbox{
		scanner: scanner{
			cfg:    cfg,
			handle: handle,
			logger: logger,
			clk:    clk,
			states: make(map[string]fileState),
		},
		ctx:   ctx,
		stop:  stop,
		ready: make(chan struct{}),
	}, nil
}

// Ready закрывается, когда каталог доступен и наблюдение установлено или заменено
// периодическим сканированием. При ошибке запуска канал не закрывается: владелец
// одновременно ждёт Ready, результат Run или отмену запуска. После готовности
// за ошибками работы следят по результату Run.
func (i *Inbox) Ready() <-chan struct{} {
	return i.ready
}

// Run запускается владельцем один раз. Ошибка запуска или чтения каталога при
// сканировании завершает Run; штатная остановка через Stop возвращает nil.
// Ошибки наблюдения и отдельных файлов не останавливают работу.
// Перед возвратом Run закрывает наблюдение и освобождает состояние повторов.
func (i *Inbox) Run() error {
	if !i.started.CompareAndSwap(false, true) {
		return errors.New("повторный запуск файлового входа")
	}
	defer i.stop()
	defer clear(i.scanner.states)
	if i.ctx.Err() != nil {
		return nil
	}
	dir := i.scanner.cfg.Dir
	if err := prepareDir(dir); err != nil {
		i.scanner.diagnostics.add(i.ctx, ioError, 1)
		return err
	}
	watcher := i.watch(dir)
	if watcher != nil {
		defer i.closeWatch(watcher)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		i.scanner.diagnostics.add(i.ctx, ioError, 1)
		return fmt.Errorf("чтение каталога файлового входа при запуске: %w", err)
	}
	if i.ctx.Err() != nil {
		return nil
	}
	close(i.ready)
	i.scanner.scan(i.ctx, entries)
	return i.loop(i.ctx, watcher)
}

// Stop прекращает планирование новых операций; уже начатый Handle завершается
// с обычным удалением файла по результату. Допустим до Run и при повторных вызовах.
// Владелец дожидается завершения через результат Run.
func (i *Inbox) Stop() {
	i.stop()
}

func prepareDir(dir string) error {
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return fmt.Errorf("создание каталога файлового входа: %w", err)
	}
	// Для листинга, доступа к записям и удаления нужны права r/w/x каталога.
	const directoryAccess = 0o7
	if err := syscall.Access(dir, directoryAccess); err != nil {
		return fmt.Errorf("проверка доступа к каталогу файлового входа: %w", err)
	}
	return nil
}

func (i *Inbox) watch(dir string) *fsnotify.Watcher {
	watcher, err := fsnotify.NewWatcher()
	if err == nil {
		if err = watcher.Add(dir); err == nil {
			return watcher
		}
		i.closeWatch(watcher)
	}
	// Сбой наблюдения не препятствует запуску, если каталог доступен для сканирования.
	i.scanner.logger.Warn("уведомления файлового входа недоступны, используется периодическое сканирование")
	return nil
}

func (i *Inbox) closeWatch(watcher *fsnotify.Watcher) {
	if err := watcher.Close(); err != nil {
		i.scanner.logger.Warn("не удалось закрыть наблюдение за файловым входом")
	}
}

func (i *Inbox) loop(ctx context.Context, watcher *fsnotify.Watcher) error {
	ticker := time.NewTicker(i.scanner.cfg.ScanInterval)
	defer ticker.Stop()
	var events <-chan fsnotify.Event
	var watchErrors <-chan error
	if watcher != nil {
		events, watchErrors = watcher.Events, watcher.Errors
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
			if event.Op&(fsnotify.Create|fsnotify.Write) == 0 || !strings.HasSuffix(event.Name, publishedSuffix) {
				continue
			}
		case err, ok := <-watchErrors:
			if !ok {
				watchErrors = nil
				continue
			}
			i.reportWatchError(err)
		}
		if ctx.Err() != nil {
			break
		}
		if err := i.rescan(ctx); err != nil && ctx.Err() == nil {
			return err
		}
	}
	return nil
}

func (i *Inbox) rescan(ctx context.Context) error {
	entries, err := os.ReadDir(i.scanner.cfg.Dir)
	if err != nil {
		i.scanner.diagnostics.add(ctx, ioError, 1)
		return fmt.Errorf("чтение каталога файлового входа при сканировании: %w", err)
	}
	i.scanner.scan(ctx, entries)
	return nil
}

func (i *Inbox) reportWatchError(err error) {
	if errors.Is(err, fsnotify.ErrEventOverflow) {
		i.scanner.logger.Warn("переполнение уведомлений файлового входа, каталог будет пересканирован")
		return
	}
	i.scanner.logger.Warn("сбой уведомлений файлового входа, каталог будет пересканирован")
}

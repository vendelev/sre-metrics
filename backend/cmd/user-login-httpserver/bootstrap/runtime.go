package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"

	userlogin "github.com/vendelev/sre-metrics/user-login/bootstrap"
)

// taskResult сводит фоновые задачи запуска к общему виду для неблокирующего
// чтения ошибок: HTTP-задачу публикует runtime core, остальные — модуль.
type taskResult struct {
	done <-chan struct{}
	err  func() error // Читается только после закрытия done.
}

func (a *App) taskResults() []taskResult {
	tasks := make([]taskResult, 0, 3)
	if serve := a.server.ServeTask(); serve != nil {
		tasks = append(tasks, taskResult{done: serve.Done(), err: serve.Err})
	}
	if task := a.module.WorkerTask(); task != nil {
		tasks = append(tasks, taskResult{done: task.Done(), err: task.Err})
	}
	if task := a.module.InboxTask(); task != nil {
		tasks = append(tasks, taskResult{done: task.Done(), err: task.Err})
	}
	return tasks
}

// run собирает и запускает приложение до отмены ctx или ошибки компонента.
// Неуспешный запуск сам выполняет очистку и возвращает ошибку без разбора
// её состава; специальная нормализация отмены запуска не выполняется.
func (a *App) run(ctx context.Context, ready func(net.Addr)) error {
	a.ready = ready
	startErr := a.start(ctx)
	var stopErr error
	if startErr == nil {
		select {
		case <-ctx.Done():
		case <-a.module.Failed():
		case <-a.server.Failed():
		}
		stopErr = a.stop(ctx)
	}
	result := errors.Join(startErr, stopErr)
	if result != nil {
		a.logger.Error("не удалось штатно остановить сервис", slog.Any("error", result))
	} else {
		a.logger.Info("сервис user-login штатно остановлен")
	}
	return result
}

// start открывает ресурсы в согласованном порядке; неуспешная фаза возвращает
// управление для очистки уже открытых ресурсов.
func (a *App) start(ctx context.Context) error {
	if err := a.startResources(ctx); err != nil {
		// Неуспешный запуск сам выполняет ту же процедуру очистки; ошибка
		// очистки дополняет исходную, если та ещё не получена из задачи.
		if cleanupErr := a.stop(ctx); cleanupErr != nil {
			if errors.Is(cleanupErr, err) {
				return cleanupErr
			}
			return errors.Join(err, cleanupErr)
		}
		return err
	}
	return nil
}

// startResources открывает ресурсы в согласованном порядке: listener →
// SDK/pipeline → активация инструментов модуля → воркер → файловый вход → HTTP → готовность.
func (a *App) startResources(ctx context.Context) error {
	if err := a.server.Open(ctx); err != nil {
		return err
	}
	if err := a.startTelemetry(ctx); err != nil {
		return err
	}
	if err := a.module.StartWorker(ctx); err != nil {
		return err
	}
	if err := a.module.StartInbox(ctx); err != nil {
		return err
	}
	if err := a.server.StartServe(ctx); err != nil {
		return err
	}
	return a.announceReady(ctx)
}

// startTelemetry запускает OTLP pipeline core и активирует предметные
// инструменты модуля в meter общего provider.
func (a *App) startTelemetry(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := a.metrics.Start(ctx); err != nil {
		return fmt.Errorf("запуск OTLP pipeline: %w", err)
	}
	return a.module.Start(ctx, a.metrics.Meter(userlogin.ScopeName))
}

// announceReady объявляет готовность только после успешных фаз запуска
// и при отсутствии ошибок уже запущенных фоновых задач.
func (a *App) announceReady(ctx context.Context) error {
	if err := a.taskErrors(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	a.logger.Info("сервис user-login запущен", slog.String("address", a.server.Addr().String()))
	if a.ready != nil {
		a.ready(a.server.Addr())
	}
	return nil
}

// stop объединяет очистку частичного запуска и штатную остановку. Повторный
// вызов возвращает тот же результат, без нового бюджета и повторного экспорта.
func (a *App) stop(ctx context.Context) error {
	a.stopOnce.Do(func() { a.stopErr = a.shutdown(ctx) })
	return a.stopErr
}

// shutdown выполняет остановку в пределах одного общего бюджета без резерва SDK.
func (a *App) shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.cfg.ShutdownTimeout)
	defer cancel()

	closeErr := a.server.CloseListener()
	// Прекращение планирования обслуживания опережает HTTP Shutdown: воркер
	// дожидается начатой операции, новые тики не планируются.
	a.module.StopMaintenance()
	a.module.StopInbox()
	if a.module.WorkerTask() != nil {
		stopDeadline := context.AfterFunc(ctx, a.module.Terminate)
		defer stopDeadline()
	}
	var httpErr error
	if a.server.ServeTask() != nil {
		// При неуспехе Shutdown соединения закрываются принудительно внутри
		// компонента, ошибки объединяются.
		httpErr = a.server.Shutdown(ctx)
	}
	// После сигнала файловому входу и HTTP Shutdown закрываем приём. Даже при
	// истёкшем бюджете поздний обработчик не сможет поставить событие в очередь.
	a.module.CloseQueue()
	drainErr := a.waitTasks(ctx)
	if drainErr != nil && httpErr == nil {
		// Close обрывает соединения, но не завершает произвольный обработчик Go.
		drainErr = errors.Join(drainErr, a.server.Close())
	}
	a.module.Terminate()
	var sdkErr error
	// Финальный экспорт — best effort с оставшимся временем. Даже при
	// истёкшем контексте передаётся команда освобождения ресурсов.
	if err := a.metrics.Shutdown(ctx); err != nil {
		sdkErr = fmt.Errorf("освобождение SDK метрик: %w", err)
	}
	result := errors.Join(closeErr, httpErr, drainErr, a.taskErrors(), sdkErr)
	if ctx.Err() != nil {
		result = errors.Join(result,
			fmt.Errorf("исчерпан бюджет остановки user-login (%s): %w", a.cfg.ShutdownTimeout, ctx.Err()))
	}
	return result
}

// waitTasks дожидается завершения HTTP, обработки остатка очереди
// воркером и файлового входа в пределах одного бюджета.
func (a *App) waitTasks(ctx context.Context) error {
	if serve := a.server.ServeTask(); serve != nil {
		select {
		case <-serve.Done():
		case <-ctx.Done():
			return fmt.Errorf("исчерпан бюджет завершения обработки: %w", ctx.Err())
		}
	}
	if err := a.module.Drain(ctx); err != nil {
		return err
	}
	if inbox := a.module.InboxTask(); inbox != nil {
		select {
		case <-inbox.Done():
		case <-ctx.Done():
			return fmt.Errorf("исчерпан бюджет завершения файлового входа: %w", ctx.Err())
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("исчерпан бюджет завершения обработки: %w", err)
	}
	return nil
}

func (a *App) taskErrors() error {
	var result error
	for _, task := range a.taskResults() {
		select {
		case <-task.done:
			result = errors.Join(result, task.err())
		default:
		}
	}
	return result
}

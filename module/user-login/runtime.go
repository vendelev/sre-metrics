package userlogin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/otel"
)

// backgroundTask публикует результат закрытием done. После истечения бюджета
// владелец не ждёт произвольную Go-функцию и не читает ещё не готовый результат.
type backgroundTask struct {
	done chan struct{}
	err  error // Читается только после закрытия done.
}

func (s *service) startTask(operation func() error) *backgroundTask {
	task := &backgroundTask{done: make(chan struct{})}
	go func() {
		task.err = operation()
		close(task.done)
		if task.err != nil {
			select {
			case s.failed <- struct{}{}:
			default:
			}
		}
	}()
	return task
}

func (s *service) start(ctx context.Context) error {
	err := s.startResources(ctx)
	if err == nil {
		return nil
	}
	// Неуспешный запуск сам выполняет ту же процедуру очистки; ошибка очистки
	// дополняет исходную.
	if cleanupErr := s.stop(ctx); cleanupErr != nil {
		return errors.Join(err, cleanupErr)
	}
	return err
}

func (s *service) startResources(ctx context.Context) error {
	if err := s.openListener(ctx); err != nil {
		return err
	}
	if err := s.startSDK(ctx); err != nil {
		return err
	}
	if err := s.startMaintenance(ctx); err != nil {
		return err
	}
	if err := s.startHTTP(ctx); err != nil {
		return err
	}
	return s.announceReady(ctx)
}

func (s *service) announceReady(ctx context.Context) error {
	if err := s.taskErrors(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.logger.Info("сервис user-login запущен", slog.String("address", s.listener.Addr().String()))
	if s.ready != nil {
		s.ready(s.listener.Addr())
	}
	return nil
}

// stop объединяет очистку частичного запуска и штатную остановку. Повторный
// вызов возвращает тот же результат, без нового бюджета и повторного экспорта.
func (s *service) stop(ctx context.Context) error {
	s.stopOnce.Do(func() { s.stopErr = s.shutdown(ctx) })
	return s.stopErr
}

// shutdown выполняет остановку в пределах одного общего бюджета без резерва SDK.
func (s *service) shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ShutdownTimeout)
	defer cancel()

	closeErr := s.closeListener()
	if s.stopMaintenance != nil {
		s.stopMaintenance()
	}
	var httpErr error
	if s.serverTask != nil {
		httpErr = shutdownError(s.server.Shutdown(ctx))
	}
	drainErr := s.waitTasks(ctx)
	if httpErr != nil || drainErr != nil {
		// Close обрывает соединения, но не завершает произвольный обработчик Go.
		drainErr = errors.Join(drainErr, shutdownError(s.server.Close()))
	}
	var sdkErr error
	if s.pipeline != nil {
		// Финальный экспорт — best effort с оставшимся временем. Даже при
		// истёкшем контексте передаётся команда освобождения ресурсов.
		if err := s.pipeline.Shutdown(ctx); err != nil {
			sdkErr = fmt.Errorf("освобождение SDK метрик: %w", err)
		}
	}
	result := errors.Join(closeErr, httpErr, drainErr, s.taskErrors(), sdkErr)
	if ctx.Err() != nil {
		result = errors.Join(result,
			fmt.Errorf("исчерпан бюджет остановки user-login (%s): %w", s.cfg.ShutdownTimeout, ctx.Err()))
	}
	return result
}

func (s *service) waitTasks(ctx context.Context) error {
	for _, task := range []*backgroundTask{s.serverTask, s.maintenanceTask} {
		if task == nil {
			continue
		}
		select {
		case <-task.done:
		case <-ctx.Done():
			return fmt.Errorf("исчерпан бюджет завершения обработки: %w", ctx.Err())
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("исчерпан бюджет завершения обработки: %w", err)
	}
	return nil
}

func (s *service) taskErrors() error {
	var result error
	for _, task := range []*backgroundTask{s.serverTask, s.maintenanceTask} {
		if task == nil {
			continue
		}
		select {
		case <-task.done:
			result = errors.Join(result, task.err)
		default:
		}
	}
	return result
}

func (s *service) startSDK(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	pipeline, err := otel.StartPipeline(ctx, s.recorder, s.logger.With("component", "otel"))
	s.pipeline = pipeline
	if err != nil {
		return fmt.Errorf("запуск OTLP pipeline: %w", err)
	}
	return ctx.Err()
}

package userlogin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"github.com/vendelev/sre-metrics/service/user-login/internal/infrastructure/otel"
)

// runOptions позволяет тестам управлять ресурсными границами и обслуживанием,
// не создавая отдельный путь запуска сервиса.
type runOptions struct {
	listen       func(context.Context) (net.Listener, error)
	ready        func(net.Addr)
	confirmReady func(context.Context) error
	maintain     func() error
}

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
	if s.options.confirmReady != nil {
		if err := s.options.confirmReady(ctx); err != nil {
			return err
		}
	}
	if err := s.taskErrors(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.logger.Info("сервис user-login запущен", slog.String("address", s.listener.Addr().String()))
	if s.options.ready != nil {
		s.options.ready(s.listener.Addr())
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

// metricPipeline описывает только владение ресурсами; запись получает Store
// через уже собранный Recorder. Тесты lifecycle могут использовать локальный SDK.
type metricPipeline interface {
	Shutdown(context.Context) error
}

func (s *service) startSDK(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	open := s.openPipeline
	if open == nil {
		open = func(ctx context.Context, recorder *otel.Recorder) (metricPipeline, error) {
			pipeline, err := otel.StartPipeline(ctx, recorder, otel.PipelineSettings{
				Endpoint: s.cfg.OTLPMetricsEndpoint, Interval: s.cfg.OTLPExportInterval,
				ExportTimeout: s.cfg.OTLPExportTimeout,
			}, s.logger.With("component", "otel"))
			if err != nil {
				return nil, err
			}
			return pipeline, nil
		}
	}
	pipeline, err := open(ctx, s.recorder)
	s.pipeline = pipeline
	if err != nil {
		return fmt.Errorf("запуск OTLP pipeline: %w", err)
	}
	return ctx.Err()
}

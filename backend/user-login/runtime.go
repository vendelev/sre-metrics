package userlogin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/httpserver"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/otel"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/queue"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/worker"
	"github.com/vendelev/sre-metrics/user-login/internal/presentation/config"
)

// backgroundTask публикует результат закрытием done. После истечения бюджета
// владелец не ждёт произвольную Go-функцию и не читает ещё не готовый результат.
type backgroundTask struct {
	done chan struct{}
	err  error // Читается только после закрытия done.
}

// service владеет компонентами и ресурсами одного запуска. Сборка выполняется
// явной цепочкой конструкторов в app.go; контейнер зависимостей и его
// lifecycle не используются. Поля после пустой строки — состояние протокола
// одного запуска: этот файл задаёт его порядок и очистку.
type service struct {
	httpServer *httpserver.Server
	worker     *worker.Worker
	eventQueue *queue.EventQueue
	recorder   *otel.Recorder
	cfg        config.Config
	logger     *slog.Logger
	pipeline   *otel.Pipeline

	ready      func(net.Addr)
	serverTask *backgroundTask
	workerTask *backgroundTask
	failed     chan struct{}
	stopOnce   sync.Once
	stopErr    error // Читается после stopOnce.Do, включая конкурентный повторный вызов.
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

// startResources открывает ресурсы в согласованном порядке: listener →
// SDK/pipeline → воркер → HTTP → готовность. Неуспешная фаза возвращает
// управление для очистки уже открытых ресурсов.
func (s *service) startResources(ctx context.Context) error {
	if err := s.httpServer.Open(ctx); err != nil {
		return err
	}
	if err := s.startSDK(ctx); err != nil {
		return err
	}
	if err := s.startWorker(ctx); err != nil {
		return err
	}
	if err := s.startHTTP(ctx); err != nil {
		return err
	}
	return s.announceReady(ctx)
}

// startWorker запускает фоновую задачу последовательной обработки очереди.
func (s *service) startWorker(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.workerTask = s.startTask(s.worker.Run)
	return nil
}

// startHTTP запускает фоновую задачу Serve; техническую интерпретацию её
// результата выполняет httpserver.Server.
func (s *service) startHTTP(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.serverTask = s.startTask(s.httpServer.Serve)
	return nil
}

// announceReady объявляет готовность только после успешных фаз запуска
// и при отсутствии ошибок уже запущенных фоновых задач.
func (s *service) announceReady(ctx context.Context) error {
	if err := s.taskErrors(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.logger.Info("сервис user-login запущен", slog.String("address", s.httpServer.Addr().String()))
	if s.ready != nil {
		s.ready(s.httpServer.Addr())
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

	closeErr := s.httpServer.CloseListener()
	// Прекращение планирования обслуживания опережает HTTP Shutdown: воркер
	// дожидается начатой операции, новые тики не планируются.
	s.worker.StopMaintenance()
	if s.workerTask != nil {
		stopDeadline := context.AfterFunc(ctx, s.worker.Stop)
		defer stopDeadline()
	}
	var httpErr error
	if s.serverTask != nil {
		// При неуспехе Shutdown соединения закрываются принудительно внутри
		// компонента, ошибки объединяются.
		httpErr = s.httpServer.Shutdown(ctx)
	}
	if s.eventQueue != nil {
		// После HTTP Shutdown новых постановок нет. Даже при истёкшем бюджете
		// поздний обработчик не сможет поставить событие после завершения воркера.
		s.eventQueue.Close()
	}
	drainErr := s.waitTasks(ctx)
	if drainErr != nil && httpErr == nil {
		// Close обрывает соединения, но не завершает произвольный обработчик Go.
		drainErr = errors.Join(drainErr, s.httpServer.Close())
	}
	s.worker.Stop()
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
	for _, task := range []*backgroundTask{s.serverTask, s.workerTask} {
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
	for _, task := range []*backgroundTask{s.serverTask, s.workerTask} {
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

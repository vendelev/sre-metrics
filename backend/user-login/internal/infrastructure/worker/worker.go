package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/vendelev/sre-metrics/user-login/internal/application"
	dError "github.com/vendelev/sre-metrics/user-login/internal/domain/error"
	dEvent "github.com/vendelev/sre-metrics/user-login/internal/domain/event"
	dScenario "github.com/vendelev/sre-metrics/user-login/internal/domain/scenario"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/queue"
)

// Worker выполняет последовательную обработку очереди событий: один цикл
// вызывает Process и Maintain, не перекрывая их. Отмена на двух уровнях
// принадлежит владельцу остановки: StopMaintenance прекращает планирование
// обслуживания, Stop отменяет весь воркер.
type Worker struct {
	observer *application.LoginObserver
	queue    *queue.EventQueue
	interval time.Duration
	logger   *slog.Logger

	workerCtx       context.Context
	stop            context.CancelFunc
	maintenanceCtx  context.Context
	stopMaintenance context.CancelFunc
}

// New собирает исполнителя из уже собранных компонентов. Контексты отмены
// создаются при сборке: Stop допустим до запуска и при очистке частично
// успешного запуска. Родитель не распространяет отмену — контекст остановки
// сервиса не отменяет воркер.
func New(
	observer *application.LoginObserver,
	events *queue.EventQueue,
	maintenanceInterval time.Duration,
	logger *slog.Logger,
) (*Worker, error) {
	if observer == nil {
		return nil, errors.New("не задана операция обработки событий")
	}
	if events == nil {
		return nil, errors.New("не задана очередь приёма событий")
	}
	if logger == nil {
		return nil, errors.New("не задан журнал сервиса")
	}
	if maintenanceInterval <= 0 {
		return nil, fmt.Errorf("интервал обслуживания user-login должен быть положительным: %s", maintenanceInterval)
	}
	workerCtx, stop := context.WithCancel(context.WithoutCancel(context.Background()))
	maintenanceCtx, stopMaintenance := context.WithCancel(workerCtx)
	return &Worker{
		observer:        observer,
		queue:           events,
		interval:        maintenanceInterval,
		logger:          logger,
		workerCtx:       workerCtx,
		stop:            stop,
		maintenanceCtx:  maintenanceCtx,
		stopMaintenance: stopMaintenance,
	}, nil
}

// Run последовательно изменяет состояние через Process и Maintain до отмены
// владельцем или закрытия очереди. Закрытый канал приёма дочитывается до
// конца; отмена владельцем не является ошибкой задачи: исчерпание бюджета
// сообщает shutdown. Ошибки самих Process и Maintain сохраняются, даже если
// отмена произошла одновременно. Выход закрывает очередь: новые постановки
// невозможны и при отказе воркера.
func (w *Worker) Run() error {
	defer w.stop()
	defer w.queue.Close()
	return w.loop(w.workerCtx, w.maintenanceCtx.Done())
}

// StopMaintenance прекращает планирование обслуживания и позволяет завершить
// текущую операцию; вызывается владельцем до HTTP Shutdown.
func (w *Worker) StopMaintenance() {
	w.stopMaintenance()
}

// Stop отменяет воркер: новые операции не планируются, незавершённый drain
// возвращает ошибку. Повторный вызов безопасен.
func (w *Worker) Stop() {
	w.stop()
}

func (w *Worker) loop(ctx context.Context, maintenanceStopped <-chan struct{}) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	ticks := ticker.C
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-maintenanceStopped:
			ticker.Stop()
			ticks = nil
			maintenanceStopped = nil
		case event, ok := <-w.queue.Events():
			if !ok {
				return nil
			}
			if err := ctx.Err(); err != nil {
				return nil
			}
			if err := w.processEvent(event); err != nil {
				return err
			}
		case <-ticks:
			select {
			case <-maintenanceStopped:
				continue
			default:
			}
			if err := ctx.Err(); err != nil {
				return nil
			}
			if err := w.observer.Maintain(); err != nil {
				return fmt.Errorf("периодическое обслуживание user-login: %w", err)
			}
		}
	}
}

func (w *Worker) processEvent(event *dEvent.Request) error {
	result, err := w.observer.Process(event)
	if err != nil {
		reason := "internal_error"
		switch {
		case errors.Is(err, dError.ErrConflict):
			reason = "event_conflict"
		case errors.Is(err, dScenario.ErrScenarioExpired):
			reason = "scenario_expired"
		case errors.Is(err, dError.ErrCapacityExceeded):
			reason = "capacity_exceeded"
		}
		attrs := []any{
			slog.String("type", string(event.Type)),
			slog.String("event_id", event.EventID.String()),
			slog.String("scenario_id", event.ScenarioID.String()),
			slog.String("reason", reason),
		}
		if reason == "internal_error" {
			w.logger.Error("не удалось обработать событие", attrs...)
			return fmt.Errorf("фоновая обработка события user-login: %w", err)
		}
		w.logger.Warn("событие отклонено при обработке", attrs...)
		return nil
	}
	if result.NegativeDurationScenarioID != uuid.Nil {
		w.logger.Warn("исходный момент успеха раньше начала: длительность не учитывается",
			slog.String("scenario_id", result.NegativeDurationScenarioID.String()))
	}
	return nil
}

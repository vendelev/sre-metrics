package userlogin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	dError "github.com/vendelev/sre-metrics/user-login/internal/domain/error"
	dEvent "github.com/vendelev/sre-metrics/user-login/internal/domain/event"
	dScenario "github.com/vendelev/sre-metrics/user-login/internal/domain/scenario"
)

func (s *service) startWorker(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	workerCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	maintenanceCtx, stopMaintenance := context.WithCancel(workerCtx)
	s.stopWorker = cancel
	s.stopMaintenance = stopMaintenance
	s.workerTask = s.startTask(func() error {
		defer cancel()
		defer s.eventQueue.Close()
		return s.runWorker(workerCtx, maintenanceCtx.Done())
	})
	return nil
}

// runWorker последовательно изменяет состояние через Process и Maintain.
// Закрытый канал приёма дочитывается до конца; отмена ограничивает drain бюджетом.
// Отмена владельцем не является ошибкой задачи: исчерпание бюджета сообщает shutdown.
// Ошибки самих Process и Maintain сохраняются, даже если отмена произошла одновременно.
func (s *service) runWorker(ctx context.Context, maintenanceStopped <-chan struct{}) error {
	ticker := time.NewTicker(s.cfg.MaintenanceInterval)
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
		case event, ok := <-s.eventQueue.Events():
			if !ok {
				return nil
			}
			if err := ctx.Err(); err != nil {
				return nil
			}
			if err := s.processEvent(event); err != nil {
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
			if err := s.observer.Maintain(); err != nil {
				return fmt.Errorf("периодическое обслуживание user-login: %w", err)
			}
		}
	}
}

func (s *service) processEvent(event *dEvent.Request) error {
	result, err := s.observer.Process(event)
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
			s.logger.Error("не удалось обработать событие", attrs...)
			return fmt.Errorf("фоновая обработка события user-login: %w", err)
		}
		s.logger.Warn("событие отклонено при обработке", attrs...)
		return nil
	}
	if result.NegativeDurationScenarioID != uuid.Nil {
		s.logger.Warn("исходный момент успеха раньше начала: длительность не учитывается",
			slog.String("scenario_id", result.NegativeDurationScenarioID.String()))
	}
	return nil
}

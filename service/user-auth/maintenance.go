package userauth

import (
	"context"
	"fmt"
	"time"
)

func (s *service) startMaintenance(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Отмена запуска сначала должна прекратить приём HTTP, а не оборвать задачу.
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.stopMaintenance = cancel
	operation := s.observer.Maintain
	if s.options.maintain != nil {
		operation = s.options.maintain
	}
	s.maintenanceTask = s.startTask(func() error {
		if err := maintain(ctx, s.cfg.MaintenanceInterval, operation); err != nil {
			return fmt.Errorf("периодическое обслуживание user-auth: %w", err)
		}
		return nil
	})
	return nil
}

func maintain(ctx context.Context, interval time.Duration, operation func() error) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// При одновременной готовности тика и отмены новый вызов не нужен.
			if ctx.Err() != nil {
				return nil
			}
			if err := operation(); err != nil {
				return err
			}
		}
	}
}

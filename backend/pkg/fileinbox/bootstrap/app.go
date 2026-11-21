// Package bootstrap собирает файловый вход из его внутренних компонентов.
package bootstrap

import (
	"errors"
	"log/slog"

	pkgClock "github.com/vendelev/sre-metrics/pkg/clock"
	pkgApi "github.com/vendelev/sre-metrics/pkg/fileinbox/api"
	pkgConfig "github.com/vendelev/sre-metrics/pkg/fileinbox/config"
	aUsecase "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/application/usecase"
	iFilesystem "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/infrastructure/filesystem"
	iWatcher "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/infrastructure/watcher"
	pReceiver "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/presentation/receiver"
)

// New проверяет конфигурацию и собирает файловый вход, не открывая ресурсов.
func New(cfg pkgConfig.Config, handle pkgApi.Handle, logger *slog.Logger, clk pkgClock.Clock) (pkgApi.Inbox, error) {
	if err := cfg.Validate(); err != nil {
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
	limits := aUsecase.Limits{
		MaxFilesPerScan: cfg.MaxFilesPerScan,
		MaxFileBytes:    cfg.MaxFileBytes,
		MaxAttempts:     cfg.MaxAttempts,
	}
	scanner := aUsecase.New(iFilesystem.New(cfg.Dir), handle, limits, clk, logger)
	return pReceiver.New(cfg.Dir, cfg.ScanInterval, scanner, iWatcher.New, logger), nil
}

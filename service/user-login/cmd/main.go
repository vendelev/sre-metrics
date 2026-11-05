package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	userlogin "github.com/vendelev/sre-metrics/service/user-login"
)

func main() {
	if err := run(); err != nil {
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("service", "user-login", "component", "main")
		logger.Error("сервис завершился с ошибкой", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return userlogin.Run(ctx)
}

package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	userauth "github.com/vendelev/sre-metrics/service/user-auth"
)

func main() {
	if err := run(); err != nil {
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("service", "user-auth", "component", "main")
		logger.Error("сервис завершился с ошибкой", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return userauth.Run(ctx)
}

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	userauth "metrics/service/user-auth"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return userauth.Run(ctx)
}

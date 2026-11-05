package userlogin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"

	"github.com/vendelev/sre-metrics/service/user-login/internal/presentation/config"
)

func TestRunBindCancellation(t *testing.T) {
	bindErr := &net.OpError{Op: "listen", Net: "tcp", Err: syscall.EADDRINUSE}
	// Нормализация отмены запуска не выполняется: любая отмена во время bind —
	// ошибка запуска, независимый сбой рядом с отменой не теряется.
	for _, tc := range []struct {
		name        string
		listenError func(error) error
		want        error
	}{
		{"чистая отмена", func(err error) error { return err }, context.Canceled},
		{"отмена в сетевой обёртке", func(err error) error { return &net.OpError{Op: "listen", Net: "tcp", Err: err} }, context.Canceled},
		{"отмена с контекстом операции", func(err error) error { return fmt.Errorf("открытие listener: %w", err) }, context.Canceled},
		{"сбой bind одновременно с отменой", func(error) error { return bindErr }, syscall.EADDRINUSE},
		{"сбой bind объединён с отменой", func(err error) error { return errors.Join(err, bindErr) }, syscall.EADDRINUSE},
		{"обёртка объединённой ошибки", func(err error) error {
			return fmt.Errorf("открытие listener: %w", errors.Join(err, bindErr))
		}, syscall.EADDRINUSE},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := testService(t, config.Default())
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ready := false
			options := runOptions{
				listen: func(ctx context.Context) (net.Listener, error) {
					cancel()
					return nil, tc.listenError(ctx.Err())
				},
				ready: func(net.Addr) { ready = true },
			}
			err := app.run(ctx, options)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ожидалась ошибка %v, получено %v", tc.want, err)
			}
			if ready {
				t.Fatal("неуспешный запуск объявлен готовым")
			}
		})
	}
}

func TestRunBindFailureWithCancellation(t *testing.T) {
	t.Parallel()
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = occupied.Close() }()
	cfg := config.Default()
	cfg.ListenAddr = occupied.Addr().String()
	app := testService(t, cfg)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err = app.run(ctx, runOptions{listen: func(ctx context.Context) (net.Listener, error) {
		var listenConfig net.ListenConfig
		listener, err := listenConfig.Listen(ctx, "tcp", cfg.ListenAddr)
		cancel()
		return listener, err
	}})
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("настоящий сбой bind потерян при одновременной отмене: %v", err)
	}
}

type closeErrorListener struct {
	net.Listener
	err error
}

func (l closeErrorListener) Close() error {
	return errors.Join(l.Listener.Close(), l.err)
}

func TestRunCancellationPreservesCleanupError(t *testing.T) {
	t.Parallel()
	app := testService(t, config.Default())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	listener := newControlledListener()
	cause := errors.New("сбой закрытия listener")
	err := app.run(ctx, runOptions{listen: func(context.Context) (net.Listener, error) {
		cancel()
		return closeErrorListener{Listener: listener, err: cause}, nil
	}})
	if !errors.Is(err, cause) {
		t.Fatalf("ошибка очистки потеряна при отмене запуска: %v", err)
	}
	select {
	case <-listener.closed:
	default:
		t.Fatal("listener не закрыт после отмены запуска")
	}
}

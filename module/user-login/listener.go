package userlogin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
)

// ownedListener закрывается один раз, в том числе при частично успешном запуске.
type ownedListener struct {
	net.Listener
	once    sync.Once
	err     error
	closing atomic.Bool
}

func (l *ownedListener) Close() error {
	l.once.Do(func() {
		l.closing.Store(true)
		l.err = l.Listener.Close()
	})
	return l.err
}

func (s *service) openListener(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("запуск HTTP user-login на %s: %w", s.cfg.ListenAddr, err)
	}
	s.listener = &ownedListener{Listener: listener}
	return ctx.Err()
}

func (s *service) closeListener() error {
	if s.listener == nil {
		return nil
	}
	if err := s.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("закрытие listener user-login: %w", err)
	}
	return nil
}

func (s *service) startHTTP(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.serverTask = s.startTask(func() error {
		err := s.server.Serve(s.listener)
		if s.listener.closing.Load() && (errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed)) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("работа HTTP user-login: %w", err)
		}
		return nil
	})
	return nil
}

func shutdownError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("остановка HTTP user-login: %w", err)
}

package userlogin

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/clock"
)

// Произвольный отказ Accept нельзя надёжно воспроизвести через настоящий TCP.
// Обёртка проверяет передачу ошибки, возвращённой http.Server.Serve.
type failedAcceptListener struct {
	net.Listener
	err error
}

func (l failedAcceptListener) Accept() (net.Conn, error) {
	return nil, l.err
}

func TestHTTPFailure(t *testing.T) {
	t.Parallel()
	r := newServiceReceiver(t)
	app := testService(t, actualConfig(r), clock.RealTime{})
	t.Cleanup(func() { _ = app.stop(t.Context()) })
	if err := app.openListener(t.Context()); err != nil {
		t.Fatal(err)
	}
	cause := &net.OpError{Op: "accept", Net: "tcp", Err: errors.New("управляемый отказ Accept")}
	app.listener.Listener = failedAcceptListener{Listener: app.listener.Listener, err: cause}
	if err := app.startHTTP(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, app.serverTask.done)
	if err := app.stop(t.Context()); !errors.Is(err, cause) {
		t.Fatalf("ошибка Serve потеряна: %v", err)
	}
	assertListenerClosed(t, app)
}

// closeTriggeredAcceptListener воспроизводит ошибку после начала Shutdown:
// настоящий TCP при таком закрытии выдаёт net.ErrClosed, а не произвольный отказ.
type closeTriggeredAcceptListener struct {
	net.Listener
	entered chan struct{}
	closed  chan struct{}
	err     error
}

func (l *closeTriggeredAcceptListener) Accept() (net.Conn, error) {
	close(l.entered)
	<-l.closed
	return nil, l.err
}

func (l *closeTriggeredAcceptListener) Close() error {
	err := l.Listener.Close()
	close(l.closed)
	return err
}

func TestHTTPShutdownUsesServeResult(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		cause error
	}{
		{name: "окончательная сетевая ошибка скрыта остановкой",
			cause: &net.OpError{Op: "accept", Net: "tcp", Err: errors.New("окончательный сетевой отказ")}},
		{name: "штатное закрытие", cause: net.ErrClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := testService(t, actualConfig(newServiceReceiver(t)), clock.RealTime{})
			t.Cleanup(func() { _ = app.stop(t.Context()) })
			if err := app.openListener(t.Context()); err != nil {
				t.Fatal(err)
			}
			listener := &closeTriggeredAcceptListener{
				Listener: app.listener.Listener,
				entered:  make(chan struct{}), closed: make(chan struct{}), err: tc.cause,
			}
			app.listener.Listener = listener
			if err := app.startHTTP(t.Context()); err != nil {
				t.Fatal(err)
			}
			waitSignal(t, listener.entered)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if err := app.server.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			waitSignal(t, app.serverTask.done)
			// При начавшемся Shutdown Serve возвращает ErrServerClosed,
			// даже если Accept вернул другую ошибку. Этот результат не переопределяем.
			if err := app.stop(t.Context()); err != nil {
				t.Fatalf("штатная остановка Serve завершилась ошибкой: %v", err)
			}
			assertListenerClosed(t, app)
		})
	}
}

type closeErrorListener struct {
	net.Listener
	err error
}

func (l closeErrorListener) Close() error {
	return errors.Join(l.Listener.Close(), l.err)
}

// Системный Close обычно успешен; подмена ограничена конкретной фазой очистки
// и не требует фабрики listener в рабочем запуске.
func TestCancellationPreservesCleanupErrors(t *testing.T) {
	t.Parallel()
	r := newServiceReceiver(t)
	r.fail.Store(true)
	cfg := actualConfig(r)
	cfg.OTLPExportInterval = time.Hour
	app := testService(t, cfg, clock.RealTime{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	t.Cleanup(func() { _ = app.stop(t.Context()) })
	if err := app.openListener(ctx); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("сбой закрытия listener")
	app.listener.Listener = closeErrorListener{Listener: app.listener.Listener, err: cause}
	if err := app.startSDK(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	err := app.stop(ctx)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "освобождение SDK метрик") {
		t.Fatalf("ошибка закрытия listener или финального экспорта потеряна при отмене: %v", err)
	}
	if r.calls.Load() != 1 {
		t.Fatal("финальная отправка не состоялась или повторилась")
	}
	assertListenerClosed(t, app)
}

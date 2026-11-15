package httpserver

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/vendelev/sre-metrics/core/config"
)

func newTestServer(t *testing.T, handler http.Handler) *Server {
	t.Helper()
	return New(config.HTTP{
		Addr:              "127.0.0.1:0",
		ReadHeaderTimeout: 50 * time.Millisecond,
		ReadTimeout:       time.Second,
		WriteTimeout:      time.Second,
	}, handler, log.New(io.Discard, "", 0))
}

func waitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("не дождались сигнала")
	}
}

func waitResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("не дождались завершения задачи")
		return nil
	}
}

func assertListenerClosed(t *testing.T, srv *Server) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", srv.Addr().String(), time.Second)
	if err == nil {
		_ = conn.Close()
		t.Fatal("listener принимает соединения после остановки")
	}
}

func TestAddrNilBeforeOpen(t *testing.T) {
	srv := New(config.HTTP{Addr: "127.0.0.1:0"}, http.NotFoundHandler(), nil)
	if srv.Addr() != nil {
		t.Fatal("адрес доступен до открытия listener")
	}
}

// Произвольный отказ Accept нельзя надёжно воспроизвести через настоящий TCP.
// Обёртка проверяет передачу ошибки, возвращённой http.Server.Serve.
type failedAcceptListener struct {
	net.Listener
	err error
}

func (l failedAcceptListener) Accept() (net.Conn, error) {
	return nil, l.err
}

// Ошибка Accept — техническая ошибка Serve. Маршрут этой ошибки в результат
// запуска сервиса проверяют тесты сборки через общий механизм задач.
func TestServeReportsAcceptFailure(t *testing.T) {
	srv := newTestServer(t, http.NotFoundHandler())
	if err := srv.Open(t.Context()); err != nil {
		t.Fatal(err)
	}
	cause := &net.OpError{Op: "accept", Net: "tcp", Err: errors.New("управляемый отказ Accept")}
	srv.listener.Listener = failedAcceptListener{Listener: srv.listener.Listener, err: cause}
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve() }()
	if err := waitResult(t, serveDone); !errors.Is(err, cause) {
		t.Fatalf("ошибка Serve потеряна: %v", err)
	}
	assertListenerClosed(t, srv)
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

// Serve завершается без ошибки, когда Shutdown уже начался: результат
// Accept при начавшемся Shutdown не переопределяет штатный результат.
func TestServeResultAfterShutdown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
	}{
		{name: "окончательная сетевая ошибка скрыта остановкой",
			cause: &net.OpError{Op: "accept", Net: "tcp", Err: errors.New("окончательный сетевой отказ")}},
		{name: "штатное закрытие", cause: net.ErrClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t, http.NotFoundHandler())
			t.Cleanup(func() { _ = srv.Close() })
			if err := srv.Open(t.Context()); err != nil {
				t.Fatal(err)
			}
			listener := &closeTriggeredAcceptListener{
				Listener: srv.listener.Listener,
				entered:  make(chan struct{}), closed: make(chan struct{}), err: tc.cause,
			}
			srv.listener.Listener = listener
			serveDone := make(chan error, 1)
			go func() { serveDone <- srv.Serve() }()
			waitSignal(t, listener.entered)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if err := srv.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			if err := waitResult(t, serveDone); err != nil {
				t.Fatalf("штатная остановка Serve завершилась ошибкой: %v", err)
			}
			assertListenerClosed(t, srv)
		})
	}
}

// Зависший обработчик не продлевает остановку: по исчерпании бюджета
// Shutdown объединяет свою ошибку с принудительным закрытием соединений.
func TestShutdownForceClosesHungRequests(t *testing.T) {
	entered, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	releaseHandler := sync.OnceFunc(func() { close(release) })
	defer releaseHandler()
	srv := newTestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		defer close(exited)
		<-release
	}))
	if err := srv.Open(t.Context()); err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve() }()
	requestDone := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 2 * time.Second}
		defer client.CloseIdleConnections()
		response, err := client.Get("http://" + srv.Addr().String() + "/metrics/b/user-login/events")
		if err == nil {
			err = response.Body.Close()
		}
		requestDone <- err
	}()
	waitSignal(t, entered)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := srv.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("исчерпание бюджета не сообщено: %v", err)
	}
	if err := waitResult(t, requestDone); err == nil {
		t.Fatal("соединение зависшего запроса не закрыто принудительно")
	}
	assertListenerClosed(t, srv)
	releaseHandler()
	waitSignal(t, exited)
	if err := waitResult(t, serveDone); err != nil {
		t.Fatalf("Serve после принудительного закрытия завершился ошибкой: %v", err)
	}
}

type closeErrorListener struct {
	net.Listener
	err error
}

func (l closeErrorListener) Close() error {
	return errors.Join(l.Listener.Close(), l.err)
}

// Системный Close обычно успешен; подмена ограничена проверкой передачи
// ошибки закрытия владельцу остановки и однократности закрытия.
func TestCloseListenerPreservesErrorOnce(t *testing.T) {
	srv := newTestServer(t, http.NotFoundHandler())
	if err := srv.Open(t.Context()); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("сбой закрытия listener")
	srv.listener.Listener = closeErrorListener{Listener: srv.listener.Listener, err: cause}
	if err := srv.CloseListener(); !errors.Is(err, cause) {
		t.Fatalf("ошибка закрытия listener потеряна: %v", err)
	}
	if err := srv.CloseListener(); !errors.Is(err, cause) {
		t.Fatalf("повторное закрытие потеряло ошибку однократного закрытия: %v", err)
	}
}

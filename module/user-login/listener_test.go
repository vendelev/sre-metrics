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

// Устойчивый отказ Accept одновременно с отменой нельзя надёжно воспроизвести
// через TCP: закрытие сокета возвращает штатный net.ErrClosed. Обёртка нужна
// только для проверки сохранения исходной ошибки внутри HTTP-фазы.
type failedAcceptListener struct {
	net.Listener
	err    error
	cancel context.CancelFunc
}

func (l failedAcceptListener) Accept() (net.Conn, error) {
	l.cancel()
	return nil, l.err
}

func TestHTTPFailureWithCancellation(t *testing.T) {
	t.Parallel()
	r := newServiceReceiver(t)
	app := testService(t, actualConfig(r), clock.RealTime{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	t.Cleanup(func() { _ = app.stop(t.Context()) })
	if err := app.openListener(ctx); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("управляемый отказ Accept")
	app.listener.Listener = failedAcceptListener{Listener: app.listener.Listener, err: cause, cancel: cancel}
	if err := app.startSDK(ctx); err != nil {
		t.Fatal(err)
	}
	if err := app.startHTTP(ctx); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, app.serverTask.done)
	if err := app.stop(ctx); !errors.Is(err, cause) {
		t.Fatalf("сбой Accept потерян при отмене: %v", err)
	}
	assertListenerClosed(t, app)
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

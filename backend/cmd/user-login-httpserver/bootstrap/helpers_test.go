package bootstrap

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appconfig "github.com/vendelev/sre-metrics/cmd/user-login-httpserver/config"
	"github.com/vendelev/sre-metrics/core/config"
	"github.com/vendelev/sre-metrics/core/oteltest"
	userlogin "github.com/vendelev/sre-metrics/user-login/bootstrap"
	userloginconfig "github.com/vendelev/sre-metrics/user-login/config"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
}

// barrierClock приостанавливает одну предметную операцию на существующей
// границе часов. Это позволяет воспроизвести незавершённое обслуживание без
// хука в рабочем коде.
type barrierClock struct {
	*testClock
	armed                    atomic.Bool
	entered, release, exited chan struct{}
}

func newBarrierClock(now time.Time) *barrierClock {
	return &barrierClock{
		testClock: &testClock{now: now},
		entered:   make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{}),
	}
}

func (c *barrierClock) Now() time.Time {
	if c.armed.CompareAndSwap(true, false) {
		close(c.entered)
		<-c.release
		defer close(c.exited)
	}
	return c.testClock.Now()
}

// actualConfig настраивает настоящее окружение SDK; вызывающие тесты не
// запускаются параллельно.
func actualConfig(t *testing.T, r *oteltest.Receiver) (*userloginconfig.Config, config.HTTP, appconfig.Config) {
	t.Helper()
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", r.URL())
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TIMEOUT", "200")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE", "cumulative")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "20")
	t.Setenv("OTEL_METRIC_EXPORT_TIMEOUT", "200")
	t.Setenv(
		"OTEL_RESOURCE_ATTRIBUTES",
		"service.instance.id=service-test-instance,host.name=node-test,deployment.environment.name=test",
	)
	cfg := userloginconfig.Default()
	settings := config.Default()
	settings.Addr = "127.0.0.1:0"
	settings.ReadHeaderTimeout = 50 * time.Millisecond
	settings.ReadTimeout, settings.WriteTimeout = 100*time.Millisecond, 100*time.Millisecond
	process := appconfig.Default()
	process.ShutdownTimeout = 2 * time.Second
	return &cfg, settings, process
}

// testApp собирает настоящую сборку приложения с проверенной конфигурацией.
// Управляемые часы передаются как значение вызывающего кода: модуль собирает
// их через публичный контракт Clock.
func testApp(
	t *testing.T,
	process appconfig.Config,
	cfg *userloginconfig.Config,
	settings config.HTTP,
	c userlogin.Clock,
) *App {
	t.Helper()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := settings.Validate(); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	app, err := newApp(process, cfg, settings, logger, c)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

type runningService struct {
	cancel  context.CancelFunc
	done    chan struct{}
	err     error
	address net.Addr
}

func runActual(t *testing.T, app *App) *runningService {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	running := &runningService{cancel: cancel, done: make(chan struct{})}
	ready := make(chan net.Addr, 1)
	go func() { running.err = app.run(ctx, func(addr net.Addr) { ready <- addr }); close(running.done) }()
	t.Cleanup(func() { cancel(); waitSignal(t, running.done) })
	select {
	case running.address = <-ready:
	case <-running.done:
		t.Fatalf("ошибка запуска: %v", running.err)
	case <-time.After(3 * time.Second):
		t.Fatal("нет готовности сервиса")
	}
	return running
}

func (s *runningService) stop(t *testing.T) error {
	t.Helper()
	s.cancel()
	waitSignal(t, s.done)
	return s.err
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

func waitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("не дождались сигнала")
	}
}

func assertListenerClosed(t *testing.T, app *App) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", app.server.Addr().String(), time.Second)
	if err == nil {
		_ = conn.Close()
		t.Fatal("listener принимает соединения после остановки")
	}
}

// waitListenerClosed опрашивает адрес до первого отказа соединения. Закрытие
// listener — первый шаг остановки, поэтому отказ детерминированно означает,
// что протокол остановки начался и дойдёт до Shutdown и ожидания drain.
func waitListenerClosed(t *testing.T, address net.Addr) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", address.String(), time.Second)
		if err != nil {
			return
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("остановка не закрыла listener")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

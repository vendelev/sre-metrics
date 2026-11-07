package userlogin

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vendelev/sre-metrics/user-login/internal/application"
	"github.com/vendelev/sre-metrics/user-login/internal/domain/contract"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/clock"
	"github.com/vendelev/sre-metrics/user-login/internal/presentation/config"
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

// barrierClock приостанавливает одну предметную операцию на существующей границе
// часов. Это позволяет воспроизвести незавершённое обслуживание без хука в сервисе.
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

func TestNewServiceSharesState(t *testing.T) {
	t.Parallel()
	r := newServiceReceiver(t)
	cfg := actualConfig(r)
	cfg.MaintenanceInterval = 5 * time.Millisecond
	c := &testClock{now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	app := testService(t, cfg, c)
	running := runActual(t, app)
	if err := sendServiceEvent(t, running, 1, 100, "login_started", c.Now(), false); err != nil {
		t.Fatal(err)
	}
	c.Advance(15 * time.Minute)
	nextServiceMetrics(t, r, func(_ receivedMetrics, v serviceValues) bool {
		return v.completed["unknown"] == 1
	})
	if err := running.stop(t); err != nil {
		t.Fatal(err)
	}
	if lastServiceMetrics(t, r).completed["unknown"] != 1 {
		t.Fatal("обслуживание и HTTP не используют одно состояние")
	}
}

func testService(t *testing.T, cfg *config.Config, c contract.Clock) *service {
	t.Helper()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	app, err := newService(cfg, logger, c)
	if err != nil {
		t.Fatal(err)
	}
	if app.pipeline != nil || app.recorder.Reader() != nil {
		t.Fatal("SDK запущен при сборке")
	}
	return app
}

func TestNewServiceCopiesConfig(t *testing.T) {
	t.Parallel()
	cfg := actualConfig(newServiceReceiver(t))
	want := *cfg
	app := testService(t, cfg, clock.RealTime{})
	*cfg = config.Config{}
	if app.cfg != want || app.server.Addr != want.ListenAddr {
		t.Fatal("изменение исходной конфигурации повлияло на собранный сервис")
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

func waitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("не дождались сигнала")
	}
}

func TestRunBindFailure(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	r := newServiceReceiver(t)
	cfg := actualConfig(r)
	cfg.ListenAddr = listener.Addr().String()
	app := testService(t, cfg, clock.RealTime{})
	ready := false
	if err := app.run(t.Context(), func(net.Addr) { ready = true }); err == nil ||
		!strings.Contains(err.Error(), "запуск HTTP") {
		t.Fatalf("ошибка занятого адреса потеряна: %v", err)
	}
	if ready || app.pipeline != nil || app.maintenanceTask != nil || app.serverTask != nil {
		t.Fatal("после ошибки bind запущены компоненты или объявлена готовность")
	}
}

func TestRunCancelledBeforeStartup(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := newServiceReceiver(t)
	app := testService(t, actualConfig(r), clock.RealTime{})
	ready := false
	if err := app.run(ctx, func(net.Addr) { ready = true }); !errors.Is(err, context.Canceled) {
		t.Fatalf("отмена до запуска не возвращена как ошибка: %v", err)
	}
	if ready || app.listener != nil || app.pipeline != nil {
		t.Fatal("при отмене до запуска открыты ресурсы или объявлена готовность")
	}
}

// Реальный memory не выдаёт произвольную техническую ошибку. Подмена существующего
// domain.Store нужна только для проверки сохранения причины отказа обслуживания.
type failedStore struct {
	err    error
	cancel context.CancelFunc
}

func (s failedStore) Within(func(contract.State) error) error {
	if s.cancel != nil {
		s.cancel()
	}
	return s.err
}

func TestRunMaintenanceFailure(t *testing.T) {
	for _, withCancellation := range []bool{false, true} {
		name := "без отмены"
		if withCancellation {
			name = "одновременно с отменой"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newServiceReceiver(t)
			cfg := actualConfig(r)
			cfg.MaintenanceInterval = time.Millisecond
			app := testService(t, cfg, clock.RealTime{})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cause := errors.New("управляемый сбой обслуживания")
			store := failedStore{err: cause}
			if withCancellation {
				store.cancel = cancel
			}
			var err error
			app.observer, err = application.NewLoginObserver(store, clock.RealTime{})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- app.run(ctx, nil) }()
			if err := waitResult(t, done); !errors.Is(err, cause) {
				t.Fatalf("сбой обслуживания замаскирован: %v", err)
			}
			assertListenerClosed(t, app)
		})
	}
}

func TestRunShutdownBudget(t *testing.T) {
	t.Parallel()
	r := newServiceReceiver(t)
	app := testService(t, actualConfig(r), clock.RealTime{})
	app.cfg.ShutdownTimeout = 50 * time.Millisecond
	entered, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	releaseRequest := sync.OnceFunc(func() { close(release) })
	defer releaseRequest()
	app.server.Handler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		defer close(exited)
		<-release
	})
	running := runActual(t, app)
	requestDone := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 2 * time.Second}
		defer client.CloseIdleConnections()
		response, err := client.Get("http://" + running.address.String() + "/events")
		if err == nil {
			err = response.Body.Close()
		}
		requestDone <- err
	}()
	waitSignal(t, entered)
	if err := running.stop(t); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("исчерпание бюджета не сообщено: %v", err)
	}
	if err := waitResult(t, requestDone); err == nil {
		t.Fatal("соединение зависшего запроса не закрыто принудительно")
	}
	assertListenerClosed(t, app)
	releaseRequest()
	waitSignal(t, exited)
}

func TestNewServiceConstructorError(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.NodeName, cfg.Environment = "node-test", "test"
	cfg.MaxScenarios = 0
	app, err := newService(&cfg, slog.New(slog.DiscardHandler), clock.RealTime{})
	if err == nil || !strings.Contains(err.Error(), "лимиты сценариев и событий должны быть положительными") {
		t.Fatalf("ошибка конструктора memory потеряна: %v", err)
	}
	if app != nil {
		t.Fatal("после ошибки сборки возвращено частичное приложение")
	}
}

func assertListenerClosed(t *testing.T, app *service) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", app.listener.Addr().String(), time.Second)
	if err == nil {
		_ = conn.Close()
		t.Fatal("listener принимает соединения после остановки")
	}
}

// Проверка внутренних фаз не требует внедряемого сбоя готовности в рабочем коде.
// Отмена задаётся между фазами, затем вызывается тот же протокол очистки.
func TestStartupCancellationBetweenPhases(t *testing.T) {
	for _, phase := range []string{"после bind", "после SDK", "после обслуживания", "после HTTP"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			r := newServiceReceiver(t)
			cfg := actualConfig(r)
			cfg.OTLPExportInterval = time.Hour
			app := testService(t, cfg, clock.RealTime{})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			t.Cleanup(func() { _ = app.stop(t.Context()) })
			app.failed = make(chan struct{}, 1)
			ready := false
			app.ready = func(net.Addr) { ready = true }
			phases := []struct {
				name string
				run  func(context.Context) error
			}{
				{"после bind", app.openListener}, {"после SDK", app.startSDK},
				{"после обслуживания", app.startMaintenance}, {"после HTTP", app.startHTTP},
			}
			for _, step := range phases {
				if err := step.run(ctx); err != nil {
					t.Fatal(err)
				}
				if step.name == phase {
					cancel()
				}
				if ctx.Err() != nil {
					break
				}
			}
			if err := app.announceReady(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("отмена частичного запуска потеряна: %v", err)
			}
			if err := app.stop(ctx); err != nil {
				t.Fatal(err)
			}
			assertListenerClosed(t, app)
			if ready {
				t.Fatal("отменённый запуск объявлен готовым")
			}
			if phase == "после bind" {
				if app.pipeline != nil || r.calls.Load() != 0 {
					t.Fatal("SDK запущен до соответствующей фазы")
				}
			} else if lastServiceMetrics(t, r).histogram != nil {
				t.Fatal("фиктивное наблюдение при очистке")
			}
			before := r.calls.Load()
			if err := app.stop(ctx); err != nil || r.calls.Load() != before {
				t.Fatalf("повторная очистка изменила результат или повторила экспорт: %v", err)
			}
		})
	}
}

func TestStopConcurrentCallsShareCleanup(t *testing.T) {
	t.Parallel()
	r := newServiceReceiver(t)
	r.fail.Store(true)
	r.hold.Store(true)
	cfg := actualConfig(r)
	cfg.OTLPExportInterval = time.Hour
	app := testService(t, cfg, clock.RealTime{})
	running := runActual(t, app)
	release := sync.OnceFunc(func() { close(r.release) })
	defer release()
	running.cancel()
	waitSignal(t, r.entered)
	repeated := make(chan error, 1)
	attempted := make(chan struct{})
	go func() { close(attempted); repeated <- app.stop(t.Context()) }()
	waitSignal(t, attempted)
	release()
	first := running.stop(t)
	second := waitResult(t, repeated)
	if first == nil || second == nil || !errors.Is(first, second) {
		t.Fatalf("конкурентные остановки не сохранили одну ошибку: %v / %v", first, second)
	}
	if err := app.stop(t.Context()); !errors.Is(err, second) || r.calls.Load() != 1 {
		t.Fatal("повторная очистка изменила результат или повторила экспорт")
	}
}

// Для точной проверки бюджета достаточно фаз ожидания без SDK и сети.
// Реальный экспорт с оставшимся бюджетом проверяется в app_otlp_test.go.
func TestShutdownUsesOneBudget(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rollback   bool
		closeDelay time.Duration
	}{
		{name: "остановка"},
		{name: "очистка частичного запуска", rollback: true},
		{name: "медленный Close", closeDelay: 8 * time.Second},
		{name: "очистка с медленным Close", rollback: true, closeDelay: 8 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				app := &service{cfg: config.Default(), server: &http.Server{}}
				if tc.closeDelay > 0 {
					listener, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = listener.Close() }()
					app.listener = &ownedListener{
						Listener: delayedCloseListener{Listener: listener, delay: tc.closeDelay},
					}
				}
				app.serverTask = &backgroundTask{done: make(chan struct{})}
				app.maintenanceTask = &backgroundTask{done: make(chan struct{})}
				started := time.Now()
				go func() {
					time.Sleep(3 * time.Second)
					close(app.serverTask.done)
				}()
				var err error
				if tc.rollback {
					ctx, cancel := context.WithCancel(t.Context())
					cancel()
					// Уже открытые ресурсы представляют частичный запуск. Отказ
					// следующей фазы должен сам вызвать общую очистку из start.
					err = app.start(ctx)
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("исходная ошибка запуска потеряна: %v", err)
					}
				} else {
					err = app.stop(t.Context())
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("исчерпание общего бюджета потеряно: %v", err)
				}
				if elapsed := time.Since(started); elapsed != app.cfg.ShutdownTimeout {
					t.Fatalf("закрытие listener и ожидание задач получили отдельные бюджеты: %s", elapsed)
				}
				before := time.Now()
				if err := app.stop(t.Context()); !errors.Is(err, context.DeadlineExceeded) || time.Now() != before {
					t.Fatalf("повторная очистка получила новый бюджет: %v", err)
				}
				close(app.maintenanceTask.done)
			})
		})
	}
}

// Управляемая задержка Close нужна для точной проверки расходования бюджета;
// ядро ОС не предоставляет надёжного способа задержать закрытие TCP-listener.
type delayedCloseListener struct {
	net.Listener
	delay time.Duration
}

func (l delayedCloseListener) Close() error {
	time.Sleep(l.delay)
	return l.Listener.Close()
}

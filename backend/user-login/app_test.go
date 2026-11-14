package userlogin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/vendelev/sre-metrics/user-login/internal/application"
	"github.com/vendelev/sre-metrics/user-login/internal/domain/contract"
	dEvent "github.com/vendelev/sre-metrics/user-login/internal/domain/event"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/clock"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/httpserver"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/queue"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/worker"
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
	r := newServiceReceiver(t)
	cfg := actualConfig(t, r)
	cfg.MaintenanceInterval = 5 * time.Millisecond
	c := &testClock{now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	app := testService(t, cfg, c)
	running := runActual(t, app)
	if err := sendServiceEvent(t, running, 1, 100, "login_started", c.Now()); err != nil {
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

// AC-50–AC-51: настоящая сборка отвечает до обработки, переполнение не ставит
// событие в очередь, а штатный drain включает принятые события в последний OTLP-пакет.
func TestServiceAsyncAdmissionAndQueueDrain(t *testing.T) {
	r := newServiceReceiver(t)
	cfg := actualConfig(t, r)
	cfg.EventQueueSize = 2
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
	c := newBarrierClock(time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC))
	app := testService(t, cfg, c)
	now := c.Now()
	// Первое событие попадает в очередь до запуска воркера; барьер затем
	// останавливает именно Process, а не проверку возраста новых запросов.
	first := &dEvent.Request{
		EventID:    uuid.MustParse("019aabcd-0000-7000-8000-000000000001"),
		ScenarioID: uuid.MustParse("019aabcd-0000-7000-9000-000000000064"),
		OccurredAt: now.Add(-5 * time.Second),
		Type:       dEvent.LoginStarted,
		Source:     dEvent.SourceMIS,
	}
	if err := app.eventQueue.Enqueue(first); err != nil {
		t.Fatal(err)
	}
	c.armed.Store(true)
	release := sync.OnceFunc(func() { close(c.release) })
	defer release()
	running := runActual(t, app)
	waitSignal(t, c.entered)
	if err := sendServiceEvent(t, running, 2, 100, "login_mode_determined", now); err != nil {
		t.Fatal(err)
	}
	if err := sendServiceEvent(t, running, 3, 100, "login_succeeded", now); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		at     time.Time
		status int
		body   string
	}{
		{"переполнение", now, 503, "{\"error\":\"overloaded\"}\n"},
		{"возраст раньше вместимости", now.Add(-31 * time.Minute), 400, "{\"error\":\"invalid_event\"}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Timeout: time.Second}
			defer client.CloseIdleConnections()
			body := fmt.Sprintf(
				`{"event_id":"019aabcd-0000-7000-8000-000000000004","scenario_id":"019aabcd-0000-7000-9000-000000000064","occurred_at":%q,"source":"mis","type":"login_failed"}`,
				tc.at.Format("2006-01-02T15:04:05.000Z"),
			)
			response, err := client.Post(
				"http://"+running.address.String()+"/events",
				"application/json",
				strings.NewReader(body),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			data, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != tc.status || string(data) != tc.body ||
				response.Header.Get("Content-Type") != "application/json" {
				t.Fatalf("ответ: %d %s", response.StatusCode, data)
			}
		})
	}
	if len(app.eventQueue.Events()) != 2 {
		t.Fatal("отказ изменил очередь или HTTP дождался обработки")
	}
	// Закрытие listener — первый шаг остановки: отказ соединения означает,
	// что протокол остановки начался и дойдёт до Shutdown и ожидания drain.
	running.cancel()
	waitListenerClosed(t, running.address)
	select {
	case <-running.done:
		t.Fatal("остановка не дождалась воркера")
	default:
	}
	release()
	if err := running.stop(t); err != nil {
		t.Fatal(err)
	}
	final := lastServiceMetrics(t, r)
	if final.completed["success"] != 1 || final.completed["technical_error"] != 0 || final.histogram == nil ||
		final.histogram.Count != 1 ||
		final.histogram.GetSum() != 5 {
		t.Fatalf("drain не включён в последний пакет: %+v", final)
	}
	if len(app.eventQueue.Events()) != 0 {
		t.Fatal("штатная остановка оставила события")
	}
}

func TestServiceQueuedProcessingFailure(t *testing.T) {
	r := newServiceReceiver(t)
	c := &testClock{now: time.Now()}
	cfg := actualConfig(t, r)
	app := testService(t, cfg, c)
	cause := errors.New("синтетический отказ обработки очереди")
	observer, err := application.NewLoginObserver(failedStore{err: cause}, c)
	if err != nil {
		t.Fatal(err)
	}
	app.worker = mustWorker(t, observer, app.eventQueue, cfg.MaintenanceInterval)
	if err := app.eventQueue.Enqueue(ingestEvent(1, dEvent.LoginSucceeded, c.Now())); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- app.run(t.Context(), nil) }()
	if err := waitResult(t, done); !errors.Is(err, cause) {
		t.Fatalf("сбой очереди не остановил сервис: %v", err)
	}
	assertListenerClosed(t, app)
}

func TestServiceQueueDrainDeadline(t *testing.T) {
	r := newServiceReceiver(t)
	cfg := actualConfig(t, r)
	cfg.ShutdownTimeout = 100 * time.Millisecond
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
	c := newBarrierClock(time.Now())
	app := testService(t, cfg, c)
	for id := 1; id <= 2; id++ {
		if err := app.eventQueue.Enqueue(ingestEvent(id, dEvent.LoginSucceeded, c.Now())); err != nil {
			t.Fatal(err)
		}
	}
	c.armed.Store(true)
	release := sync.OnceFunc(func() { close(c.release) })
	defer release()
	running := runActual(t, app)
	waitSignal(t, c.entered)
	if err := running.stop(t); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("незавершённая очередь скрыта: %v", err)
	}
	if err := app.eventQueue.Enqueue(ingestEvent(3, dEvent.LoginSucceeded, c.testClock.Now())); err == nil {
		t.Fatal("после остановки приём открыт")
	}
	release()
	waitSignal(t, app.workerTask.done)
	if app.workerTask.err != nil {
		t.Fatalf("отмена по бюджету добавила ошибку воркера: %v", app.workerTask.err)
	}
	if len(app.eventQueue.Events()) != 1 {
		t.Fatal("после исчерпания бюджета запущено следующее событие")
	}
}

// AC-39: даже уже сохранённый ID не освобождается от проверки возраста при приёме.
func TestServiceStoredDuplicateAge(t *testing.T) {
	r := newServiceReceiver(t)
	c := &testClock{now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	app := testService(t, actualConfig(t, r), c)
	running := runActual(t, app)
	at := c.Now().Add(-29 * time.Minute)
	if err := sendServiceEvent(t, running, 1, 100, "login_succeeded", at); err != nil {
		t.Fatal(err)
	}
	nextServiceMetrics(t, r, func(_ receivedMetrics, v serviceValues) bool { return v.completed["success"] == 1 })
	c.Advance(2 * time.Minute)
	body := fmt.Sprintf(
		`{"event_id":"019aabcd-0000-7000-8000-000000000001","scenario_id":"019aabcd-0000-7000-9000-000000000064","occurred_at":%q,"source":"mis","type":"login_succeeded"}`,
		at.Format("2006-01-02T15:04:05.000Z"),
	)
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Post(
		"http://"+running.address.String()+"/events",
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 400 || string(data) != "{\"error\":\"invalid_event\"}\n" {
		t.Fatalf("старый сохранённый повтор: %d %s", response.StatusCode, data)
	}
	if err := running.stop(t); err != nil {
		t.Fatal(err)
	}
	if lastServiceMetrics(t, r).completed["success"] != 1 {
		t.Fatal("старый повтор изменил бизнес-агрегат")
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

// mustWorker собирает воркера для сервисных проверок с подменённым наблюдателем.
func mustWorker(
	t *testing.T,
	observer *application.LoginObserver,
	events *queue.EventQueue,
	interval time.Duration,
) *worker.Worker {
	t.Helper()
	w, err := worker.New(observer, events, interval, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// idleWorker собирает настоящего воркера для синтетических проверок протокола
// остановки: его операции не вызываются, но компоненты настоящие.
func idleWorker(t *testing.T) *worker.Worker {
	t.Helper()
	observer, err := application.NewLoginObserver(failedStore{}, clock.RealTime{})
	if err != nil {
		t.Fatal(err)
	}
	return mustWorker(t, observer, queue.NewEventQueue(1), time.Second)
}

// ingestEvent воспроизводит событие приёма для сервисных проверок очереди.
func ingestEvent(id int, kind dEvent.Type, at time.Time) *dEvent.Request {
	return &dEvent.Request{
		EventID:    uuid.MustParse(fmt.Sprintf("019aabcd-0000-7000-8000-%012x", id)),
		ScenarioID: uuid.MustParse("019aabcd-0000-7000-9000-000000000100"),
		OccurredAt: at,
		Type:       kind,
		Source:     dEvent.SourceMIS,
	}
}

func TestNewServiceCopiesConfig(t *testing.T) {
	cfg := actualConfig(t, newServiceReceiver(t))
	want := *cfg
	app := testService(t, cfg, clock.RealTime{})
	*cfg = config.Config{}
	if app.cfg != want {
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
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	r := newServiceReceiver(t)
	cfg := actualConfig(t, r)
	cfg.ListenAddr = listener.Addr().String()
	app := testService(t, cfg, clock.RealTime{})
	ready := false
	if err := app.run(t.Context(), func(net.Addr) { ready = true }); err == nil ||
		!strings.Contains(err.Error(), "запуск HTTP") {
		t.Fatalf("ошибка занятого адреса потеряна: %v", err)
	}
	if ready || app.pipeline != nil || app.workerTask != nil || app.serverTask != nil {
		t.Fatal("после ошибки bind запущены компоненты или объявлена готовность")
	}
}

func TestRunCancelledBeforeStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := newServiceReceiver(t)
	app := testService(t, actualConfig(t, r), clock.RealTime{})
	ready := false
	if err := app.run(ctx, func(net.Addr) { ready = true }); !errors.Is(err, context.Canceled) {
		t.Fatalf("отмена до запуска не возвращена как ошибка: %v", err)
	}
	if ready || app.httpServer.Addr() != nil || app.pipeline != nil {
		t.Fatal("при отмене до запуска открыты ресурсы или объявлена готовность")
	}
}

// Реальный memory не выдаёт произвольную техническую ошибку. Подмена существующего
// domain.Store нужна только для проверки сохранения причины отказа обслуживания.
type failedStore struct {
	contract.Store
	err    error
	cancel context.CancelFunc
}

func (s failedStore) RemoveRetained(time.Time) error {
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
			r := newServiceReceiver(t)
			cfg := actualConfig(t, r)
			cfg.MaintenanceInterval = time.Millisecond
			app := testService(t, cfg, clock.RealTime{})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cause := errors.New("управляемый сбой обслуживания")
			store := failedStore{err: cause}
			if withCancellation {
				store.cancel = cancel
			}
			observer, err := application.NewLoginObserver(store, clock.RealTime{})
			if err != nil {
				t.Fatal(err)
			}
			app.worker = mustWorker(t, observer, app.eventQueue, cfg.MaintenanceInterval)
			done := make(chan error, 1)
			go func() { done <- app.run(ctx, nil) }()
			if err := waitResult(t, done); !errors.Is(err, cause) {
				t.Fatalf("сбой обслуживания замаскирован: %v", err)
			}
			assertListenerClosed(t, app)
		})
	}
}

func TestNewServiceConstructorError(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
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
	conn, err := net.DialTimeout("tcp", app.httpServer.Addr().String(), time.Second)
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

// Проверка внутренних фаз не требует внедряемого сбоя готовности в рабочем коде.
// Отмена задаётся между фазами, затем вызывается тот же протокол очистки.
func TestStartupCancellationBetweenPhases(t *testing.T) {
	for _, phase := range []string{"после bind", "после SDK", "после воркера", "после HTTP"} {
		t.Run(phase, func(t *testing.T) {
			r := newServiceReceiver(t)
			cfg := actualConfig(t, r)
			t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
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
				{"после bind", app.httpServer.Open}, {"после SDK", app.startSDK},
				{"после воркера", app.startWorker}, {"после HTTP", app.startHTTP},
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
	r := newServiceReceiver(t)
	r.fail.Store(true)
	r.hold.Store(true)
	cfg := actualConfig(t, r)
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
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
// Реальный экспорт с оставшимся бюджетом проверяется в app_otlp_test.go,
// механизм listener и его закрытие — в пакете httpserver.
func TestShutdownUsesOneBudget(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rollback bool
	}{
		{name: "остановка"},
		{name: "очистка частичного запуска", rollback: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				app := &service{
					cfg:        config.Default(),
					httpServer: httpserver.New(httpserver.Config{}, http.NotFoundHandler(), log.New(io.Discard, "", 0)),
					worker:     idleWorker(t),
					eventQueue: queue.NewEventQueue(1),
				}
				app.serverTask = &backgroundTask{done: make(chan struct{})}
				app.workerTask = &backgroundTask{done: make(chan struct{})}
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
				close(app.workerTask.done)
			})
		})
	}
}

// Системный Close listener обычно успешен; передачу его ошибки проверяет
// пакет httpserver. Здесь проверяется, что при отмене запуска ошибка
// финального экспорта SDK сохраняется, а повторная очистка её не повторяет.
func TestCancellationPreservesExportError(t *testing.T) {
	r := newServiceReceiver(t)
	r.fail.Store(true)
	cfg := actualConfig(t, r)
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
	app := testService(t, cfg, clock.RealTime{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	t.Cleanup(func() { _ = app.stop(t.Context()) })
	if err := app.httpServer.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if err := app.startSDK(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	err := app.stop(ctx)
	// Контекст остановки не наследует отмену запуска: исчерпание бюджета
	// сообщается отдельно, но ошибка финального экспорта сохраняется в нём.
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "освобождение SDK метрик") {
		t.Fatalf("ошибка финального экспорта потеряна при отмене: %v", err)
	}
	if r.calls.Load() != 1 {
		t.Fatal("финальная отправка не состоялась или повторилась")
	}
	assertListenerClosed(t, app)
}

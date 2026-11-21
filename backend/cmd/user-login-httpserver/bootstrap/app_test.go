package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	appconfig "github.com/vendelev/sre-metrics/cmd/user-login-httpserver/config"
	coreconfig "github.com/vendelev/sre-metrics/core/config"
	"github.com/vendelev/sre-metrics/core/oteltest"
	userloginconfig "github.com/vendelev/sre-metrics/user-login/config"
)

func TestAppSharesState(t *testing.T) {
	r := oteltest.NewReceiver(t)
	cfg, settings, process := actualConfig(t, r)
	cfg.Processing.MaintenanceInterval = 5 * time.Millisecond
	c := &testClock{now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	app := testApp(t, process, cfg, settings, c)
	running := runActual(t, app)
	if err := sendServiceEvent(t, running, 1, 100, "login_started", c.Now()); err != nil {
		t.Fatal(err)
	}
	c.Advance(15 * time.Minute)
	nextServiceMetrics(t, r, func(_ oteltest.Sample, v serviceValues) bool {
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
// событие в очередь, а штатный drain включает принятые события в последний
// OTLP-пакет. Барьер удерживает воркера в первом тике обслуживания: принятые
// при занятом воркере события обрабатываются в drain.
func TestAppAsyncAdmissionAndQueueDrain(t *testing.T) {
	r := oteltest.NewReceiver(t)
	cfg, settings, process := actualConfig(t, r)
	// Интервал обслуживания больше сегмента отправки: воркер детерминированно
	// уходит в барьер первым тиком, не успевая взять события из очереди.
	cfg.Processing.EventQueueSize = 3
	cfg.Processing.MaintenanceInterval = time.Second
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
	c := newBarrierClock(time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC))
	app := testApp(t, process, cfg, settings, c)
	now := c.Now()
	c.armed.Store(true)
	release := sync.OnceFunc(func() { close(c.release) })
	defer release()
	running := runActual(t, app)
	waitSignal(t, c.entered)
	for _, event := range []struct {
		id   int
		kind string
		at   time.Time
	}{
		{1, "login_started", now.Add(-5 * time.Second)},
		{2, "login_mode_determined", now},
		{3, "login_succeeded", now},
	} {
		if err := sendServiceEvent(t, running, event.id, 100, event.kind, event.at); err != nil {
			t.Fatal(err)
		}
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
				"http://"+running.address.String()+"/metrics/b/user-login/events",
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
	// Отклонённое при переполнении событие не изменило обработанные итоги.
	final := lastServiceMetrics(t, r)
	if final.completed["success"] != 1 || final.completed["technical_error"] != 0 || final.histogram == nil ||
		final.histogram.Count != 1 ||
		final.histogram.GetSum() != 5 {
		t.Fatalf("drain не включён в последний пакет: %+v", final)
	}
}

// Очередь приёма после исчерпания бюджета остановки не продолжает обработку:
// следующий запрос не проходит, а оставшиеся события не обрабатываются.
// Поведение остатка очереди и ошибки воркера проверяется рядом с bootstrap
// модуля.
func TestAppQueueDrainDeadline(t *testing.T) {
	r := oteltest.NewReceiver(t)
	cfg, settings, process := actualConfig(t, r)
	process.ShutdownTimeout = 100 * time.Millisecond
	cfg.Processing.MaintenanceInterval = time.Second
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
	// Часы в UTC: литеральная Z в формате тела означает UTC, а локальная зона
	// сместила бы occurred_at и отклонила бы возраст события.
	c := newBarrierClock(time.Now().UTC())
	app := testApp(t, process, cfg, settings, c)
	c.armed.Store(true)
	release := sync.OnceFunc(func() { close(c.release) })
	defer release()
	running := runActual(t, app)
	// Воркер уходит в барьер первым тиком обслуживания, до отправки событий:
	// время аргументов не конкурирует за барьер с воркером.
	waitSignal(t, c.entered)
	if err := sendServiceEvent(t, running, 1, 100, "login_succeeded", c.Now()); err != nil {
		t.Fatal(err)
	}
	if err := sendServiceEvent(t, running, 2, 100, "login_succeeded", c.Now()); err != nil {
		t.Fatal(err)
	}
	if err := running.stop(t); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("незавершённая очередь скрыта: %v", err)
	}
	// После закрытия listener приём недоступен.
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	body := fmt.Sprintf(
		`{"event_id":"019aabcd-0000-7000-8000-000000000003","scenario_id":"019aabcd-0000-7000-9000-000000000100","occurred_at":%q,"source":"mis","type":"login_succeeded"}`,
		c.Now().Format("2006-01-02T15:04:05.000Z"),
	)
	response, err := client.Post(
		"http://"+running.address.String()+"/metrics/b/user-login/events",
		"application/json",
		strings.NewReader(body),
	)
	if err == nil {
		_ = response.Body.Close()
		t.Fatal("после остановки приём открыт")
	}
	release()
}

// AC-39: даже уже сохранённый ID не освобождается от проверки возраста при приёме.
func TestAppStoredDuplicateAge(t *testing.T) {
	r := oteltest.NewReceiver(t)
	cfg, settings, process := actualConfig(t, r)
	c := &testClock{now: time.Now().UTC()}
	app := testApp(t, process, cfg, settings, c)
	running := runActual(t, app)
	at := c.Now().Add(-29 * time.Minute)
	if err := sendServiceEvent(t, running, 1, 100, "login_succeeded", at); err != nil {
		t.Fatal(err)
	}
	nextServiceMetrics(t, r, func(_ oteltest.Sample, v serviceValues) bool { return v.completed["success"] == 1 })
	c.Advance(2 * time.Minute)
	body := fmt.Sprintf(
		`{"event_id":"019aabcd-0000-7000-8000-000000000001","scenario_id":"019aabcd-0000-7000-9000-000000000064","occurred_at":%q,"source":"mis","type":"login_succeeded"}`,
		at.Format("2006-01-02T15:04:05.000Z"),
	)
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Post(
		"http://"+running.address.String()+"/metrics/b/user-login/events",
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

func TestAppRunBindFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	r := oteltest.NewReceiver(t)
	cfg, settings, process := actualConfig(t, r)
	settings.Addr = listener.Addr().String()
	app := testApp(t, process, cfg, settings, &testClock{now: time.Now()})
	ready := false
	if err := app.run(t.Context(), func(net.Addr) { ready = true }); err == nil ||
		!strings.Contains(err.Error(), "запуск HTTP") {
		t.Fatalf("ошибка занятого адреса потеряна: %v", err)
	}
	if ready || r.Calls() != 0 || app.module.WorkerTask() != nil || app.server.ServeTask() != nil {
		t.Fatal("после ошибки bind запущены компоненты или объявлена готовность")
	}
}

func TestAppRunCancelledBeforeStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := oteltest.NewReceiver(t)
	cfg, settings, process := actualConfig(t, r)
	app := testApp(t, process, cfg, settings, &testClock{now: time.Now()})
	ready := false
	if err := app.run(ctx, func(net.Addr) { ready = true }); !errors.Is(err, context.Canceled) {
		t.Fatalf("отмена до запуска не возвращена как ошибка: %v", err)
	}
	if ready || app.server.Addr() != nil || r.Calls() != 0 {
		t.Fatal("при отмене до запуска открыты ресурсы или объявлена готовность")
	}
}

func TestAppInboxUnavailableBeforeReady(t *testing.T) {
	r := oteltest.NewReceiver(t)
	cfg, settings, process := actualConfig(t, r)
	cfg.Inbox.Dir = filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(cfg.Inbox.Dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	app := testApp(t, process, cfg, settings, &testClock{now: time.Now()})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	ready := false
	err := app.run(ctx, func(net.Addr) { ready = true })
	if _, ok := errors.AsType[*os.PathError](err); !ok {
		t.Fatalf("ошибка доступа к каталогу не сохранена: %v", err)
	}
	if ready || app.server.ServeTask() != nil {
		t.Fatal("ошибка каталога не остановила запуск до HTTP и готовности")
	}
	assertListenerClosed(t, app)
}

func TestAppInboxDisappearsAfterReady(t *testing.T) {
	r := oteltest.NewReceiver(t)
	cfg, settings, process := actualConfig(t, r)
	cfg.Inbox.ScanInterval = 10 * time.Millisecond
	app := testApp(t, process, cfg, settings, &testClock{now: time.Now()})
	running := runActual(t, app)
	if err := os.Remove(cfg.Inbox.Dir); err != nil {
		t.Fatal(err)
	}
	// Запуск ещё не отменён: процесс должен завершиться по сигналу задачи.
	waitSignal(t, running.done)
	if !errors.Is(running.err, os.ErrNotExist) || !strings.Contains(running.err.Error(), "при сканировании") {
		t.Fatalf("ошибка исчезнувшего каталога потеряна: %v", running.err)
	}
	assertListenerClosed(t, app)
}

// AC-31: блокируем чтение часов только в обработке файла. Остановка не
// дожидается произвольной Go-функции сверх общего бюджета; SDK не получает новый
// срок, а запоздалое событие не принимается в закрытую очередь.
func TestAppBlockedInboxSharesShutdownBudget(t *testing.T) {
	r := oteltest.NewReceiver(t)
	r.SetHold(true)
	defer r.Release()
	cfg, settings, process := actualConfig(t, r)
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TIMEOUT", "5000")
	cfg.Inbox.Dir = t.TempDir()
	cfg.Processing.MaintenanceInterval = time.Minute
	process.ShutdownTimeout = 400 * time.Millisecond
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	path := publishInboxFile(t, cfg.Inbox.Dir, "019aabcd-0000-7000-8000-000000000001",
		serviceEventJSON(1, 100, "login_succeeded", now))
	c := newBarrierClock(now)
	app := testApp(t, process, cfg, settings, c)
	release := sync.OnceFunc(func() { close(c.release) })
	defer release()
	c.armed.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	running := &runningService{cancel: cancel, done: make(chan struct{})}
	go func() {
		running.err = app.run(ctx, func(addr net.Addr) { running.address = addr })
		close(running.done)
	}()
	t.Cleanup(func() { cancel(); waitSignal(t, running.done) })
	waitSignal(t, c.entered)
	inbox := app.module.InboxTask()
	if inbox == nil {
		t.Fatal("нет задачи файлового входа")
	}
	defer func() { release(); waitSignal(t, inbox.Done()) }()

	started := time.Now()
	err := running.stop(t)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "завершения файлового входа") {
		t.Fatalf("незавершённый файловый вход скрыт: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 3*process.ShutdownTimeout/2 {
		t.Fatalf("остановка вышла за общий бюджет: %s", elapsed)
	}
	select {
	case <-inbox.Done():
		t.Fatal("блокировка файлового входа снялась раньше времени")
	default:
	}
	release()
	waitSignal(t, inbox.Done())
	if inbox.Err() != nil {
		t.Fatalf("файловый вход не остановился штатно после снятия блокировки: %v", inbox.Err())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("файл удалён без постановки в очередь: %v", err)
	}
}

// Ошибка конструктора memory теряется при сборке контейнера и останавливает
// запуск до открытия ресурсов.
func TestAppConstructorError(t *testing.T) {
	t.Parallel()
	cfg := userloginconfig.Default()
	cfg.Processing.MaxScenarios = 0
	app, err := newApp(
		appconfig.Default(),
		&cfg,
		coreconfig.Default(),
		slog.New(slog.DiscardHandler),
		&testClock{now: time.Now()},
	)
	if err == nil || !strings.Contains(err.Error(), "лимиты сценариев и событий должны быть положительными") {
		t.Fatalf("ошибка конструктора memory потеряна: %v", err)
	}
	if app != nil {
		t.Fatal("после ошибки сборки возвращено частичное приложение")
	}
}

func TestAppInboxConstructorError(t *testing.T) {
	t.Parallel()
	cfg := userloginconfig.Default()
	cfg.Inbox.Dir = filepath.Join(t.TempDir(), "inbox")
	cfg.Inbox.ScanInterval = 0
	app, err := newApp(appconfig.Default(), &cfg, coreconfig.Default(),
		slog.New(slog.DiscardHandler), &testClock{now: time.Now()})
	if app != nil || err == nil || !strings.Contains(err.Error(), "сборка файлового входа") {
		t.Fatalf("ошибка сборки файлового входа потеряна: %v", err)
	}
	if _, err := os.Stat(cfg.Inbox.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("сборка открыла каталог: %v", err)
	}
}

// Проверка внутренних фаз не требует внедряемого сбоя готовности в рабочем
// коде. Отмена задаётся между фазами, затем вызывается тот же протокол
// очистки.
func TestAppStartupCancellationBetweenPhases(t *testing.T) {
	for _, phase := range []string{"после bind", "после SDK", "после воркера", "после файлового входа", "после HTTP"} {
		t.Run(phase, func(t *testing.T) {
			r := oteltest.NewReceiver(t)
			cfg, settings, process := actualConfig(t, r)
			t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
			app := testApp(t, process, cfg, settings, &testClock{now: time.Now()})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			t.Cleanup(func() { _ = app.stop(t.Context()) })
			ready := false
			app.ready = func(net.Addr) { ready = true }
			phases := []struct {
				name string
				run  func(context.Context) error
			}{
				{"после bind", app.server.Open}, {"после SDK", app.startTelemetry},
				{"после воркера", app.module.StartWorker}, {"после файлового входа", app.module.StartInbox},
				{"после HTTP", app.server.StartServe},
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
			if inbox := app.module.InboxTask(); inbox != nil {
				select {
				case <-inbox.Done():
				default:
					t.Fatal("очистка частичного запуска не дождалась файлового входа")
				}
			}
			assertListenerClosed(t, app)
			if ready {
				t.Fatal("отменённый запуск объявлен готовым")
			}
			if phase == "после bind" {
				if r.Calls() != 0 {
					t.Fatal("SDK запущен до соответствующей фазы")
				}
			} else if lastServiceMetrics(t, r).histogram != nil {
				t.Fatal("фиктивное наблюдение при очистке")
			}
			before := r.Calls()
			if err := app.stop(ctx); err != nil || r.Calls() != before {
				t.Fatalf("повторная очистка изменила результат или повторила экспорт: %v", err)
			}
		})
	}
}

func TestStopConcurrentCallsShareCleanup(t *testing.T) {
	r := oteltest.NewReceiver(t)
	r.SetFail(true)
	r.SetHold(true)
	cfg, settings, process := actualConfig(t, r)
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
	app := testApp(t, process, cfg, settings, &testClock{now: time.Now()})
	running := runActual(t, app)
	release := sync.OnceFunc(r.Release)
	defer release()
	running.cancel()
	waitSignal(t, r.Entered())
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
	if err := app.stop(t.Context()); !errors.Is(err, second) || r.Calls() != 1 {
		t.Fatal("повторная очистка изменила результат или повторила экспорт")
	}
}

// Системный Close listener обычно успешен; передачу его ошибки проверяет
// пакет httpserver. Здесь проверяется, что при отмене запуска ошибка
// финального экспорта SDK сохраняется, а повторная очистка её не повторяет.
func TestCancellationPreservesExportError(t *testing.T) {
	r := oteltest.NewReceiver(t)
	r.SetFail(true)
	cfg, settings, process := actualConfig(t, r)
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
	app := testApp(t, process, cfg, settings, &testClock{now: time.Now()})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	t.Cleanup(func() { _ = app.stop(t.Context()) })
	if err := app.server.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if err := app.startTelemetry(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	err := app.stop(ctx)
	// Контекст остановки не наследует отмену запуска: исчерпание бюджета
	// сообщается отдельно, но ошибка финального экспорта сохраняется в нём.
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "освобождение SDK метрик") {
		t.Fatalf("ошибка финального экспорта потеряна при отмене: %v", err)
	}
	if r.Calls() != 1 {
		t.Fatal("финальная отправка не состоялась или повторилась")
	}
	assertListenerClosed(t, app)
}

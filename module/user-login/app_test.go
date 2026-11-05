package userlogin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/vendelev/sre-metrics/user-login/internal/application"
	"github.com/vendelev/sre-metrics/user-login/internal/domain"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/clock"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/otel"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/otel/oteltest"
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

func TestNewServiceSharesState(t *testing.T) {
	synctest.Test(t, testSharedState)
}

func testSharedState(t *testing.T) {
	t.Helper()
	clock := &testClock{now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	cfg := config.Default()
	cfg.NodeName, cfg.Environment = "node-test", "test"
	cfg.ListenAddr = "127.0.0.1:0"
	app, err := newService(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), clock)
	if err != nil {
		t.Fatal(err)
	}
	useLocalPipeline(app)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	listener := newControlledListener()
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	options := listenerOptions(listener)
	options.ready = func(addr net.Addr) { ready <- addr }
	go func() { done <- app.run(ctx, options) }()
	if address := <-ready; address.String() != listener.Addr().String() {
		t.Fatalf("неверный адрес готовности: %v", address)
	}
	synctest.Wait()
	body := fmt.Sprintf(
		`{"version":1,"event_id":"019aabcd-0000-7000-8000-000000000001","scenario_id":"019aabcd-0000-7000-8000-000000000100","occurred_at":%q,"source":"mis","type":"login_started"}`,
		clock.Now().Format("2006-01-02T15:04:05.000Z"),
	)
	r := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	app.server.Handler.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("HTTP-приём: статус %d, тело %s", w.Code, w.Body.String())
	}
	clock.Advance(15 * time.Minute)
	timer := time.NewTimer(cfg.MaintenanceInterval)
	defer timer.Stop()
	<-timer.C
	synctest.Wait()
	snapshot := oteltest.Read(t, app.recorder)
	if snapshot.Completed[domain.OutcomeUnknown] != 1 {
		t.Fatalf("обслуживание не закрыло сценарий, принятый HTTP: %+v", snapshot.Completed)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func testService(t *testing.T, cfg config.Config) *service {
	t.Helper()
	cfg.NodeName, cfg.Environment = "node-test", "test"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	app, err := newService(cfg, logger, clock.Real{})
	if err != nil {
		t.Fatal(err)
	}
	useLocalPipeline(app)
	return app
}

// Локальный SDK подменяет только сетевую границу в прежних lifecycle-тестах.
// Реальный production-путь проверяется отдельно с HTTP-получателем protobuf.
type localPipeline struct {
	recorder *otel.Recorder
	err      error
}

func (p *localPipeline) Shutdown(ctx context.Context) error {
	return errors.Join(p.err, p.recorder.Shutdown(ctx))
}
func useLocalPipeline(app *service) {
	app.openPipeline = func(_ context.Context, recorder *otel.Recorder) (metricPipeline, error) {
		if err := recorder.StartLocal(); err != nil {
			return nil, err
		}
		return &localPipeline{recorder: recorder}, nil
	}
}

func listenerOptions(listener net.Listener) runOptions {
	return runOptions{listen: func(context.Context) (net.Listener, error) { return listener, nil }}
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

func TestRunHTTPAndGracefulShutdown(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.ListenAddr = "127.0.0.1:0"
	app := testService(t, cfg)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	type collection struct {
		data metricdata.ResourceMetrics
		err  error
	}
	collected := make(chan collection, 1)
	releaseRequest := sync.OnceFunc(func() { close(release) })
	defer func() { releaseRequest(); waitSignal(t, exited) }()
	handler := app.server.Handler
	app.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		defer close(exited)
		<-release
		handler.ServeHTTP(w, r)
		var data metricdata.ResourceMetrics
		err := app.recorder.Reader().Collect(context.Background(), &data)
		collected <- collection{data: data, err: err}
	})
	shutdownStarted := make(chan struct{})
	app.server.RegisterOnShutdown(func() { close(shutdownStarted) })
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() { done <- app.run(ctx, runOptions{ready: func(addr net.Addr) { ready <- addr }}) }()
	var address net.Addr
	select {
	case address = <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("сервер не сообщил о готовности")
	}
	requestDone := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 2 * time.Second}
		defer client.CloseIdleConnections()
		body := fmt.Sprintf(
			`{"version":1,"event_id":"019aabcd-0000-7000-8000-000000000001","scenario_id":"019aabcd-0000-7000-8000-000000000100","occurred_at":%q,"source":"mis","type":"login_succeeded"}`,
			time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		)
		response, err := client.Post("http://"+address.String()+"/events", "application/json", strings.NewReader(body))
		if err == nil {
			err = response.Body.Close()
			if response.StatusCode != http.StatusNoContent {
				err = fmt.Errorf("статус %d вместо 204", response.StatusCode)
			}
		}
		requestDone <- err
	}()
	waitSignal(t, entered)
	cancel()
	waitSignal(t, shutdownStarted)
	conn, err := net.DialTimeout("tcp", address.String(), time.Second)
	if err == nil {
		_ = conn.Close()
		t.Fatal("сервер принимает новые соединения во время завершения текущего запроса")
	}
	select {
	case err := <-done:
		t.Fatalf("сервис завершился до текущего запроса: %v", err)
	default:
	}
	// Разрешаем запрос после начала Shutdown: отмена запуска не обрывает обработку.
	releaseRequest()
	if err := waitResult(t, requestDone); err != nil {
		t.Fatal(err)
	}
	if err := waitResult(t, done); err != nil {
		t.Fatal(err)
	}
	result := <-collected
	if result.err != nil {
		t.Fatalf("SDK закрыт до завершения текущего запроса: %v", result.err)
	}
	var success int64
	for _, scope := range result.data.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != otel.CompletedName {
				continue
			}
			for _, p := range m.Data.(metricdata.Sum[int64]).DataPoints {
				value, _ := p.Attributes.Value("outcome")
				if value.AsString() == "success" {
					success = p.Value
				}
			}
		}
	}
	if success != 1 {
		t.Fatal("SDK не содержит измерение завершённого при остановке запроса")
	}
	conn, err = net.DialTimeout("tcp", address.String(), time.Second)
	if err == nil {
		_ = conn.Close()
		t.Fatal("сервер принимает соединения после остановки")
	}
	var data metricdata.ResourceMetrics
	if err := app.recorder.Reader().Collect(t.Context(), &data); !errors.Is(err, sdkmetric.ErrReaderShutdown) {
		t.Fatalf("SDK не освобождён после обработки: %v", err)
	}
}

func TestRunBindFailure(t *testing.T) {
	synctest.Test(t, testBindFailure)
}

func testBindFailure(t *testing.T) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	cfg := config.Default()
	cfg.ListenAddr = listener.Addr().String()
	app := testService(t, cfg)
	store := &blockingStore{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		exited:  make(chan struct{}),
		err:     errors.New("обслуживание не должно запускаться"),
	}
	close(store.release)
	app.observer, err = application.NewLoginObserver(store, clock.Real{})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.run(t.Context(), runOptions{}); err == nil || !strings.Contains(err.Error(), "запуск HTTP") {
		t.Fatalf("ошибка занятого адреса потеряна: %v", err)
	}
	timer := time.NewTimer(3 * cfg.MaintenanceInterval)
	defer timer.Stop()
	<-timer.C
	synctest.Wait()
	if store.calls != 0 {
		t.Fatal("ошибка bind оставила работающее обслуживание")
	}
}

func TestRunCancelledBeforeStartup(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	app := testService(t, config.Default())
	called := false
	options := runOptions{listen: func(context.Context) (net.Listener, error) {
		called = true
		return newControlledListener(), nil
	}}
	// Отмена до запуска не нормализуется в успех: запуск не состоялся.
	if err := app.run(ctx, options); !errors.Is(err, context.Canceled) {
		t.Fatalf("отмена до запуска не возвращена как ошибка: %v", err)
	}
	if called {
		t.Fatal("при отмене до запуска вызвана фабрика listener")
	}
}

type failedStore struct{ err error }

func (s failedStore) Within(func(domain.State) error) error { return s.err }

type failedListener struct {
	net.Listener
	err    error
	cancel context.CancelFunc
}

func (l failedListener) Accept() (net.Conn, error) {
	l.cancel()
	return nil, l.err
}

func TestRunComponentFailure(t *testing.T) {
	for _, component := range []string{"HTTP одновременно с отменой", "обслуживание"} {
		t.Run(component, func(t *testing.T) {
			t.Parallel()
			cfg := config.Default()
			cfg.MaintenanceInterval = time.Millisecond
			app := testService(t, cfg)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cause := errors.New("управляемый сбой компонента")
			input := net.Listener(listener)
			if component == "обслуживание" {
				app.observer, err = application.NewLoginObserver(failedStore{cause}, clock.Real{})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				input = failedListener{listener, cause, cancel}
			}
			done := make(chan error, 1)
			go func() { done <- app.run(ctx, listenerOptions(input)) }()
			if err := waitResult(t, done); !errors.Is(err, cause) {
				t.Fatalf("сбой компонента замаскирован: %v", err)
			}
			if _, err := listener.Accept(); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("listener не освобождён после сбоя: %v", err)
			}
		})
	}
}

func TestRunShutdownBudget(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.HTTPReadHeaderTimeout = 5 * time.Millisecond
	cfg.HTTPReadTimeout, cfg.HTTPWriteTimeout = 10*time.Millisecond, 10*time.Millisecond
	cfg.ShutdownTimeout = 50 * time.Millisecond
	app := testService(t, cfg)
	entered, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	app.server.Handler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		defer close(exited)
		<-release
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	done := make(chan error, 1)
	go func() { done <- app.run(ctx, listenerOptions(listener)) }()
	requestDone := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 2 * time.Second}
		defer client.CloseIdleConnections()
		response, err := client.Get("http://" + listener.Addr().String() + "/events")
		if err == nil {
			err = response.Body.Close()
		}
		requestDone <- err
	}()
	waitSignal(t, entered)
	defer func() { close(release); waitSignal(t, exited) }()
	cancel()
	if err := waitResult(t, done); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("исчерпание бюджета не сообщено: %v", err)
	}
	if err := waitResult(t, requestDone); err == nil {
		t.Fatal("соединение зависшего запроса не закрыто принудительно")
	}
}

func TestNewServiceConstructorError(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.NodeName, cfg.Environment = "node-test", "test"
	cfg.MaxScenarios = 0
	app, err := newService(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), clock.Real{})
	if err == nil || !strings.Contains(err.Error(), "лимиты сценариев и событий должны быть положительными") {
		t.Fatalf("ошибка конструктора memory потеряна: %v", err)
	}
	if app != nil {
		t.Fatal("после ошибки сборки возвращено частичное приложение")
	}
}

func TestNoSDKBeforeBind(t *testing.T) {
	t.Parallel()
	app := testService(t, config.Default())
	cause := errors.New("синтетическая ошибка bind до запуска SDK")
	checked := false
	err := app.run(t.Context(), runOptions{listen: func(ctx context.Context) (net.Listener, error) {
		if app.pipeline != nil || app.recorder.Reader() != nil {
			t.Fatal("SDK создан до bind")
		}
		checked = true
		return nil, cause
	}})
	if !checked || !errors.Is(err, cause) {
		t.Fatalf("проверка отсутствия SDK до bind: %v", err)
	}
	if app.pipeline != nil || app.recorder.Reader() != nil {
		t.Fatal("SDK появился после ошибки bind")
	}
}

func TestSDKCleanupErrorNotHiddenByCancellation(t *testing.T) {
	t.Parallel()
	app := testService(t, config.Default())
	cleanupCause := errors.New("синтетическая ошибка очистки SDK")
	app.openPipeline = func(_ context.Context, recorder *otel.Recorder) (metricPipeline, error) {
		if err := recorder.StartLocal(); err != nil {
			return nil, err
		}
		return &localPipeline{recorder: recorder, err: cleanupCause}, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cause := errors.New("синтетическая независимая ошибка bind")
	options := listenerOptions(newControlledListener())
	options.confirmReady = func(context.Context) error { cancel(); return errors.Join(context.Canceled, cause) }
	err := app.run(ctx, options)
	if !errors.Is(err, cause) || !errors.Is(err, cleanupCause) {
		t.Fatalf("ошибка bind или освобождения SDK потеряна: %v", err)
	}
}

// controlledListener позволяет проверять lifecycle и общий бюджет в synctest
// без реального сокета, который не участвует в синтетическом времени.
type controlledListener struct {
	closed chan struct{}
	once   sync.Once
}

func newControlledListener() *controlledListener {
	return &controlledListener{closed: make(chan struct{})}
}

func (l *controlledListener) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}

func (l *controlledListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *controlledListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}
}

type blockingStore struct {
	entered, release, exited chan struct{}
	calls                    int
	err                      error
	cancel                   context.CancelFunc
}

func (s *blockingStore) Within(func(domain.State) error) error {
	s.calls++
	close(s.entered)
	<-s.release
	defer close(s.exited)
	if s.cancel != nil {
		s.cancel()
	}
	return s.err
}

func TestStartupRollback(t *testing.T) {
	for _, scenario := range []string{
		"ошибка подтверждения готовности", "отмена частичного запуска",
		"ошибка готовности одновременно с отменой", "общий бюджет rollback",
	} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := config.Default()
				app := testService(t, cfg)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				store := &blockingStore{
					entered: make(chan struct{}),
					release: make(chan struct{}),
					exited:  make(chan struct{}),
				}
				var err error
				app.observer, err = application.NewLoginObserver(store, clock.Real{})
				if err != nil {
					t.Fatal(err)
				}
				listener := newControlledListener()
				cause := errors.New("сбой подтверждения готовности")
				var stoppingAt time.Time
				options := listenerOptions(listener)
				options.confirmReady = func(ctx context.Context) error {
					<-store.entered
					stoppingAt = time.Now()
					if scenario == "отмена частичного запуска" {
						cancel()
						return ctx.Err()
					}
					if scenario == "ошибка готовности одновременно с отменой" {
						cancel()
						return errors.Join(ctx.Err(), cause)
					}
					return cause
				}
				ready := false
				options.ready = func(net.Addr) { ready = true }
				done := make(chan error, 1)
				go func() { done <- app.run(ctx, options) }()
				<-listener.closed
				synctest.Wait()
				var data metricdata.ResourceMetrics
				if err := app.recorder.Reader().Collect(t.Context(), &data); err != nil {
					t.Fatalf("SDK закрыт до завершения обслуживания при rollback: %v", err)
				}
				if scenario != "общий бюджет rollback" {
					select {
					case err := <-done:
						t.Fatalf("rollback не дождался обслуживания: %v", err)
					default:
					}
					close(store.release)
				}
				err = <-done
				if collectErr := app.recorder.Reader().
					Collect(t.Context(), &data); !errors.Is(
					collectErr,
					sdkmetric.ErrReaderShutdown,
				) {
					t.Fatalf("rollback не освободил SDK: %v", collectErr)
				}
				if scenario == "отмена частичного запуска" {
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("отмена запуска не возвращена как ошибка: %v", err)
					}
				} else if !errors.Is(err, cause) {
					t.Fatalf("исходная ошибка запуска потеряна: %v", err)
				}
				if scenario == "общий бюджет rollback" {
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("бюджет не ограничил rollback: %v", err)
					}
					if elapsed := time.Since(stoppingAt); elapsed != cfg.ShutdownTimeout {
						t.Fatalf("вместо одного бюджета использовано %s", elapsed)
					}
					// Deadline не завершает произвольную функцию: освобождаем её явно.
					select {
					case <-store.exited:
						t.Fatal("заблокированное обслуживание неожиданно завершилось")
					default:
					}
					close(store.release)
				}
				<-store.exited
				synctest.Wait()
				if ready {
					t.Fatal("неуспешный запуск объявлен готовым")
				}
				timer := time.NewTimer(3 * cfg.MaintenanceInterval)
				defer timer.Stop()
				<-timer.C
				synctest.Wait()
				if store.calls != 1 {
					t.Fatalf("после остановки появились вызовы: %d", store.calls)
				}
			})
		})
	}
}

func TestStartupCancellationAfterBind(t *testing.T) {
	t.Parallel()
	app := testService(t, config.Default())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	listener := newControlledListener()
	err := app.run(ctx, runOptions{listen: func(context.Context) (net.Listener, error) {
		cancel()
		return listener, nil
	}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("отмена запуска после bind не возвращена как ошибка: %v", err)
	}
	select {
	case <-listener.closed:
	default:
		t.Fatal("частично неуспешный запуск не закрыл свой listener")
	}
}

func TestMaintenanceFailureWithCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := config.Default()
		app := testService(t, cfg)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		cause := errors.New("сбой обслуживания одновременно с отменой")
		store := &blockingStore{
			entered: make(chan struct{}),
			release: make(chan struct{}),
			exited:  make(chan struct{}),
			err:     cause,
			cancel:  cancel,
		}
		var err error
		app.observer, err = application.NewLoginObserver(store, clock.Real{})
		if err != nil {
			t.Fatal(err)
		}
		listener := newControlledListener()
		done := make(chan error, 1)
		go func() { done <- app.run(ctx, listenerOptions(listener)) }()
		<-store.entered
		close(store.release)
		if err := <-done; !errors.Is(err, cause) {
			t.Fatalf("ошибка обслуживания потеряна при отмене: %v", err)
		}
		<-listener.closed
		<-store.exited
	})
}

type shutdownProbe struct {
	recorder         *otel.Recorder
	entered, release chan struct{}
	calls            atomic.Int64
	err              error
}

func (p *shutdownProbe) Shutdown(ctx context.Context) error {
	p.calls.Add(1)
	close(p.entered)
	select {
	case <-p.release:
	case <-ctx.Done():
	}
	return errors.Join(p.err, p.recorder.Shutdown(ctx))
}

func TestStopConcurrentCallsShareCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app := testService(t, config.Default())
		cause := errors.New("синтетическая ошибка завершения SDK")
		probe := &shutdownProbe{entered: make(chan struct{}), release: make(chan struct{}), err: cause}
		app.openPipeline = func(_ context.Context, r *otel.Recorder) (metricPipeline, error) {
			if err := r.StartLocal(); err != nil {
				return nil, err
			}
			probe.recorder = r
			return probe, nil
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		ready := make(chan struct{})
		options := listenerOptions(newControlledListener())
		options.ready = func(net.Addr) { close(ready) }
		done := make(chan error, 1)
		go func() { done <- app.run(ctx, options) }()
		waitSignal(t, ready)
		cancel()
		waitSignal(t, probe.entered)
		repeated := make(chan error, 1)
		attempted := make(chan struct{})
		go func() { close(attempted); repeated <- app.stop(t.Context()) }()
		waitSignal(t, attempted)
		close(probe.release)
		for _, result := range []<-chan error{done, repeated} {
			if err := waitResult(t, result); !errors.Is(err, cause) {
				t.Fatalf("потеряна общая ошибка остановки: %v", err)
			}
		}
		if probe.calls.Load() != 1 {
			t.Fatal("конкурентные остановки повторили освобождение SDK")
		}
	})
}

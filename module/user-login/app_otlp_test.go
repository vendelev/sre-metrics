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
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	collector "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/otel"
	"github.com/vendelev/sre-metrics/user-login/internal/presentation/config"
)

// Локальный получатель проверяет именно границу сервиса, не внешний Collector.
type serviceReceiver struct {
	server  *httptest.Server
	samples chan receivedMetrics
	entered chan struct{}
	release chan struct{}
	fail    atomic.Bool
	hold    atomic.Bool
	calls   atomic.Int64
}

type receivedMetrics struct {
	request *collector.ExportMetricsServiceRequest
	failed  bool
}

func newServiceReceiver(t *testing.T) *serviceReceiver {
	t.Helper()
	r := &serviceReceiver{
		samples: make(chan receivedMetrics, 128),
		entered: make(chan struct{}, 128),
		release: make(chan struct{}),
	}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		r.calls.Add(1)
		if request.URL.Path != "/v1/metrics" || request.Method != http.MethodPost ||
			request.Header.Get("Content-Type") != "application/x-protobuf" {
			t.Error("неверный OTLP HTTP-контракт")
		}
		data, err := io.ReadAll(request.Body)
		if err != nil {
			return
		}
		message := new(collector.ExportMetricsServiceRequest)
		if err := proto.Unmarshal(data, message); err != nil {
			t.Error(err)
			return
		}
		failed := r.fail.Load()
		select {
		case r.samples <- receivedMetrics{message, failed}:
		case <-request.Context().Done():
			return
		}
		if r.hold.Load() {
			select {
			case r.entered <- struct{}{}:
			case <-request.Context().Done():
				return
			}
			select {
			case <-r.release:
			case <-request.Context().Done():
				return
			}
		}
		if failed {
			http.Error(w, "синтетический отказ", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() { r.server.Close() })
	return r
}

func actualConfig(r *serviceReceiver) config.Config {
	cfg := config.Default()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.NodeName, cfg.Environment = "node-test", "test"
	cfg.OTLPMetricsEndpoint = r.server.URL + "/v1/metrics"
	cfg.OTLPExportInterval = 20 * time.Millisecond
	cfg.OTLPExportTimeout = 200 * time.Millisecond
	cfg.ShutdownTimeout = 2 * time.Second
	cfg.HTTPReadHeaderTimeout = 50 * time.Millisecond
	cfg.HTTPReadTimeout, cfg.HTTPWriteTimeout = 100*time.Millisecond, 100*time.Millisecond
	return cfg
}

func actualService(t *testing.T, cfg config.Config, c *testClock) *service {
	t.Helper()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	app, err := newService(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), c)
	if err != nil {
		t.Fatal(err)
	}
	if app.pipeline != nil || app.recorder.Reader() != nil {
		t.Fatal("SDK запущен при сборке")
	}
	return app
}

type runningService struct {
	cancel  context.CancelFunc
	done    chan struct{}
	err     error
	address net.Addr
}

func runActual(t *testing.T, app *service, options runOptions) *runningService {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	running := &runningService{cancel: cancel, done: make(chan struct{})}
	ready := make(chan net.Addr, 1)
	options.ready = func(addr net.Addr) { ready <- addr }
	go func() { running.err = app.run(ctx, options); close(running.done) }()
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

func sendServiceEvent(
	t *testing.T,
	running *runningService,
	id, scenario int,
	kind string,
	at time.Time,
	blocked bool,
) error {
	t.Helper()
	source, extra := "mis", ""
	if kind == "login_mode_determined" {
		source, extra = "auth", `,"mode":"credentials"`
	}
	body := fmt.Sprintf(
		`{"version":1,"event_id":"019aabcd-0000-7000-8000-%012x","scenario_id":"019aabcd-0000-7000-9000-%012x","occurred_at":%q,"source":%q,"type":%q%s}`,
		id,
		scenario,
		at.Format("2006-01-02T15:04:05.000Z"),
		source,
		kind,
		extra,
	)
	request, err := http.NewRequest(
		http.MethodPost,
		"http://"+running.address.String()+"/events",
		strings.NewReader(body),
	)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if blocked {
		request.Header.Set("X-Test-Barrier", "yes")
	}
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("HTTP %d вместо 204", response.StatusCode)
	}
	return nil
}

type serviceValues struct {
	completed map[string]int64
	errors    map[string]int64
	histogram *metricpb.HistogramDataPoint
	instance  string
	starts    map[string]uint64
}

func serviceMetrics(t *testing.T, message *collector.ExportMetricsServiceRequest) serviceValues {
	t.Helper()
	if len(message.ResourceMetrics) != 1 {
		t.Fatal("число Resource")
	}
	rm := message.ResourceMetrics[0]
	resource := make(map[string]string)
	for _, a := range rm.Resource.Attributes {
		resource[a.Key] = a.Value.GetStringValue()
	}
	instance, err := uuid.Parse(resource["service.instance.id"])
	if err != nil || instance.Version() != 7 {
		t.Fatal("UUID Resource")
	}
	if len(rm.Resource.Attributes) != 5 || !reflect.DeepEqual(resource, map[string]string{
		"service.namespace":           "sptr",
		"service.name":                "mis-login-observability",
		"service.instance.id":         instance.String(),
		"host.name":                   "node-test",
		"deployment.environment.name": "test",
	}) {
		t.Fatalf("Resource: %v", resource)
	}
	if len(rm.ScopeMetrics) != 1 || rm.ScopeMetrics[0].Scope.Name != otel.ScopeName {
		t.Fatal("scope сервиса")
	}
	values := serviceValues{
		completed: make(map[string]int64),
		errors:    make(map[string]int64),
		starts:    make(map[string]uint64),
		instance:  instance.String(),
	}
	for _, m := range rm.ScopeMetrics[0].Metrics {
		if sum := m.GetSum(); sum != nil {
			if !sum.IsMonotonic ||
				sum.AggregationTemporality != metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE ||
				m.Unit != "1" {
				t.Fatal("контракт Counter")
			}
			for _, point := range sum.DataPoints {
				if len(point.Attributes) != 1 || point.StartTimeUnixNano == 0 ||
					point.TimeUnixNano < point.StartTimeUnixNano {
					t.Fatal("метаданные Counter")
				}
				value := point.Attributes[0].Value.GetStringValue()
				switch m.Name {
				case otel.CompletedName:
					values.completed[value] = point.GetAsInt()
				case otel.TechnicalErrorsName:
					values.errors[value] = point.GetAsInt()
				default:
					t.Fatal("лишний Counter")
				}
				values.starts[m.Name+value] = point.StartTimeUnixNano
			}
			continue
		}
		h := m.GetHistogram()
		if m.Name != otel.DurationName || m.Unit != "s" || h == nil ||
			h.AggregationTemporality != metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE ||
			len(h.DataPoints) != 1 {
			t.Fatal("контракт Histogram")
		}
		values.histogram = h.DataPoints[0]
		if !reflect.DeepEqual(values.histogram.ExplicitBounds, []float64{5, 30, 60}) || values.histogram.Min != nil ||
			values.histogram.Max != nil ||
			len(values.histogram.Exemplars) != 0 {
			t.Fatal("метаданные Histogram")
		}
	}
	if len(values.completed) != 4 || len(values.errors) != 2 {
		t.Fatal("нет нулевых Counter")
	}
	encoded, err := protojson.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"019aabcd", "event_id", "scenario_id", "X-Test-Barrier", "http://"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("посторонние данные: %s", forbidden)
		}
	}
	return values
}

func nextServiceMetrics(
	t *testing.T,
	r *serviceReceiver,
	predicate func(receivedMetrics, serviceValues) bool,
) serviceValues {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case sample := <-r.samples:
			values := serviceMetrics(t, sample.request)
			if predicate(sample, values) {
				return values
			}
		case <-timer.C:
			t.Fatal("нет ожидаемого OTLP-пакета")
			return serviceValues{}
		}
	}
}

func lastServiceMetrics(t *testing.T, r *serviceReceiver) serviceValues {
	t.Helper()
	var last *collector.ExportMetricsServiceRequest
	for {
		select {
		case sample := <-r.samples:
			last = sample.request
		default:
			if last == nil {
				t.Fatal("нет финального пакета")
			}
			return serviceMetrics(t, last)
		}
	}
}

// BR-42, AC-31: текущий запрос и обслуживание завершаются до последнего пакета.
func TestServiceOTLPFinalAfterProcessing(t *testing.T) {
	r := newServiceReceiver(t)
	cfg := actualConfig(r)
	cfg.MaintenanceInterval = 20 * time.Millisecond
	c := &testClock{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	app := actualService(t, cfg, c)
	requestEntered, requestRelease := make(chan struct{}), make(chan struct{})
	maintenanceEntered, maintenanceRelease := make(chan struct{}), make(chan struct{})
	releaseRequest := sync.OnceFunc(func() { close(requestRelease) })
	releaseMaintenance := sync.OnceFunc(func() { close(maintenanceRelease) })
	defer releaseRequest()
	defer releaseMaintenance()
	handler := app.server.Handler
	app.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Test-Barrier") == "yes" {
			close(requestEntered)
			<-requestRelease
		}
		handler.ServeHTTP(w, request)
	})
	var first atomic.Bool
	options := runOptions{maintain: func() error {
		if first.CompareAndSwap(false, true) {
			close(maintenanceEntered)
			<-maintenanceRelease
		}
		return app.observer.Maintain()
	}}
	shutdownStarted := make(chan struct{})
	app.server.RegisterOnShutdown(func() { close(shutdownStarted) })
	running := runActual(t, app, options)
	baseline := nextServiceMetrics(
		t,
		r,
		func(_ receivedMetrics, v serviceValues) bool { return v.completed["success"] == 0 },
	)
	if baseline.histogram != nil {
		t.Fatal("фиктивная Histogram")
	}
	now := c.Now()
	for _, event := range []struct {
		id, scenario int
		kind         string
		at           time.Time
	}{
		{1, 100, "login_started", now.Add(-14*time.Minute - 59*time.Second)},
		{2, 200, "login_started", now.Add(-5 * time.Second)},
		{3, 200, "login_mode_determined", now},
	} {
		if err := sendServiceEvent(t, running, event.id, event.scenario, event.kind, event.at, false); err != nil {
			t.Fatal(err)
		}
	}
	requestDone := make(chan error, 1)
	go func() { requestDone <- sendServiceEvent(t, running, 4, 200, "login_succeeded", now, true) }()
	waitSignal(t, requestEntered)
	waitSignal(t, maintenanceEntered)
	c.Advance(2 * time.Second)
	running.cancel()
	waitSignal(t, shutdownStarted)
	if conn, err := net.DialTimeout("tcp", running.address.String(), time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("новое соединение при остановке")
	}
	releaseRequest()
	if err := waitResult(t, requestDone); err != nil {
		t.Fatal(err)
	}
	select {
	case <-running.done:
		t.Fatal("остановка не дождалась обслуживания")
	default:
	}
	releaseMaintenance()
	if err := running.stop(t); err != nil {
		t.Fatal(err)
	}
	final := lastServiceMetrics(t, r)
	if final.completed["success"] != 1 || final.completed["unknown"] != 1 || final.histogram == nil ||
		final.histogram.Count != 1 ||
		final.histogram.GetSum() != 5 ||
		!reflect.DeepEqual(final.histogram.BucketCounts, []uint64{1, 0, 0, 0}) {
		t.Fatalf("последний пакет: %+v", final)
	}
	if final.instance != baseline.instance || !reflect.DeepEqual(final.starts, baseline.starts) {
		t.Fatal("SDK/Resource пересоздан")
	}
	if app.recorder.Reader() != nil {
		t.Fatal("ManualReader в production")
	}
	select {
	case <-r.samples:
		t.Fatal("отправка после остановки")
	case <-time.After(3 * cfg.OTLPExportInterval):
	}
}

// BR-41, AC-30: сеть не удерживает HTTP и не сбрасывает cumulative-накопление.
func TestServiceOTLPRecovery(t *testing.T) {
	r := newServiceReceiver(t)
	r.fail.Store(true)
	cfg := actualConfig(r)
	c := &testClock{now: time.Now().UTC().Truncate(time.Millisecond)}
	app := actualService(t, cfg, c)
	running := runActual(t, app, runOptions{})
	baseline := nextServiceMetrics(t, r, func(sample receivedMetrics, _ serviceValues) bool { return sample.failed })
	r.hold.Store(true)
	waitSignal(t, r.entered)
	for id := 1; id <= 2; id++ {
		if err := sendServiceEvent(t, running, id, id, "login_succeeded", c.Now(), false); err != nil {
			t.Fatal(err)
		}
	}
	close(r.release)
	nextServiceMetrics(t, r, func(sample receivedMetrics, v serviceValues) bool {
		return sample.failed && v.completed["success"] == 2
	})
	r.fail.Store(false)
	recovered := nextServiceMetrics(t, r, func(sample receivedMetrics, v serviceValues) bool {
		return !sample.failed && v.completed["success"] == 2
	})
	if recovered.instance != baseline.instance || !reflect.DeepEqual(recovered.starts, baseline.starts) {
		t.Fatal("сброс SDK после отказа")
	}
	select {
	case <-running.done:
		t.Fatalf("ошибка экспорта остановила сервис: %v", running.err)
	default:
	}
	if err := running.stop(t); err != nil {
		t.Fatal(err)
	}
	if lastServiceMetrics(t, r).completed["success"] != 2 {
		t.Fatal("неверный финальный итог")
	}
}

func TestServiceOTLPStartupCleanup(t *testing.T) {
	for _, scenario := range []string{"ошибка после старта", "отмена после старта", "независимая ошибка с отменой", "отмена внутри запуска pipeline", "ошибка внутри запуска pipeline"} {
		t.Run(scenario, func(t *testing.T) {
			r := newServiceReceiver(t)
			cfg := actualConfig(r)
			cfg.OTLPExportInterval = time.Hour
			app := actualService(t, cfg, &testClock{now: time.Now()})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cause := errors.New("синтетический сбой запуска")
			listener := newControlledListener()
			options := listenerOptions(listener)
			ready := false
			options.ready = func(net.Addr) { ready = true }
			if strings.Contains(scenario, "внутри") {
				app.openPipeline = func(ctx context.Context, recorder *otel.Recorder) (metricPipeline, error) {
					p, err := otel.StartPipeline(
						ctx,
						recorder,
						otel.PipelineSettings{
							Endpoint:      cfg.OTLPMetricsEndpoint,
							Interval:      cfg.OTLPExportInterval,
							ExportTimeout: cfg.OTLPExportTimeout,
						},
						app.logger,
					)
					if err != nil {
						return nil, err
					}
					cancel()
					if strings.HasPrefix(scenario, "ошибка") {
						return p, errors.Join(ctx.Err(), cause)
					}
					return p, nil
				}
			} else {
				options.confirmReady = func(context.Context) error {
					if strings.Contains(scenario, "отмена") || strings.Contains(scenario, "отменой") {
						cancel()
					}
					if scenario == "отмена после старта" {
						return ctx.Err()
					}
					if ctx.Err() != nil {
						return errors.Join(ctx.Err(), cause)
					}
					return cause
				}
			}
			err := app.run(ctx, options)
			if ready {
				t.Fatal("частичный запуск объявлен готовым")
			}
			if strings.HasPrefix(scenario, "отмена") {
				// Отмена запуска не нормализуется в успех.
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("отмена запуска не возвращена как ошибка: %v", err)
				}
			} else if !errors.Is(err, cause) {
				t.Fatalf("потерян независимый сбой: %v", err)
			}
			waitSignal(t, listener.closed)
			if app.pipeline == nil {
				t.Fatal("pipeline не был создан")
			}
			v := lastServiceMetrics(t, r)
			if v.completed["success"] != 0 || v.histogram != nil {
				t.Fatal("фиктивные измерения при rollback")
			}
			before := r.calls.Load()
			if err := app.stop(ctx); err != nil {
				t.Fatal(err)
			}
			if r.calls.Load() != before {
				t.Fatal("повторная очистка отправила пакет")
			}
		})
	}
}

func TestServicePipelineFailureClosesListener(t *testing.T) {
	r := newServiceReceiver(t)
	cfg := actualConfig(r)
	app := actualService(t, cfg, &testClock{now: time.Now()})
	app.cfg.OTLPMetricsEndpoint = "http://localhost"
	listener := newControlledListener()
	err := app.run(t.Context(), listenerOptions(listener))
	if err == nil || !strings.Contains(err.Error(), "USER_AUTH_OTLP_METRICS_ENDPOINT") {
		t.Fatalf("ошибка pipeline: %v", err)
	}
	waitSignal(t, listener.closed)
	if app.pipeline != nil || r.calls.Load() != 0 {
		t.Fatal("ресурсы после отказа инициализации")
	}
}

func TestServiceFinalExportTimeout(t *testing.T) {
	r := newServiceReceiver(t)
	r.hold.Store(true)
	cfg := actualConfig(r)
	cfg.OTLPExportInterval = time.Hour
	cfg.OTLPExportTimeout = 100 * time.Millisecond
	c := &testClock{now: time.Now().UTC().Truncate(time.Millisecond)}
	app := actualService(t, cfg, c)
	running := runActual(t, app, runOptions{})
	if err := sendServiceEvent(t, running, 1, 1, "login_succeeded", c.Now(), false); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := running.stop(t)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > cfg.ShutdownTimeout {
		t.Fatalf("неограниченный/скрытый отказ экспорта: %v", err)
	}
	if r.calls.Load() != 1 {
		t.Fatal("повтор финальной отправки")
	}
	if lastServiceMetrics(t, r).completed["success"] != 1 {
		t.Fatal("содержимое последней попытки")
	}
}

type budgetPipeline struct {
	recorder *otel.Recorder
	started  time.Time
	deadline time.Time
}

func (p *budgetPipeline) Shutdown(ctx context.Context) error {
	p.started = time.Now()
	p.deadline, _ = ctx.Deadline()
	<-ctx.Done()
	return errors.Join(ctx.Err(), p.recorder.Shutdown(ctx))
}

func TestServiceSharedShutdownBudget(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rollback   bool
		closeDelay time.Duration
	}{
		{"остановка", false, 0}, {"rollback", true, 0}, {"медленное закрытие listener съедает бюджет", false, 8 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) { testServiceSharedShutdownBudget(t, tc.rollback, tc.closeDelay) })
	}
}

// Быстрый drain передаёт SDK оставшееся время общего бюджета, а не полный бюджет.
func TestServiceEarlyDrainGivesSDKRemainingBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := config.Default()
		app := testService(t, cfg)
		var pipeline *budgetPipeline
		app.openPipeline = func(_ context.Context, r *otel.Recorder) (metricPipeline, error) {
			if err := r.StartLocal(); err != nil {
				return nil, err
			}
			pipeline = &budgetPipeline{recorder: r}
			return pipeline, nil
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		options := listenerOptions(newControlledListener())
		var stoppingAt time.Time
		options.ready = func(net.Addr) { stoppingAt = time.Now(); cancel() }
		if err := app.run(ctx, options); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ошибка timeout SDK потеряна: %v", err)
		}
		if pipeline.started != stoppingAt || pipeline.deadline.Sub(stoppingAt) != cfg.ShutdownTimeout ||
			time.Since(stoppingAt) != cfg.ShutdownTimeout {
			t.Fatal("быстрый drain выдал SDK бюджет вне общего срока")
		}
	})
}

type delayedCloseListener struct {
	net.Listener
	delay time.Duration
}

func (l delayedCloseListener) Close() error { time.Sleep(l.delay); return l.Listener.Close() }

func testServiceSharedShutdownBudget(t *testing.T, rollback bool, closeDelay time.Duration) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		cfg := config.Default()
		app := testService(t, cfg)
		var pipeline *budgetPipeline
		app.openPipeline = func(_ context.Context, r *otel.Recorder) (metricPipeline, error) {
			if err := r.StartLocal(); err != nil {
				return nil, err
			}
			pipeline = &budgetPipeline{recorder: r}
			return pipeline, nil
		}
		entered, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
		options := listenerOptions(newControlledListener())
		if closeDelay > 0 {
			options = listenerOptions(delayedCloseListener{Listener: newControlledListener(), delay: closeDelay})
		}
		options.maintain = func() error { close(entered); <-release; defer close(exited); return nil }
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var start time.Time
		cause := errors.New("ошибка готовности для проверки бюджета rollback")
		if rollback {
			options.confirmReady = func(context.Context) error { <-entered; start = time.Now(); return cause }
		}
		done := make(chan error, 1)
		go func() { done <- app.run(ctx, options) }()
		<-entered
		if !rollback {
			start = time.Now()
			cancel()
		}
		err := <-done
		if rollback && !errors.Is(err, cause) {
			t.Fatalf("потеряна ошибка startup: %v", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("нет ошибки бюджета: %v", err)
		}
		if pipeline.started.Sub(start) != cfg.ShutdownTimeout ||
			pipeline.deadline.Sub(start) != cfg.ShutdownTimeout ||
			time.Since(start) != cfg.ShutdownTimeout {
			t.Fatalf(
				"бюджет: начало SDK %s, deadline %s, всего %s",
				pipeline.started.Sub(start),
				pipeline.deadline.Sub(start),
				time.Since(start),
			)
		}
		before := time.Now()
		if err := app.stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("потеряна ошибка повторной очистки: %v", err)
		}
		if time.Now() != before {
			t.Fatal("повторная очистка получила новый бюджет")
		}
		select {
		case <-exited:
			t.Fatal("контекст завершил произвольную Go-функцию")
		default:
		}
		close(release)
		<-exited
		synctest.Wait()
	})
}

// Сеть здесь настоящая; граница незавершённого обслуживания управляется каналом.
func TestServiceIncompleteDrainShutsDownSDK(t *testing.T) {
	r := newServiceReceiver(t)
	cfg := actualConfig(r)
	cfg.ShutdownTimeout = 300 * time.Millisecond
	cfg.HTTPReadTimeout, cfg.HTTPWriteTimeout = 50*time.Millisecond, 50*time.Millisecond
	cfg.MaintenanceInterval = time.Millisecond
	c := &testClock{now: time.Now().UTC().Truncate(time.Millisecond)}
	app := actualService(t, cfg, c)
	httpEntered, httpRelease, httpExited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	releaseHTTP := sync.OnceFunc(func() { close(httpRelease) })
	defer releaseHTTP()
	handler := app.server.Handler
	app.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		close(httpEntered)
		defer close(httpExited)
		<-httpRelease
		handler.ServeHTTP(w, request)
	})
	entered, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	options := runOptions{
		maintain: func() error { close(entered); <-release; defer close(exited); return app.observer.Maintain() },
	}
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()
	running := runActual(t, app, options)
	waitSignal(t, entered)
	requestDone := make(chan error, 1)
	go func() { requestDone <- sendServiceEvent(t, running, 1, 1, "login_succeeded", c.Now(), false) }()
	waitSignal(t, httpEntered)
	if err := running.stop(t); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("незавершённое обслуживание скрыто: %v", err)
	}
	// Обычный Shutdown отправляет доступные значения, но запрос и обслуживание
	// ещё заблокированы. Даже успешная отправка не делает остановку успешной
	// и не означает окончательного снимка их измерений.
	if lastServiceMetrics(t, r).completed["success"] != 0 {
		t.Fatal("незавершённый запрос объявлен учтённым")
	}
	for _, signal := range []<-chan struct{}{httpExited, exited} {
		select {
		case <-signal:
			t.Fatal("Shutdown SDK завершил заблокированную Go-функцию")
		default:
		}
	}
	if err := waitResult(t, requestDone); err == nil {
		t.Fatal("зависшее HTTP-соединение не закрыто принудительно")
	}
	releaseHTTP()
	waitSignal(t, httpExited)
	releaseOnce()
	waitSignal(t, exited)
	// Получатель может опубликовать уже начатый до Shutdown запрос после
	// возврата остановки. Продолжающийся периодический экспорт не даст
	// наступить полосе тишины длиной в три интервала.
	quiet := time.NewTimer(3 * cfg.OTLPExportInterval)
	defer quiet.Stop()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case <-r.samples:
			if !quiet.Stop() {
				select {
				case <-quiet.C:
				default:
				}
			}
			quiet.Reset(3 * cfg.OTLPExportInterval)
		case <-quiet.C:
			return
		case <-timeout.C:
			t.Fatal("экспорт продолжается после аварийного завершения")
		}
	}
}

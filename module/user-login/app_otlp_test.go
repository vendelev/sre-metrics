package userlogin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
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

func actualConfig(r *serviceReceiver) *config.Config {
	cfg := config.Default()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.NodeName, cfg.Environment = "node-test", "test"
	cfg.OTLPMetricsEndpoint = r.server.URL + "/v1/metrics"
	cfg.OTLPExportInterval = 20 * time.Millisecond
	cfg.OTLPExportTimeout = 200 * time.Millisecond
	cfg.ShutdownTimeout = 2 * time.Second
	cfg.HTTPReadHeaderTimeout = 50 * time.Millisecond
	cfg.HTTPReadTimeout, cfg.HTTPWriteTimeout = 100*time.Millisecond, 100*time.Millisecond
	return &cfg
}

type runningService struct {
	cancel  context.CancelFunc
	done    chan struct{}
	err     error
	address net.Addr
}

func runActual(t *testing.T, app *service) *runningService {
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
		`{"event_id":"019aabcd-0000-7000-8000-%012x","scenario_id":"019aabcd-0000-7000-9000-%012x","occurred_at":%q,"source":%q,"type":%q%s}`,
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
	c := newBarrierClock(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC))
	app := testService(t, cfg, c)
	requestEntered, requestRelease := make(chan struct{}), make(chan struct{})
	releaseRequest := sync.OnceFunc(func() { close(requestRelease) })
	releaseMaintenance := sync.OnceFunc(func() { close(c.release) })
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
	shutdownStarted := make(chan struct{})
	app.server.RegisterOnShutdown(func() { close(shutdownStarted) })
	running := runActual(t, app)
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
	c.armed.Store(true)
	waitSignal(t, c.entered)
	c.Advance(2 * time.Second)
	running.cancel()
	waitSignal(t, shutdownStarted)
	if conn, err := net.DialTimeout("tcp", running.address.String(), time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("новое соединение при остановке")
	}
	select {
	case <-running.done:
		t.Fatal("остановка не дождалась обслуживания")
	default:
	}
	releaseMaintenance()
	waitSignal(t, c.exited)
	select {
	case <-running.done:
		t.Fatal("остановка не дождалась HTTP-запроса")
	default:
	}
	releaseRequest()
	if err := waitResult(t, requestDone); err != nil {
		t.Fatal(err)
	}
	if err := running.stop(t); err != nil {
		t.Fatal(err)
	}
	assertListenerClosed(t, app)
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
	app := testService(t, cfg, c)
	running := runActual(t, app)
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

func TestServicePipelineFailureClosesListener(t *testing.T) {
	r := newServiceReceiver(t)
	cfg := actualConfig(r)
	app := testService(t, cfg, &testClock{now: time.Now()})
	app.cfg.OTLPMetricsEndpoint = "http://localhost"
	ready := false
	err := app.run(t.Context(), func(net.Addr) { ready = true })
	if err == nil || !strings.Contains(err.Error(), "USER_AUTH_OTLP_METRICS_ENDPOINT") {
		t.Fatalf("ошибка pipeline: %v", err)
	}
	assertListenerClosed(t, app)
	if ready || app.pipeline != nil || r.calls.Load() != 0 {
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
	app := testService(t, cfg, c)
	running := runActual(t, app)
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

// Настоящее обслуживание расходует часть бюджета; зависший OTLP-получатель
// должен получить лишь остаток, а не новый полный timeout остановки.
func TestServiceSharedShutdownBudget(t *testing.T) {
	t.Parallel()
	r := newServiceReceiver(t)
	r.hold.Store(true)
	cfg := actualConfig(r)
	cfg.OTLPExportInterval = time.Hour
	cfg.OTLPExportTimeout = 5 * time.Second
	cfg.ShutdownTimeout = time.Second
	cfg.MaintenanceInterval = time.Millisecond
	c := newBarrierClock(time.Now().UTC().Truncate(time.Millisecond))
	app := testService(t, cfg, c)
	releaseMaintenance := sync.OnceFunc(func() { close(c.release) })
	defer releaseMaintenance()
	shutdownStarted := make(chan struct{})
	app.server.RegisterOnShutdown(func() { close(shutdownStarted) })
	running := runActual(t, app)
	if err := sendServiceEvent(t, running, 1, 1, "login_succeeded", c.Now(), false); err != nil {
		t.Fatal(err)
	}
	c.armed.Store(true)
	waitSignal(t, c.entered)
	started := time.Now()
	running.cancel()
	waitSignal(t, shutdownStarted)
	// Ожидание намеренно расходует бюджет, а не задаёт порядок goroutine:
	// начало Shutdown и завершение обслуживания подтверждаются отдельными сигналами.
	timer := time.NewTimer(3 * cfg.ShutdownTimeout / 4)
	defer timer.Stop()
	<-timer.C
	releaseMaintenance()
	waitSignal(t, c.exited)
	waitSignal(t, r.entered)
	if err := running.stop(t); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("исчерпание общего бюджета не сообщено: %v", err)
	}
	// Допуск нужен планировщику и race detector; отдельный полный бюджет SDK
	// увеличил бы длительность как минимум до 1,75 общего бюджета.
	if elapsed := time.Since(started); elapsed > 3*cfg.ShutdownTimeout/2 {
		t.Fatalf("SDK получил новый бюджет после обслуживания: %s", elapsed)
	}
	if lastServiceMetrics(t, r).completed["success"] != 1 || r.calls.Load() != 1 {
		t.Fatal("неверная или повторная финальная попытка экспорта")
	}
}

// Сеть здесь настоящая; граница незавершённого обслуживания управляется каналом.
func TestServiceIncompleteDrainShutsDownSDK(t *testing.T) {
	r := newServiceReceiver(t)
	cfg := actualConfig(r)
	cfg.ShutdownTimeout = 300 * time.Millisecond
	cfg.HTTPReadTimeout, cfg.HTTPWriteTimeout = 50*time.Millisecond, 50*time.Millisecond
	cfg.MaintenanceInterval = time.Millisecond
	c := newBarrierClock(time.Now().UTC().Truncate(time.Millisecond))
	app := testService(t, cfg, c)
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
	c.armed.Store(true)
	releaseOnce := sync.OnceFunc(func() { close(c.release) })
	defer releaseOnce()
	running := runActual(t, app)
	waitSignal(t, c.entered)
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
	for _, signal := range []<-chan struct{}{httpExited, c.exited} {
		select {
		case <-signal:
			t.Fatal("Shutdown SDK завершил заблокированную Go-функцию")
		default:
		}
	}
	if err := waitResult(t, requestDone); err == nil {
		t.Fatal("зависшее HTTP-соединение не закрыто принудительно")
	}
	releaseOnce()
	waitSignal(t, c.exited)
	releaseHTTP()
	waitSignal(t, httpExited)
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

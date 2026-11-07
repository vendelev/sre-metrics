package otel

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vendelev/sre-metrics/user-login/internal/domain/event"
	collector "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/vendelev/sre-metrics/user-login/internal/application"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/memory"
	"github.com/vendelev/sre-metrics/user-login/internal/presentation/httpapi"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func pipelineSettings(endpoint string) PipelineSettings {
	return PipelineSettings{Endpoint: endpoint, Interval: time.Hour, ExportTimeout: time.Second}
}

func preparedRecorder(t *testing.T) *Recorder {
	t.Helper()
	r, err := Prepare(ResourceSettings{NodeName: "node-test", Environment: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func cleanupPipeline(t *testing.T, p *Pipeline) {
	t.Helper()
	if p == nil {
		return
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// Страховка при Fatal: освобождаем SDK и transport без финальной отправки.
		// Ошибки штатного Shutdown проверяются в самих тестах.
		_ = p.Shutdown(ctx)
	})
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("не дождались сигнала")
		var zero T
		return zero
	}
}

func flush(t *testing.T, p *Pipeline) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	return p.Recorder.provider.ForceFlush(ctx)
}

type exportClock struct{ now time.Time }

func (c exportClock) Now() time.Time { return c.now }

func observerFor(t *testing.T, r *Recorder) (*application.LoginObserver, time.Time) {
	t.Helper()
	store, err := memory.NewStore(memory.DefaultLimits(), r)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	o, err := application.NewLoginObserver(store, exportClock{now})
	if err != nil {
		t.Fatal(err)
	}
	return o, now
}

func processExportEvent(
	t *testing.T,
	o *application.LoginObserver,
	scenario int,
	kind event.Type,
	at time.Time,
) string {
	t.Helper()
	e := event.Request{
		EventID:    uuid.Must(uuid.NewV7()),
		ScenarioID: uuid.MustParse(fmt.Sprintf("019aabcd-0000-7000-9000-%012x", scenario)),
		Source:     event.SourceMIS,
		Type:       kind,
		OccurredAt: at,
	}
	if kind == event.LoginModeDetermined {
		mode := event.LoginModeCredentials
		e.Source, e.AuthMode = event.SourceAuth, &mode
	}
	if _, err := o.Process(e); err != nil {
		t.Fatal(err)
	}
	return e.EventID.String()
}

func attrs(values []*commonpb.KeyValue) map[string]string {
	m := make(map[string]string)
	for _, v := range values {
		m[v.Key] = v.Value.GetStringValue()
	}
	return m
}

type wireValues struct {
	completed map[string]int64
	errors    map[string]int64
	histogram *metricpb.HistogramDataPoint
	starts    map[string]uint64
	instance  string
}

func checkWire(t *testing.T, request *collector.ExportMetricsServiceRequest) wireValues {
	t.Helper()
	if len(request.ResourceMetrics) != 1 {
		t.Fatal("не один Resource")
	}
	rm := request.ResourceMetrics[0]
	resource := attrs(rm.Resource.Attributes)
	id, err := uuid.Parse(resource["service.instance.id"])
	if err != nil || id.Version() != 7 {
		t.Fatal("не UUID v7")
	}
	if !reflect.DeepEqual(
		resource,
		map[string]string{"service.namespace": "sptr", "service.name": "mis-login-observability",
			"service.instance.id": id.String(), "host.name": "node-test", "deployment.environment.name": "test"},
	) ||
		len(rm.Resource.Attributes) != 5 {
		t.Fatalf("Resource: %v", resource)
	}
	if len(rm.ScopeMetrics) != 1 || rm.ScopeMetrics[0].Scope.Name != ScopeName ||
		rm.ScopeMetrics[0].Scope.Version != "" ||
		len(rm.ScopeMetrics[0].Scope.Attributes) != 0 ||
		rm.SchemaUrl != "" {
		t.Fatal("неожиданный scope/схема")
	}
	result := wireValues{
		completed: make(map[string]int64),
		errors:    make(map[string]int64),
		starts:    make(map[string]uint64),
		instance:  id.String(),
	}
	for _, m := range rm.ScopeMetrics[0].Metrics {
		if m.Description != "" {
			t.Fatal("лишнее описание")
		}
		switch m.Name {
		case CompletedName, TechnicalErrorsName:
			s := m.GetSum()
			if s == nil || !s.IsMonotonic ||
				s.AggregationTemporality != metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE ||
				m.Unit != "1" {
				t.Fatal("контракт суммы")
			}
			key, target := "outcome", result.completed
			if m.Name == TechnicalErrorsName {
				key, target = "mode", result.errors
			}
			for _, point := range s.DataPoints {
				if len(point.Attributes) != 1 || attrs(point.Attributes)[key] == "" || len(point.Exemplars) != 0 ||
					point.Value == nil {
					t.Fatal("атрибуты Counter")
				}
				value := attrs(point.Attributes)[key]
				target[value] = point.GetAsInt()
				checkWireTime(t, point.StartTimeUnixNano, point.TimeUnixNano)
				result.starts[m.Name+value] = point.StartTimeUnixNano
			}
		case DurationName:
			h := m.GetHistogram()
			if h == nil ||
				h.AggregationTemporality != metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE ||
				m.Unit != "s" ||
				len(h.DataPoints) != 1 {
				t.Fatal("контракт Histogram")
			}
			point := h.DataPoints[0]
			if len(point.Attributes) != 1 || attrs(point.Attributes)["mode"] != "credentials" || point.Min != nil ||
				point.Max != nil ||
				len(point.Exemplars) != 0 ||
				!reflect.DeepEqual(point.ExplicitBounds, []float64{5, 30, 60}) {
				t.Fatal("метаданные Histogram")
			}
			checkWireTime(t, point.StartTimeUnixNano, point.TimeUnixNano)
			result.starts[m.Name] = point.StartTimeUnixNano
			result.histogram = point
		default:
			t.Fatalf("лишняя метрика %s", m.Name)
		}
	}
	if len(result.completed) != 4 || len(result.errors) != 2 {
		t.Fatal("нет baseline")
	}
	encoded, err := protojson.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"019aabcd", "scenario_id", "event_id", "synthetic-secret", "token", "cookie", "http://"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("посторонние данные в OTLP: %s", forbidden)
		}
	}
	return result
}

func checkWireTime(t *testing.T, start, end uint64) {
	t.Helper()
	if start == 0 || end < start || end > uint64(time.Now().UnixNano()) ||
		end < uint64(time.Now().Add(-time.Minute).UnixNano()) {
		t.Fatal("времена OTLP")
	}
}

// BR-41, BR-43–47, AC-08: настоящий application + memory + SDK + protobuf + HTTP.
func TestPipelineOTLPHTTP(t *testing.T) {
	requests := make(chan *collector.ExportMetricsServiceRequest, 20)
	closedConnections := make(chan struct{}, 20)
	var calls atomic.Int64
	var failure atomic.Bool
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/metrics" || r.Method != http.MethodPost ||
			r.Header.Get("Content-Type") != "application/x-protobuf" ||
			r.Header.Get("Authorization") != "" {
			t.Error("контракт HTTP")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		request := new(collector.ExportMetricsServiceRequest)
		if err := proto.Unmarshal(body, request); err != nil {
			t.Error(err)
			return
		}
		requests <- request
		if failure.Load() {
			http.Error(w, "synthetic-secret", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closedConnections <- struct{}{}
		}
	}
	server.Start()
	defer server.Close()
	r := preparedRecorder(t)
	instance, _ := r.resource.Set().Value("service.instance.id")
	p, err := StartPipeline(t.Context(), r, pipelineSettings(server.URL+"/v1/metrics"), testLogger())
	cleanupPipeline(t, p)
	if err != nil {
		t.Fatal(err)
	}
	o, now := observerFor(t, p.Recorder)
	if err := flush(t, p); err != nil {
		t.Fatal(err)
	}
	baseline := checkWire(t, receive(t, requests))
	if baseline.instance != instance.AsString() {
		t.Fatal("активация изменила ID подготовленного Recorder")
	}
	repeatedPipeline, err := StartPipeline(t.Context(), r, pipelineSettings(server.URL+"/v1/metrics"), testLogger())
	cleanupPipeline(t, repeatedPipeline)
	if repeatedPipeline != nil || err == nil {
		t.Fatalf("разрешена повторная активация Recorder: %v", err)
	}
	if baseline.histogram != nil ||
		!reflect.DeepEqual(
			baseline.completed,
			map[string]int64{"success": 0, "technical_error": 0, "rejected": 0, "unknown": 0},
		) ||
		!reflect.DeepEqual(baseline.errors, map[string]int64{"credentials": 0, "existing_session": 0}) {
		t.Fatal("фиктивные измерения")
	}
	var eventIDs []string
	for i, seconds := range []int{0, 5, 30, 60, 61} {
		eventIDs = append(
			eventIDs,
			processExportEvent(t, o, i+1, event.LoginStarted, now.Add(-time.Duration(seconds)*time.Second)),
		)
		eventIDs = append(eventIDs, processExportEvent(t, o, i+1, event.LoginModeDetermined, now))
		eventIDs = append(eventIDs, processExportEvent(t, o, i+1, event.LoginSucceeded, now))
	}
	processExportEvent(t, o, 10, event.LoginFailed, now)
	processExportEvent(t, o, 10, event.LoginModeDetermined, now)
	failure.Store(true)
	if err := flush(t, p); err == nil || strings.Contains(err.Error(), "synthetic-secret") {
		t.Fatalf("ошибка отправки: %v", err)
	}
	failed := checkWire(t, receive(t, requests))
	if calls.Load() != 2 {
		t.Fatal("retry внутри попытки")
	}
	processExportEvent(t, o, 11, event.LoginRejected, now)
	failure.Store(false)
	if err := flush(t, p); err != nil {
		t.Fatal(err)
	}
	full := checkWire(t, receive(t, requests))
	if full.completed["success"] != 5 || full.completed["technical_error"] != 1 || full.completed["rejected"] != 1 ||
		full.errors["credentials"] != 1 ||
		full.histogram.Count != 5 ||
		full.histogram.GetSum() != 156 ||
		!reflect.DeepEqual(full.histogram.BucketCounts, []uint64{2, 1, 1, 1}) {
		t.Fatalf("накопление: %+v", full)
	}
	if failed.completed["success"] != 5 || failed.completed["rejected"] != 0 || baseline.instance != full.instance {
		t.Fatal("восстановление/ID")
	}
	for key, start := range baseline.starts {
		if full.starts[key] != start {
			t.Fatal("сброс start timestamp")
		}
	}
	if err := flush(t, p); err != nil {
		t.Fatal(err)
	}
	repeated := checkWire(t, receive(t, requests))
	if !reflect.DeepEqual(repeated.completed, full.completed) || !reflect.DeepEqual(repeated.starts, full.starts) ||
		repeated.histogram.Count != full.histogram.Count ||
		repeated.histogram.GetSum() != full.histogram.GetSum() ||
		!reflect.DeepEqual(repeated.histogram.BucketCounts, full.histogram.BucketCounts) {
		t.Fatal("повторный сбор изменил накопление")
	}
	if err := p.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	receive(t, closedConnections)
	finalRequest := receive(t, requests)
	final := checkWire(t, finalRequest)
	encoded, err := protojson.Marshal(finalRequest)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range eventIDs {
		if strings.Contains(string(encoded), id) {
			t.Fatal("event_id попал в OTLP")
		}
	}
	if final.completed["rejected"] != 1 || calls.Load() != 5 {
		t.Fatal("не один финальный экспорт")
	}
	if err := flush(t, p); err == nil {
		t.Fatal("отправка после Shutdown")
	}
}

func TestPipelineTimeoutAndHTTPIndependence(t *testing.T) {
	entered, release := make(chan struct{}, 2), make(chan struct{})
	server := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { entered <- struct{}{}; <-release }),
	)
	defer server.Close()
	defer close(release)
	settings := pipelineSettings(server.URL + "/v1/metrics")
	settings.ExportTimeout = 100 * time.Millisecond
	p, err := StartPipeline(t.Context(), preparedRecorder(t), settings, testLogger())
	cleanupPipeline(t, p)
	if err != nil {
		t.Fatal(err)
	}
	o, now := observerFor(t, p.Recorder)
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		done <- p.Recorder.provider.ForceFlush(ctx)
	}()
	receive(t, entered)
	handler, err := httpapi.NewHandler(o.Process, testLogger(), httpapi.DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(
		`{"event_id":"019aabcd-0000-7000-8000-000000000001","scenario_id":"019aabcd-0000-7000-9000-000000000100","occurred_at":%q,"source":"mis","type":"login_succeeded"}`,
		now.Format("2006-01-02T15:04:05.000Z"),
	)
	request := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("экспорт повлиял на HTTP: %d", response.Code)
	}
	if err := receive(t, done); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("timeout: %v", err)
	}
	if len(entered) != 0 {
		t.Fatal("retry зависшей отправки")
	}
	// Отмена запрещает финальную отправку, но reader/exporter всё равно освобождаются.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Shutdown(ctx); err == nil {
		t.Fatal("потеряна ошибка отмены shutdown")
	}
}

func TestPipelineRejectsUntrustedHTTPS(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	p, err := StartPipeline(t.Context(), preparedRecorder(t), pipelineSettings(server.URL+"/v1/metrics"), testLogger())
	cleanupPipeline(t, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := flush(t, p); err == nil {
		t.Fatal("недоверенный сертификат принят")
	}
	if err := p.Shutdown(t.Context()); err == nil {
		t.Fatal("ошибка финального экспорта потеряна")
	}
	if calls.Load() != 0 {
		t.Fatal("HTTP достигнут без доверенного TLS")
	}
}

func TestPipelineCanceledBeforeActivation(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := preparedRecorder(t)
	// Отмена между подготовкой и активацией не должна запускать reader.
	// Очистка уже запущенного pipeline отдельно проверяется lifecycle-тестами сервиса.
	cancel()
	p, err := StartPipeline(ctx, r, pipelineSettings(server.URL+"/v1/metrics"), testLogger())
	cleanupPipeline(t, p)
	if p != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("отмена создания: %v", err)
	}
	if r.provider != nil || calls.Load() != 0 {
		t.Fatal("SDK или отправка при отмене активации")
	}
}

func TestPipelineRejectsUnpreparedRecorder(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		recorder *Recorder
	}{
		{name: "nil"},
		{name: "нулевой Recorder", recorder: &Recorder{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, err := StartPipeline(t.Context(), test.recorder,
				pipelineSettings("http://localhost:4318/v1/metrics"), testLogger())
			cleanupPipeline(t, p)
			if p != nil || err == nil {
				t.Fatalf("принят неподготовленный Recorder: %v", err)
			}
		})
	}
}

func TestPipelineRejectsChangedEnvironment(t *testing.T) {
	r := preparedRecorder(t)
	t.Setenv("OTEL_SERVICE_NAME", "synthetic-secret")
	p, err := StartPipeline(t.Context(), r, pipelineSettings("http://localhost:4318/v1/metrics"), testLogger())
	cleanupPipeline(t, p)
	if p != nil || err == nil || !strings.Contains(err.Error(), "OTEL_SERVICE_NAME") ||
		strings.Contains(err.Error(), "synthetic-secret") {
		t.Fatalf("изменённое SDK-env не отклонено безопасно: %v", err)
	}
}

func TestPipelineInvalidSettings(t *testing.T) {
	t.Parallel()
	for _, change := range []struct {
		name  string
		apply func(*PipelineSettings)
	}{
		{"endpoint", func(s *PipelineSettings) { s.Endpoint = "http://localhost" }},
		{"interval", func(s *PipelineSettings) { s.Interval = 0 }},
		{"export timeout", func(s *PipelineSettings) { s.ExportTimeout = -1 }},
	} {
		t.Run(change.name, func(t *testing.T) {
			s := pipelineSettings("http://localhost:4318/v1/metrics")
			change.apply(&s)
			p, err := StartPipeline(t.Context(), preparedRecorder(t), s, testLogger())
			cleanupPipeline(t, p)
			if p != nil || err == nil {
				t.Fatalf("невалидные настройки: %v", err)
			}
		})
	}
	s := pipelineSettings("http://localhost:4318/v1/metrics")
	r := preparedRecorder(t)
	p, err := StartPipeline(t.Context(), r, s, nil)
	cleanupPipeline(t, p)
	if p != nil || err == nil {
		t.Fatal("принят nil logger")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	p, err = StartPipeline(ctx, r, s, testLogger())
	cleanupPipeline(t, p)
	if p != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("отмена до создания: %v", err)
	}
}

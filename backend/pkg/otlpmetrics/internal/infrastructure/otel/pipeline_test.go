package otel

import (
	"context"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	collector "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
)

const (
	testCounterName = "core.pipeline.test_total"
	testScopeName   = "core-test"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func configurePipeline(t *testing.T, endpoint string) {
	t.Helper()
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", endpoint)
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TIMEOUT", "1000")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE", "cumulative")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
	t.Setenv("OTEL_METRIC_EXPORT_TIMEOUT", "1000")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.instance.id=test-instance")
}

func cleanupPipeline(t *testing.T, p *Pipeline) {
	t.Helper()
	if p == nil {
		return
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// Страховка при Fatal: освобождаем SDK без финальной отправки.
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
	return p.provider.ForceFlush(ctx)
}

// recordTestValue создаёт технический инструмент в meter общего provider
// и регистрирует одно значение. Предметные инструменты и их проводной
// контракт проверяют тесты функциональных модулей.
func recordTestValue(t *testing.T, p *Pipeline) {
	t.Helper()
	counter, err := p.Meter(testScopeName).Int64Counter(testCounterName, metric.WithUnit("1"))
	if err != nil {
		t.Fatal(err)
	}
	counter.Add(context.Background(), 1, metric.WithAttributes(attribute.String("source", "synthetic")))
}

func attrs(values []*commonpb.KeyValue) map[string]string {
	m := make(map[string]string)
	for _, v := range values {
		m[v.Key] = v.Value.GetStringValue()
	}
	return m
}

// checkWire проверяет проводной контракт механизма: один Resource из окружения,
// один scope, единственная техническая метрика, cumulative temporality,
// отсутствие exemplars и посторонних данных. Возвращает start timestamp точки.
func checkWire(t *testing.T, request *collector.ExportMetricsServiceRequest) uint64 {
	t.Helper()
	if len(request.ResourceMetrics) != 1 {
		t.Fatal("не один Resource")
	}
	rm := request.ResourceMetrics[0]
	res := attrs(rm.Resource.Attributes)
	if !reflect.DeepEqual(
		res,
		map[string]string{
			"service.instance.id": "test-instance",
		},
	) ||
		len(rm.Resource.Attributes) != 1 {
		t.Fatalf("Resource: %v", res)
	}
	if len(rm.ScopeMetrics) != 1 || rm.ScopeMetrics[0].Scope.Name != testScopeName ||
		rm.ScopeMetrics[0].Scope.Version != "" ||
		len(rm.ScopeMetrics[0].Scope.Attributes) != 0 ||
		rm.SchemaUrl != "" {
		t.Fatal("неожиданный scope/схема")
	}
	if len(rm.ScopeMetrics[0].Metrics) != 1 {
		t.Fatalf("число метрик: %d", len(rm.ScopeMetrics[0].Metrics))
	}
	m := rm.ScopeMetrics[0].Metrics[0]
	if m.Name != testCounterName || m.Description != "" {
		t.Fatal("неожиданная метрика")
	}
	s := m.GetSum()
	if s == nil || !s.IsMonotonic ||
		s.AggregationTemporality != metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE ||
		m.Unit != "1" {
		t.Fatal("контракт суммы")
	}
	if len(s.DataPoints) != 1 {
		t.Fatal("число точек суммы")
	}
	point := s.DataPoints[0]
	if len(point.Attributes) != 1 || attrs(point.Attributes)["source"] != "synthetic" ||
		len(point.Exemplars) != 0 || point.Value == nil || point.GetAsInt() <= 0 {
		t.Fatal("атрибуты и значение Counter")
	}
	encoded, err := protojson.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"synthetic-secret", "http://"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("посторонние данные в OTLP: %s", forbidden)
		}
	}
	return point.StartTimeUnixNano
}

// Технический проводной контракт: настоящий SDK + protobuf + OTLP/HTTP.
func TestPipelineOTLPHTTP(t *testing.T) {
	requests := make(chan *collector.ExportMetricsServiceRequest, 20)
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
	server.Start()
	defer server.Close()
	configurePipeline(t, server.URL+"/v1/metrics")
	p := NewPipeline(nil, testLogger())
	cleanupPipeline(t, p)
	if err := p.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	recordTestValue(t, p)
	if flushErr := flush(t, p); flushErr != nil {
		t.Fatal(flushErr)
	}
	baseline := checkWire(t, receive(t, requests))
	if err := p.Start(t.Context()); err == nil {
		t.Fatal("разрешён повторный запуск pipeline")
	}
	failure.Store(true)
	if flushErr := flush(t, p); flushErr == nil || strings.Contains(flushErr.Error(), "synthetic-secret") {
		t.Fatalf("ошибка отправки: %v", flushErr)
	}
	checkWire(t, receive(t, requests))
	if calls.Load() != 2 {
		t.Fatal("неожиданное число попыток в пределах короткого timeout")
	}
	failure.Store(false)
	if flushErr := flush(t, p); flushErr != nil {
		t.Fatal(flushErr)
	}
	full := checkWire(t, receive(t, requests))
	if full != baseline {
		t.Fatal("сброс start timestamp")
	}
	if flushErr := flush(t, p); flushErr != nil {
		t.Fatal(flushErr)
	}
	repeated := checkWire(t, receive(t, requests))
	if repeated != full {
		t.Fatal("повторный сбор изменил накопление")
	}
	if shutdownErr := p.Shutdown(t.Context()); shutdownErr != nil {
		t.Fatal(shutdownErr)
	}
	if final := checkWire(t, receive(t, requests)); final != baseline {
		t.Fatal("сброс start timestamp в финальном экспорте")
	}
	if err := flush(t, p); err == nil {
		t.Fatal("отправка после Shutdown")
	}
}

func TestPipelineTimeoutAndRelease(t *testing.T) {
	entered, release := make(chan struct{}, 2), make(chan struct{})
	server := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { entered <- struct{}{}; <-release }),
	)
	defer server.Close()
	defer close(release)
	configurePipeline(t, server.URL+"/v1/metrics")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TIMEOUT", "100")
	p := NewPipeline(nil, testLogger())
	cleanupPipeline(t, p)
	if err := p.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	recordTestValue(t, p)
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		done <- p.provider.ForceFlush(ctx)
	}()
	receive(t, entered)
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
	configurePipeline(t, server.URL+"/v1/metrics")
	p := NewPipeline(nil, testLogger())
	cleanupPipeline(t, p)
	if err := p.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	recordTestValue(t, p)
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

func TestPipelineCanceledBeforeStart(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { calls.Add(1) }))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Отмена до запуска не должна запускать reader. Очистка уже запущенного
	// pipeline проверяется lifecycle-тестами сборки модуля-потребителя.
	cancel()
	configurePipeline(t, server.URL+"/v1/metrics")
	p := NewPipeline(nil, testLogger())
	cleanupPipeline(t, p)
	if err := p.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("отмена создания: %v", err)
	}
	if p.provider != nil || calls.Load() != 0 {
		t.Fatal("SDK или отправка при отмене запуска")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("незапущенный pipeline освобождается без ошибки: %v", err)
	}
}

func TestPipelineRequiresLogger(t *testing.T) {
	t.Parallel()
	p := NewPipeline(nil, nil)
	cleanupPipeline(t, p)
	if err := p.Start(t.Context()); err == nil {
		t.Fatal("принят nil logger")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	p = NewPipeline(nil, testLogger())
	cleanupPipeline(t, p)
	if err := p.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("отмена до создания: %v", err)
	}
}

func TestPipelineUsesSDKCertificate(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer server.Close()
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caPath, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	configurePipeline(t, server.URL+"/v1/metrics")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_CERTIFICATE", caPath)
	// Специфичный metrics endpoint должен иметь приоритет над общим endpoint.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	p := NewPipeline(nil, testLogger())
	cleanupPipeline(t, p)
	if err := p.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	recordTestValue(t, p)
	if err := flush(t, p); err != nil || calls.Load() != 1 {
		t.Fatalf("экспорт с CA из окружения SDK не выполнен: %v, запросов %d", err, calls.Load())
	}
}

func TestPipelineSDKEndpointEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "общий endpoint", path: "/prefix/v1/metrics"},
		{name: "metrics endpoint", path: "/custom-metrics"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != tc.path {
					t.Errorf("путь SDK: получено %s, ожидалось %s", r.URL.Path, tc.path)
				}
				w.Header().Set("Content-Type", "application/x-protobuf")
			}))
			defer server.Close()
			configurePipeline(t, server.URL+tc.path)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", server.URL+"/prefix")
			if tc.name == "общий endpoint" {
				t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
			}
			p := NewPipeline(nil, testLogger())
			cleanupPipeline(t, p)
			if err := p.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			recordTestValue(t, p)
			if err := flush(t, p); err != nil || calls.Load() != 1 {
				t.Fatalf("endpoint SDK не применён: %v, запросов %d", err, calls.Load())
			}
		})
	}
}

func TestPipelineUsesSDKRetryAndRedirect(t *testing.T) {
	for _, mode := range []string{"retry", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			var calls, delivered atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempt := calls.Add(1)
				if attempt == 1 {
					if mode == "retry" {
						w.WriteHeader(http.StatusServiceUnavailable)
					} else {
						http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
					}
					return
				}
				if r.Method != http.MethodPost || (mode == "redirect" && r.URL.Path != "/redirected") {
					t.Error("SDK изменил метод или не выполнил redirect")
				}
				delivered.Add(1)
				w.Header().Set("Content-Type", "application/x-protobuf")
			}))
			defer server.Close()
			configurePipeline(t, server.URL+"/v1/metrics")
			// Retry SDK использует штатную задержку с jitter до 7,5 с перед второй попыткой.
			t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TIMEOUT", "15000")
			t.Setenv("OTEL_METRIC_EXPORT_TIMEOUT", "15000")
			p := NewPipeline(nil, testLogger())
			cleanupPipeline(t, p)
			if err := p.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			recordTestValue(t, p)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			if err := p.provider.ForceFlush(ctx); err != nil || delivered.Load() != 1 || calls.Load() != 2 {
				t.Fatalf("штатный %s SDK не сработал: %v, запросов %d", mode, err, calls.Load())
			}
		})
	}
}

// View потребителя передаётся в provider и применяется SDK к его инструментам.
func TestPipelineAppliesConsumerViews(t *testing.T) {
	requests := make(chan *collector.ExportMetricsServiceRequest, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	configurePipeline(t, server.URL+"/v1/metrics")
	views := []sdkmetric.View{sdkmetric.NewView(
		sdkmetric.Instrument{Name: testCounterName},
		sdkmetric.Stream{Aggregation: sdkmetric.AggregationDrop{}},
	)}
	p := NewPipeline(views, testLogger())
	cleanupPipeline(t, p)
	if err := p.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	recordTestValue(t, p)
	if err := flush(t, p); err != nil {
		t.Fatal(err)
	}
	request := receive(t, requests)
	for _, rm := range request.ResourceMetrics {
		for _, scope := range rm.ScopeMetrics {
			for _, m := range scope.Metrics {
				if m.Name == testCounterName {
					t.Fatal("View потребителя не применён к инструменту")
				}
			}
		}
	}
}

func TestPipelineResourceFromEnvironment(t *testing.T) {
	t.Setenv(
		"OTEL_RESOURCE_ATTRIBUTES",
		"test.source=env,service.name=attributes-service,service.instance.id=env-instance",
	)
	t.Setenv("OTEL_SERVICE_NAME", "env-service")
	reader := sdkmetric.NewManualReader()
	provider := newProvider(resource.Environment(), reader, nil)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &collected); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"test.source":         "env",
		"service.name":        "env-service",
		"service.instance.id": "env-instance",
	} {
		got, ok := collected.Resource.Set().Value(attribute.Key(key))
		if !ok || got.AsString() != want {
			t.Fatalf("атрибут %s: получено %v, ожидалось %s", key, got, want)
		}
	}
}

func TestPipelineResourceDoesNotGenerateInstanceID(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	reader := sdkmetric.NewManualReader()
	provider := newProvider(resource.Environment(), reader, nil)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &collected); err != nil {
		t.Fatal(err)
	}
	if _, ok := collected.Resource.Set().Value("service.instance.id"); ok {
		t.Fatal("service.instance.id создан без настройки окружения")
	}
}

package bootstrap

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	collector "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/vendelev/sre-metrics/core/oteltest"
	userlogin "github.com/vendelev/sre-metrics/user-login/bootstrap"
)

func serviceEventJSON(id, scenario int, kind string, at time.Time) string {
	source, extra := "mis", ""
	if kind == "login_mode_determined" {
		source, extra = "auth", `,"mode":"credentials"`
	}
	return fmt.Sprintf(
		`{"event_id":"019aabcd-0000-7000-8000-%012x","scenario_id":"019aabcd-0000-7000-9000-%012x","occurred_at":%q,"source":%q,"type":%q%s}`,
		id,
		scenario,
		at.Format("2006-01-02T15:04:05.000Z"),
		source,
		kind,
		extra,
	)
}

func sendServiceEvent(
	t *testing.T,
	running *runningService,
	id, scenario int,
	kind string,
	at time.Time,
) error {
	t.Helper()
	request, err := http.NewRequest(
		http.MethodPost,
		"http://"+running.address.String()+"/metrics/b/user-login/events",
		strings.NewReader(serviceEventJSON(id, scenario, kind, at)),
	)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusAccepted {
		return fmt.Errorf("HTTP %d вместо 202", response.StatusCode)
	}
	return nil
}

func publishInboxFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path+".tmp", []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".tmp", path+".json"); err != nil {
		t.Fatal(err)
	}
	return path + ".json"
}

type serviceValues struct {
	completed   map[string]int64
	errors      map[string]int64
	histogram   *metricpb.HistogramDataPoint
	instance    string
	starts      map[string]uint64
	inbox       map[string]int64
	inboxGauges map[string]bool
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
	instance := resource["service.instance.id"]
	if len(rm.Resource.Attributes) != 3 || !reflect.DeepEqual(resource, map[string]string{
		"service.instance.id":         "service-test-instance",
		"host.name":                   "node-test",
		"deployment.environment.name": "test",
	}) {
		t.Fatalf("Resource: %v", resource)
	}
	if len(rm.ScopeMetrics) != 1 || rm.ScopeMetrics[0].Scope.Name != userlogin.ScopeName {
		t.Fatal("scope сервиса")
	}
	values := serviceValues{
		completed:   make(map[string]int64),
		errors:      make(map[string]int64),
		starts:      make(map[string]uint64),
		instance:    instance,
		inbox:       make(map[string]int64),
		inboxGauges: make(map[string]bool),
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
				case userlogin.CompletedName:
					values.completed[value] = point.GetAsInt()
				case userlogin.TechnicalErrorsName:
					values.errors[value] = point.GetAsInt()
				case "file.inbox.events":
					if point.Attributes[0].Key != "outcome" {
						t.Fatal("лишний атрибут Counter файлового входа")
					}
					values.inbox[value] = point.GetAsInt()
					continue
				default:
					t.Fatal("лишний Counter")
				}
				values.starts[m.Name+value] = point.StartTimeUnixNano
			}
			continue
		}
		if m.GetGauge() != nil {
			checkInboxGauge(t, m)
			values.inboxGauges[m.Name] = true
			continue
		}
		h := m.GetHistogram()
		if m.Name != userlogin.DurationName || m.Unit != "s" || h == nil ||
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

func checkInboxGauge(t *testing.T, m *metricpb.Metric) {
	t.Helper()
	units := map[string]string{
		"file.inbox.files":           "{file}",
		"file.inbox.size":            "By",
		"file.inbox.oldest_file.age": "s",
		"file.inbox.volume.usage":    "1",
	}
	points := m.GetGauge().DataPoints
	if m.Unit == "" || m.Unit != units[m.Name] || len(points) != 1 || len(points[0].Attributes) != 0 {
		t.Fatalf("неверный Gauge файлового входа: %s", m)
	}
}

func nextServiceMetrics(
	t *testing.T,
	r *oteltest.Receiver,
	predicate func(oteltest.Sample, serviceValues) bool,
) serviceValues {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case sample := <-r.Samples():
			values := serviceMetrics(t, sample.Request)
			if predicate(sample, values) {
				return values
			}
		case <-timer.C:
			t.Fatal("нет ожидаемого OTLP-пакета")
			return serviceValues{}
		}
	}
}

func lastServiceMetrics(t *testing.T, r *oteltest.Receiver) serviceValues {
	t.Helper()
	var last *collector.ExportMetricsServiceRequest
	for {
		select {
		case sample := <-r.Samples():
			last = sample.Request
		default:
			if last == nil {
				t.Fatal("нет финального пакета")
			}
			return serviceMetrics(t, last)
		}
	}
}

// BR-25, BR-41: HTTP и файл дополняют один сценарий, а SDK экспортирует его
// исход и длительность. Имя файла не определяет event_id.
func TestAppInboxExportsSharedState(t *testing.T) {
	r := oteltest.NewReceiver(t)
	cfg, settings, process := actualConfig(t, r)
	c := &testClock{now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	app := testApp(t, process, cfg, settings, c)
	running := runActual(t, app)
	if app.module.InboxTask() == nil || app.server.ServeTask() == nil {
		t.Fatal("готовность объявлена до запуска обоих входов")
	}
	now := c.Now()
	if err := sendServiceEvent(t, running, 1, 100, "login_started", now.Add(-5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := sendServiceEvent(t, running, 2, 100, "login_mode_determined", now); err != nil {
		t.Fatal(err)
	}
	path := publishInboxFile(t, cfg.Inbox.Dir, "019aabcd-0000-7000-8000-000000000099",
		serviceEventJSON(3, 100, "login_succeeded", now))
	publishInboxFile(t, cfg.Inbox.Dir, "019aabcd-0000-7000-8000-000000000098", "синтетический секрет")
	sample := nextServiceMetrics(t, r, func(_ oteltest.Sample, v serviceValues) bool {
		return v.histogram != nil && v.inbox["accepted"] == 1 && v.inbox["invalid_event"] == 1
	})
	if sample.completed["success"] != 1 || sample.histogram.Count != 1 || sample.histogram.GetSum() != 5 {
		t.Fatalf("HTTP и файл не дополнили один сценарий: %+v", sample)
	}
	if len(sample.inbox) != 5 || !sample.inboxGauges["file.inbox.oldest_file.age"] {
		t.Fatalf("неполная диагностика файлового входа: %+v", sample)
	}
	if err := running.stop(t); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("принятый файл остался после завершения: %v", err)
	}
}

// BR-42, AC-31: обслуживание завершается до последнего пакета, а принятые
// события и drain очереди включаются в него. Дожидание активного обработчика
// HTTP и принудительное закрытие зависшего соединения проверяет пакет httpserver.
func TestAppOTLPFinalAfterProcessing(t *testing.T) {
	r := oteltest.NewReceiver(t)
	cfg, settings, process := actualConfig(t, r)
	cfg.Processing.MaintenanceInterval = 20 * time.Millisecond
	// Явная View гистограммы модуля сохраняет контракт buckets независимо
	// от агрегации, выбранной экспортёром по умолчанию.
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_DEFAULT_HISTOGRAM_AGGREGATION", "base2_exponential_bucket_histogram")
	c := newBarrierClock(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC))
	app := testApp(t, process, cfg, settings, c)
	releaseMaintenance := sync.OnceFunc(func() { close(c.release) })
	defer releaseMaintenance()
	running := runActual(t, app)
	baseline := nextServiceMetrics(
		t,
		r,
		func(_ oteltest.Sample, v serviceValues) bool { return v.completed["success"] == 0 },
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
		if err := sendServiceEvent(t, running, event.id, event.scenario, event.kind, event.at); err != nil {
			t.Fatal(err)
		}
	}
	// Очередь пуста, поэтому ближайший тик обслуживания останавливается барьером.
	c.armed.Store(true)
	waitSignal(t, c.entered)
	// Событие 4 принято в очередь, пока воркер приостановлен обслуживанием:
	// его обработка войдёт в drain и должна попасть в последний пакет.
	if err := sendServiceEvent(t, running, 4, 200, "login_succeeded", now); err != nil {
		t.Fatal(err)
	}
	c.Advance(2 * time.Second)
	running.cancel()
	// Закрытие listener — первый шаг остановки: новые соединения отклоняются.
	waitListenerClosed(t, running.address)
	select {
	case <-running.done:
		t.Fatal("остановка не дождалась обслуживания")
	default:
	}
	releaseMaintenance()
	waitSignal(t, c.exited)
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
	select {
	case <-r.Samples():
		t.Fatal("отправка после остановки")
	case <-time.After(60 * time.Millisecond):
	}
}

// BR-41, AC-30: сеть не удерживает HTTP и не сбрасывает cumulative-накопление.
func TestAppOTLPRecovery(t *testing.T) {
	r := oteltest.NewReceiver(t)
	r.SetFail(true)
	cfg, settings, process := actualConfig(t, r)
	c := &testClock{now: time.Now().UTC().Truncate(time.Millisecond)}
	app := testApp(t, process, cfg, settings, c)
	running := runActual(t, app)
	baseline := nextServiceMetrics(t, r, func(sample oteltest.Sample, _ serviceValues) bool { return sample.Failed })
	r.SetHold(true)
	waitSignal(t, r.Entered())
	for id := 1; id <= 2; id++ {
		if err := sendServiceEvent(t, running, id, id, "login_succeeded", c.Now()); err != nil {
			t.Fatal(err)
		}
	}
	r.Release()
	nextServiceMetrics(t, r, func(sample oteltest.Sample, v serviceValues) bool {
		return sample.Failed && v.completed["success"] == 2
	})
	r.SetFail(false)
	recovered := nextServiceMetrics(t, r, func(sample oteltest.Sample, v serviceValues) bool {
		return !sample.Failed && v.completed["success"] == 2
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

func TestAppPipelineFailureClosesListener(t *testing.T) {
	r := oteltest.NewReceiver(t)
	cfg, settings, process := actualConfig(t, r)
	app := testApp(t, process, cfg, settings, &testClock{now: time.Now()})
	// SDK отклоняет сочетание HTTP endpoint и настроенного TLS при инициализации после bind.
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer tlsServer.Close()
	caPath := t.TempDir() + "/ca.pem"
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsServer.Certificate().Raw})
	if err := os.WriteFile(caPath, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_CERTIFICATE", caPath)
	ready := false
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err := app.run(ctx, func(net.Addr) { ready = true })
	if err == nil || !strings.Contains(err.Error(), "создание OTLP exporter") {
		t.Fatalf("ошибка pipeline: %v", err)
	}
	assertListenerClosed(t, app)
	if ready || r.Calls() != 0 {
		t.Fatal("ресурсы после отказа инициализации")
	}
}

func TestAppFinalExportTimeout(t *testing.T) {
	r := oteltest.NewReceiver(t)
	r.SetHold(true)
	cfg, settings, process := actualConfig(t, r)
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TIMEOUT", "100")
	c := &testClock{now: time.Now().UTC().Truncate(time.Millisecond)}
	app := testApp(t, process, cfg, settings, c)
	running := runActual(t, app)
	if err := sendServiceEvent(t, running, 1, 1, "login_succeeded", c.Now()); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := running.stop(t)
	// Штатный retry SDK может ждать следующую попытку до общего deadline остановки.
	// Допуск учитывает планировщик и race detector, но не допускает нового бюджета SDK.
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*process.ShutdownTimeout/2 {
		t.Fatalf("неограниченный/скрытый отказ экспорта: %v", err)
	}
	if r.Calls() != 1 {
		t.Fatal("повтор финальной отправки")
	}
	if lastServiceMetrics(t, r).completed["success"] != 1 {
		t.Fatal("содержимое последней попытки")
	}
}

// Настоящее обслуживание расходует часть бюджета; зависший OTLP-получатель
// должен получить лишь остаток, а не новый полный timeout остановки.
func TestAppSharedShutdownBudget(t *testing.T) {
	r := oteltest.NewReceiver(t)
	r.SetHold(true)
	cfg, settings, process := actualConfig(t, r)
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TIMEOUT", "5000")
	process.ShutdownTimeout = time.Second
	cfg.Processing.MaintenanceInterval = time.Millisecond
	c := newBarrierClock(time.Now().UTC().Truncate(time.Millisecond))
	app := testApp(t, process, cfg, settings, c)
	releaseMaintenance := sync.OnceFunc(func() { close(c.release) })
	defer releaseMaintenance()
	running := runActual(t, app)
	if err := sendServiceEvent(t, running, 1, 1, "login_succeeded", c.Now()); err != nil {
		t.Fatal(err)
	}
	c.armed.Store(true)
	waitSignal(t, c.entered)
	started := time.Now()
	running.cancel()
	// Закрытие listener — первый шаг остановки и детерминированный сигнал её начала.
	waitListenerClosed(t, running.address)
	// Ожидание намеренно расходует бюджет, а не задаёт порядок goroutine:
	// начало Shutdown и завершение обслуживания подтверждаются отдельными сигналами.
	timer := time.NewTimer(3 * process.ShutdownTimeout / 4)
	defer timer.Stop()
	<-timer.C
	releaseMaintenance()
	waitSignal(t, c.exited)
	waitSignal(t, r.Entered())
	if err := running.stop(t); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("исчерпание общего бюджета не сообщено: %v", err)
	}
	// Допуск нужен планировщику и race detector; отдельный полный бюджет SDK
	// увеличил бы длительность как минимум до 1,75 общего бюджета.
	if elapsed := time.Since(started); elapsed > 3*process.ShutdownTimeout/2 {
		t.Fatalf("SDK получил новый бюджет после обслуживания: %s", elapsed)
	}
	if lastServiceMetrics(t, r).completed["success"] != 1 || r.Calls() != 1 {
		t.Fatal("неверная или повторная финальная попытка экспорта")
	}
}

// Сеть здесь настоящая; граница незавершённого обслуживания управляется каналом.
// Дожидание активного обработчика HTTP и принудительное закрытие зависшего
// соединения проверяет пакет httpserver.
func TestAppIncompleteDrainShutsDownSDK(t *testing.T) {
	r := oteltest.NewReceiver(t)
	cfg, settings, process := actualConfig(t, r)
	process.ShutdownTimeout = 300 * time.Millisecond
	cfg.Processing.MaintenanceInterval = time.Millisecond
	c := newBarrierClock(time.Now().UTC().Truncate(time.Millisecond))
	app := testApp(t, process, cfg, settings, c)
	releaseOnce := sync.OnceFunc(func() { close(c.release) })
	defer releaseOnce()
	c.armed.Store(true)
	running := runActual(t, app)
	waitSignal(t, c.entered)
	if err := sendServiceEvent(t, running, 1, 1, "login_succeeded", c.Now()); err != nil {
		t.Fatal(err)
	}
	if err := running.stop(t); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("незавершённое обслуживание скрыто: %v", err)
	}
	// Обычный Shutdown отправляет доступные значения, но обслуживание
	// ещё заблокировано. Даже успешная отправка не делает остановку успешной
	// и не означает окончательного снимка его измерений.
	if lastServiceMetrics(t, r).completed["success"] != 0 {
		t.Fatal("незавершённое обслуживание объявлено учтённым")
	}
	select {
	case <-c.exited:
		t.Fatal("Shutdown SDK завершил заблокированное обслуживание")
	default:
	}
	releaseOnce()
	waitSignal(t, c.exited)
	// Получатель может опубликовать уже начатый до Shutdown запрос после
	// возврата остановки. Продолжающийся периодический экспорт не даст
	// наступить полосе тишины длиной в три интервала.
	quiet := time.NewTimer(60 * time.Millisecond)
	defer quiet.Stop()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case <-r.Samples():
			if !quiet.Stop() {
				select {
				case <-quiet.C:
				default:
				}
			}
			quiet.Reset(60 * time.Millisecond)
		case <-quiet.C:
			return
		case <-timeout.C:
			t.Fatal("экспорт продолжается после аварийного завершения")
		}
	}
}

package oteltest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	collector "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
)

// Sample — один разобранный запрос экспорта с признаком синтетического отказа.
type Sample struct {
	Request *collector.ExportMetricsServiceRequest
	Failed  bool
}

// Receiver — настоящий OTLP/HTTP-получатель на локальном тестовом сервере.
// Проверяет транспортный контракт экспорта, разбирает protobuf и предоставляет
// управляемые отказ и удержание обработки для воспроизведения timeout, retry
// и финального экспорта при остановке.
type Receiver struct {
	server  *httptest.Server
	samples chan Sample
	entered chan struct{}
	release chan struct{}
	fail    atomic.Bool
	hold    atomic.Bool
	calls   atomic.Int64
}

// NewReceiver запускает получателя на случайном свободном порту и освобождает
// его после теста. Контракты пакета и метрик проверяют тесты владельцев.
func NewReceiver(t *testing.T) *Receiver {
	t.Helper()
	r := &Receiver{
		samples: make(chan Sample, 128),
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
		case r.samples <- Sample{Request: message, Failed: failed}:
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

// URL возвращает metrics endpoint получателя для стандартного OTEL-окружения SDK.
func (r *Receiver) URL() string { return r.server.URL + "/v1/metrics" }

// Samples возвращает канал разобранных пакетов экспорта; буфер ограничен,
// нечитаемые пакеты блокируют обработчик.
func (r *Receiver) Samples() <-chan Sample { return r.samples }

// Entered сигнализирует о приёме удерживаемого пакета.
func (r *Receiver) Entered() <-chan struct{} { return r.entered }

// Release снимает удержание обработки пакета; вызывается однократно.
func (r *Receiver) Release() { close(r.release) }

// SetFail включает синтетический отказ получателя: ответ 503 с безопасным
// текстом без секретов.
func (r *Receiver) SetFail(fail bool) { r.fail.Store(fail) }

// SetHold включает удержание обработки принятого пакета до Release.
func (r *Receiver) SetHold(hold bool) { r.hold.Store(hold) }

// Calls возвращает число принятых запросов экспорта.
func (r *Receiver) Calls() int64 { return r.calls.Load() }

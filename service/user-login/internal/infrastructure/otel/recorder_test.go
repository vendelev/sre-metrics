package otel_test

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/vendelev/sre-metrics/service/user-login/internal/application"
	"github.com/vendelev/sre-metrics/service/user-login/internal/domain"
	"github.com/vendelev/sre-metrics/service/user-login/internal/infrastructure/memory"
	"github.com/vendelev/sre-metrics/service/user-login/internal/infrastructure/otel"
	"github.com/vendelev/sre-metrics/service/user-login/internal/infrastructure/otel/oteltest"
)

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

type stack struct {
	recorder *otel.Recorder
	observer *application.LoginObserver
	now      time.Time
	next     int
	ids      []string
}

func newStack(t *testing.T) *stack {
	t.Helper()
	r := oteltest.New(t)
	store, err := memory.NewStore(memory.DefaultLimits(), r)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	observer, err := application.NewLoginObserver(store, fixedClock{now})
	if err != nil {
		t.Fatal(err)
	}
	return &stack{recorder: r, observer: observer, now: now}
}

func (s *stack) event(t *testing.T, scenario string, kind domain.EventType, mode domain.LoginMode, at time.Time) {
	t.Helper()
	s.next++
	id := fmt.Sprintf("019aabcd-0000-7000-8000-%012x", s.next)
	e := domain.Event{EventID: id, ScenarioID: scenario, Type: kind, Source: domain.SourceMIS, OccurredAt: at}
	if kind == domain.EventTypeLoginModeDetermined {
		e.Source, e.Mode = domain.SourceAuth, &mode
	}
	if _, err := s.observer.Process(e); err != nil {
		t.Fatal(err)
	}
	s.ids = append(s.ids, id, scenario)
}

func sdkCollect(t *testing.T, s *stack) metricdata.ResourceMetrics {
	t.Helper()
	before := time.Now()
	rm := oteltest.Collect(t, s.recorder)
	after := time.Now()
	assertTree(t, rm, before, after)
	encoded, err := json.Marshal(rm)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range s.ids {
		if strings.Contains(string(encoded), id) {
			t.Fatal("ID события или сценария попал в SDK")
		}
	}
	return rm
}

func assertTree(t *testing.T, rm metricdata.ResourceMetrics, before, after time.Time) {
	t.Helper()
	wantResource := map[string]string{
		"service.namespace": "sptr", "service.name": "mis-login-observability",
		"host.name": "node-test", "deployment.environment.name": "test",
	}
	if rm.Resource.Len() != 5 {
		t.Fatalf("лишние атрибуты Resource: %v", rm.Resource)
	}
	for key, want := range wantResource {
		value, ok := rm.Resource.Set().Value(attribute.Key(key))
		if !ok || value.AsString() != want {
			t.Fatalf("Resource %s: %v", key, value)
		}
	}
	instance, ok := rm.Resource.Set().Value("service.instance.id")
	id, err := uuid.Parse(instance.AsString())
	if !ok || err != nil || id.Version() != 7 || id.Variant() != uuid.RFC4122 {
		t.Fatalf("ID экземпляра не UUID v7: %v", instance)
	}
	if len(rm.ScopeMetrics) != 1 {
		t.Fatalf("число scope: %d", len(rm.ScopeMetrics))
	}
	scope := rm.ScopeMetrics[0]
	if scope.Scope.Name != otel.ScopeName || scope.Scope.Version != "" || scope.Scope.SchemaURL != "" ||
		scope.Scope.Attributes.Len() != 0 {
		t.Fatalf("неожиданный scope: %+v", scope.Scope)
	}
	seen := make(map[string]bool)
	for _, m := range scope.Metrics {
		if seen[m.Name] {
			t.Fatal("дублирующийся инструмент")
		}
		seen[m.Name] = true
		if m.Description != "" {
			t.Fatal("неожиданное описание инструмента")
		}
		switch m.Name {
		case otel.CompletedName, otel.TechnicalErrorsName:
			data, ok := m.Data.(metricdata.Sum[int64])
			if !ok || m.Unit != "1" || !data.IsMonotonic || data.Temporality != metricdata.CumulativeTemporality {
				t.Fatalf("контракт Counter: %+v", m)
			}
			key, values := "outcome", []string{"success", "technical_error", "rejected", "unknown"}
			if m.Name == otel.TechnicalErrorsName {
				key, values = "mode", []string{"credentials", "existing_session"}
			}
			if len(data.DataPoints) != len(values) {
				t.Fatal("неверное число рядов Counter")
			}
			for _, p := range data.DataPoints {
				assertAttributes(t, p.Attributes, key, values)
				assertTime(t, p.StartTime, p.Time, before, after)
				if p.Value < 0 || len(p.Exemplars) != 0 {
					t.Fatal("отрицательная сумма или exemplars Counter")
				}
			}
		case otel.DurationName:
			data, ok := m.Data.(metricdata.Histogram[float64])
			if !ok || m.Unit != "s" || data.Temporality != metricdata.CumulativeTemporality {
				t.Fatalf("контракт Histogram: %+v", m)
			}
			for _, p := range data.DataPoints {
				assertAttributes(t, p.Attributes, "mode", []string{"credentials", "existing_session"})
				assertTime(t, p.StartTime, p.Time, before, after)
				if !reflect.DeepEqual(p.Bounds, []float64{5, 30, 60}) || len(p.BucketCounts) != 4 {
					t.Fatal("границы Histogram")
				}
				_, hasMin := p.Min.Value()
				_, hasMax := p.Max.Value()
				if hasMin || hasMax || len(p.Exemplars) != 0 {
					t.Fatal("Histogram содержит min/max или exemplars")
				}
				var count uint64
				for _, n := range p.BucketCounts {
					count += n
				}
				if count != p.Count {
					t.Fatal("count не равен сумме непересекающихся корзин")
				}
			}
		default:
			t.Fatalf("неожиданный инструмент: %s", m.Name)
		}
	}
	if !seen[otel.CompletedName] || !seen[otel.TechnicalErrorsName] {
		t.Fatal("нет нулевых Counter")
	}
}

func assertAttributes(t *testing.T, attrs attribute.Set, key string, values []string) {
	t.Helper()
	value, ok := attrs.Value(attribute.Key(key))
	if attrs.Len() != 1 || !ok || value.Type() != attribute.STRING {
		t.Fatalf("неожиданные атрибуты: %v", attrs)
	}
	for _, allowed := range values {
		if value.AsString() == allowed {
			return
		}
	}
	t.Fatalf("неожиданное значение атрибута: %v", value)
}

func assertTime(t *testing.T, start, collected, before, after time.Time) {
	t.Helper()
	if start.IsZero() || start.After(collected) || collected.Before(before) || collected.After(after) {
		t.Fatalf("времена SDK: start=%s, сбор=%s, интервал=[%s, %s]", start, collected, before, after)
	}
}

func starts(rm metricdata.ResourceMetrics) map[string]time.Time {
	result := make(map[string]time.Time)
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range data.DataPoints {
					result[m.Name+p.Attributes.Encoded(attribute.DefaultEncoder())] = p.StartTime
				}
			case metricdata.Histogram[float64]:
				for _, p := range data.DataPoints {
					result[m.Name+p.Attributes.Encoded(attribute.DefaultEncoder())] = p.StartTime
				}
			}
		}
	}
	return result
}

func assertStableStarts(t *testing.T, earlier, later metricdata.ResourceMetrics) {
	t.Helper()
	next := starts(later)
	for key, start := range starts(earlier) {
		if !start.Equal(next[key]) {
			t.Fatalf("начало cumulative-ряда изменилось: %s", key)
		}
	}
}

// BR-34, BR-43–BR-47: реальные application + memory + SDK, без экспортёра.
func TestRecorderSDKContract(t *testing.T) {
	s := newStack(t)
	baseline := sdkCollect(t, s)
	zero := oteltest.Read(t, s.recorder)
	if len(zero.Completed) != 4 || len(zero.TechnicalErrors) != 2 || len(zero.Durations) != 0 {
		t.Fatalf("baseline: %+v", zero)
	}
	for _, value := range zero.Completed {
		if value != 0 {
			t.Fatal("фиктивное завершение")
		}
	}
	for _, value := range zero.TechnicalErrors {
		if value != 0 {
			t.Fatal("фиктивная ошибка")
		}
	}
	for _, scope := range baseline.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == otel.DurationName {
				t.Fatal("фиктивная Histogram до наблюдений")
			}
		}
	}
	durations := []time.Duration{
		0,
		5 * time.Second,
		5001 * time.Millisecond,
		30 * time.Second,
		30001 * time.Millisecond,
		60 * time.Second,
		61 * time.Second,
	}
	for index, mode := range []domain.LoginMode{domain.LoginModeCredentials, domain.LoginModeExistingSession} {
		for i, duration := range durations {
			scenario := fmt.Sprintf("019aabcd-0000-7000-9000-%012x", 100+index*100+i)
			s.event(t, scenario, domain.EventTypeLoginSucceeded, "", s.now)
			s.event(t, scenario, domain.EventTypeLoginStarted, "", s.now.Add(-duration))
			s.event(t, scenario, domain.EventTypeLoginModeDetermined, mode, s.now)
		}
		scenario := fmt.Sprintf("019aabcd-0000-7000-9000-%012x", 1000+index)
		s.event(t, scenario, domain.EventTypeLoginFailed, "", s.now)
		s.event(t, scenario, domain.EventTypeLoginModeDetermined, mode, s.now)
	}
	full := sdkCollect(t, s)
	assertStableStarts(t, baseline, full)
	values := oteltest.Read(t, s.recorder)
	if values.Completed[domain.OutcomeSuccess] != 14 || values.Completed[domain.OutcomeTechnicalError] != 2 {
		t.Fatalf("Counter: %+v", values)
	}
	for _, mode := range []domain.LoginMode{domain.LoginModeCredentials, domain.LoginModeExistingSession} {
		h := values.Durations[mode]
		if values.TechnicalErrors[mode] != 1 || h.Count != 7 || h.BucketCounts != [4]uint64{2, 2, 2, 1} {
			t.Fatalf("измерения %s: %+v", mode, values)
		}
		// Дробные секунды представлены float64; 1e-9 с значительно меньше
		// миллисекундной точности входа и покрывает округление семи сложений.
		if math.Abs(h.SumSeconds-191.002) > 1e-9 {
			t.Fatalf("сумма секунд: %.12f", h.SumSeconds)
		}
	}
	repeated := sdkCollect(t, s)
	assertStableStarts(t, full, repeated)
	if !reflect.DeepEqual(values, oteltest.Read(t, s.recorder)) {
		t.Fatal("сбор повторно увеличил значения")
	}
	s.event(t, "019aabcd-0000-7000-9000-000000009000", domain.EventTypeLoginRejected, "", s.now)
	updated := sdkCollect(t, s)
	assertStableStarts(t, full, updated)
	next := oteltest.Read(t, s.recorder)
	if next.Completed[domain.OutcomeRejected] != 1 || next.Completed[domain.OutcomeSuccess] != 14 {
		t.Fatal("новое событие не увеличило нужный ряд")
	}
	other := newStack(t)
	independent := sdkCollect(t, other)
	firstID, _ := full.Resource.Set().Value("service.instance.id")
	secondID, _ := independent.Resource.Set().Value("service.instance.id")
	if firstID == secondID || !reflect.DeepEqual(zero, oteltest.Read(t, other.recorder)) {
		t.Fatal("экземпляры делят ID или накопление")
	}
	if !full.Resource.Equal(updated.Resource) {
		t.Fatal("Resource пересоздан при сборе")
	}
}

func TestRecorderLateHistogram(t *testing.T) {
	s := newStack(t)
	scenario := "019aabcd-0000-7000-9000-000000009001"
	s.event(t, scenario, domain.EventTypeLoginSucceeded, "", s.now)
	before := sdkCollect(t, s)
	if len(oteltest.Read(t, s.recorder).Durations) != 0 {
		t.Fatal("успех без связанных фактов создал Histogram")
	}
	s.event(t, scenario, domain.EventTypeLoginStarted, "", s.now.Add(-time.Second))
	s.event(t, scenario, domain.EventTypeLoginModeDetermined, domain.LoginModeCredentials, s.now)
	after := sdkCollect(t, s)
	assertStableStarts(t, before, after)
	values := oteltest.Read(t, s.recorder)
	if values.Completed[domain.OutcomeSuccess] != 1 || values.Durations[domain.LoginModeCredentials].Count != 1 {
		t.Fatal("поздний факт повторил успех или потерял наблюдение")
	}
	assertStableStarts(t, after, sdkCollect(t, s))
}

// Resource-env отклоняется до создания SDK, включая дополнительные ключи.
func TestRecorderSDKResourceEnvironment(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "test.attribute=synthetic,service.name=from-attributes")
	t.Setenv("OTEL_SERVICE_NAME", "from-service-env")
	r, err := otel.New(otel.ResourceSettings{NodeName: "node-test", Environment: "test"})
	if r != nil || err == nil || !strings.Contains(err.Error(), "OTEL_RESOURCE_ATTRIBUTES") ||
		strings.Contains(err.Error(), "synthetic") {
		t.Fatalf("Resource-env не отклонён безопасно: %v", err)
	}
}

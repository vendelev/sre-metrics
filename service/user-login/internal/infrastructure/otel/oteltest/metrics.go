// Package oteltest предоставляет чтение настоящего SDK для предметных тестов.
// Этот пакет используется только тестами, не хранит и не накапливает измерения.
package oteltest

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/vendelev/sre-metrics/service/user-login/internal/domain"
	"github.com/vendelev/sre-metrics/service/user-login/internal/infrastructure/otel"
)

// Values — проекция одного SDK-сбора без времён, удобная для предметных сравнений.
// Полный контракт метаданных проверяется отдельно по ResourceMetrics.
type Values struct {
	Completed       map[domain.Outcome]int64
	TechnicalErrors map[domain.LoginMode]int64
	Durations       map[domain.LoginMode]Duration
}

// Duration — проекция настоящей SDK Histogram, а не собственный накопитель.
type Duration struct {
	BucketCounts [4]uint64
	Count        uint64
	SumSeconds   float64
}

// New создаёт изолированный SDK и регистрирует его освобождение после теста.
func New(t testing.TB) *otel.Recorder {
	t.Helper()
	r, err := otel.New(otel.ResourceSettings{NodeName: "node-test", Environment: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return r
}

// Collect собирает дерево SDK без его преобразования и без сброса cumulative.
func Collect(t testing.TB, r *otel.Recorder) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Reader().Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	return rm
}

// Read возвращает только числовые данные одного сбора. Предметные операции должны
// завершиться до вызова, если тесту нужен полный результат нескольких инструментов.
func Read(t testing.TB, r *otel.Recorder) Values {
	t.Helper()
	result := Values{
		Completed:       make(map[domain.Outcome]int64),
		TechnicalErrors: make(map[domain.LoginMode]int64),
		Durations:       make(map[domain.LoginMode]Duration),
	}
	for _, scope := range Collect(t, r).ScopeMetrics {
		for _, m := range scope.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range data.DataPoints {
					switch m.Name {
					case otel.CompletedName:
						v, _ := p.Attributes.Value(attribute.Key("outcome"))
						result.Completed[domain.Outcome(v.AsString())] = p.Value
					case otel.TechnicalErrorsName:
						v, _ := p.Attributes.Value(attribute.Key("mode"))
						result.TechnicalErrors[domain.LoginMode(v.AsString())] = p.Value
					}
				}
			case metricdata.Histogram[float64]:
				for _, p := range data.DataPoints {
					v, _ := p.Attributes.Value(attribute.Key("mode"))
					if len(p.BucketCounts) != 4 {
						t.Fatalf("неожиданные корзины SDK: %v", p.BucketCounts)
					}
					result.Durations[domain.LoginMode(v.AsString())] = Duration{
						BucketCounts: [4]uint64(p.BucketCounts), Count: p.Count, SumSeconds: p.Sum,
					}
				}
			}
		}
	}
	return result
}

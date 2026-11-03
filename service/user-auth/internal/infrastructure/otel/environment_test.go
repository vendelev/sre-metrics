package otel

import (
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestConflictingEnvironmentVersion147(t *testing.T) {
	// Независимый явный список: изменение реализации не должно молча менять политику.
	want := strings.Fields(`
		OTEL_EXPORTER_OTLP_ENDPOINT OTEL_EXPORTER_OTLP_INSECURE OTEL_EXPORTER_OTLP_CERTIFICATE
		OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE OTEL_EXPORTER_OTLP_CLIENT_KEY OTEL_EXPORTER_OTLP_HEADERS
		OTEL_EXPORTER_OTLP_COMPRESSION OTEL_EXPORTER_OTLP_TIMEOUT OTEL_EXPORTER_OTLP_PROTOCOL
		OTEL_EXPORTER_OTLP_METRICS_ENDPOINT OTEL_EXPORTER_OTLP_METRICS_INSECURE OTEL_EXPORTER_OTLP_METRICS_CERTIFICATE
		OTEL_EXPORTER_OTLP_METRICS_CLIENT_CERTIFICATE OTEL_EXPORTER_OTLP_METRICS_CLIENT_KEY OTEL_EXPORTER_OTLP_METRICS_HEADERS
		OTEL_EXPORTER_OTLP_METRICS_COMPRESSION OTEL_EXPORTER_OTLP_METRICS_TIMEOUT OTEL_EXPORTER_OTLP_METRICS_PROTOCOL
		OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE OTEL_EXPORTER_OTLP_METRICS_DEFAULT_HISTOGRAM_AGGREGATION
		OTEL_METRIC_EXPORT_INTERVAL OTEL_METRIC_EXPORT_TIMEOUT OTEL_RESOURCE_ATTRIBUTES OTEL_SERVICE_NAME
		OTEL_METRICS_EXEMPLAR_FILTER OTEL_GO_X_CARDINALITY_LIMIT OTEL_GO_X_RESOURCE OTEL_GO_X_OBSERVABILITY
		OTEL_GO_X_SELF_OBSERVABILITY OTEL_GO_X_PER_SERIES_START_TIMESTAMPS
	`)
	got := ConflictingEnvironment()
	sort.Strings(want)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("перечень конфликтов изменился: %v", got)
	}
	for _, name := range want {
		t.Run(name, func(t *testing.T) {
			for _, value := range []string{"", "synthetic-secret"} {
				t.Setenv(name, value)
				r, err := New(ResourceSettings{NodeName: "node-test", Environment: "test"})
				if r != nil || err == nil || !strings.Contains(err.Error(), name) ||
					strings.Contains(err.Error(), "synthetic-secret") {
					t.Fatalf("обход политики через прямой конструктор: %v", err)
				}
				if current, ok := os.LookupEnv(name); !ok || current != value {
					t.Fatal("окружение изменено библиотекой")
				}
			}
		})
	}
}

package otel

import "fmt"

// ConflictingEnvironment возвращает перечень конфликтующих настроек OTel v1.47.0.
// PROTOCOL не читается выбранным exporter, но противоречит явному выбору HTTP.
// Источники: otlpmetrichttp/internal/oconf/envconfig.go, sdk/metric/config.go,
// sdk/metric/env.go и sdk/internal/x/features.go закреплённой версии.
func ConflictingEnvironment() []string {
	var names []string
	for _, prefix := range []string{"OTEL_EXPORTER_OTLP_", "OTEL_EXPORTER_OTLP_METRICS_"} {
		for _, suffix := range []string{
			"ENDPOINT", "INSECURE", "CERTIFICATE", "CLIENT_CERTIFICATE", "CLIENT_KEY",
			"HEADERS", "COMPRESSION", "TIMEOUT", "PROTOCOL",
		} {
			names = append(names, prefix+suffix)
		}
	}
	return append(names,
		"OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE",
		"OTEL_EXPORTER_OTLP_METRICS_DEFAULT_HISTOGRAM_AGGREGATION",
		"OTEL_METRIC_EXPORT_INTERVAL", "OTEL_METRIC_EXPORT_TIMEOUT",
		"OTEL_RESOURCE_ATTRIBUTES", "OTEL_SERVICE_NAME", "OTEL_METRICS_EXEMPLAR_FILTER",
		"OTEL_GO_X_CARDINALITY_LIMIT", "OTEL_GO_X_RESOURCE", "OTEL_GO_X_OBSERVABILITY",
		"OTEL_GO_X_SELF_OBSERVABILITY", "OTEL_GO_X_PER_SERIES_START_TIMESTAMPS",
	)
}

// ValidateEnvironment отклоняет даже пустые SDK-env, не раскрывая их значения.
// Окружение процесса не изменяется; его конкурентное изменение не поддерживается.
func ValidateEnvironment(lookup func(string) (string, bool)) error {
	for _, name := range ConflictingEnvironment() {
		if _, ok := lookup(name); ok {
			return fmt.Errorf("%s: конфликт с явной конфигурацией сервиса", name)
		}
	}
	return nil
}

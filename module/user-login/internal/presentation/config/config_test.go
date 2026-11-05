package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/memory"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/otel"
	"github.com/vendelev/sre-metrics/user-login/internal/presentation/config"
	"github.com/vendelev/sre-metrics/user-login/internal/presentation/httpapi"
)

func source(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		if !ok {
			value, ok = required()[name]
		}
		return value, ok
	}
}

func required() map[string]string {
	return map[string]string{
		"USER_AUTH_NODE_NAME": "node-test", "USER_AUTH_ENVIRONMENT": "test",
		"USER_AUTH_OTLP_METRICS_ENDPOINT": "http://localhost:4318/v1/metrics",
	}
}

func validDefault() config.Config {
	c := config.Default()
	c.NodeName, c.Environment, c.OTLPMetricsEndpoint = "node-test", "test", "http://localhost:4318/v1/metrics"
	return c
}

func TestLoadDefaults(t *testing.T) {
	t.Parallel()
	want := config.Config{
		ListenAddr: ":8080", MaxBodyBytes: 4096,
		MaxScenarios: 10_000, MaxEventsPerScenario: 32,
		MaintenanceInterval: time.Second, HTTPReadHeaderTimeout: time.Second,
		HTTPReadTimeout: 2 * time.Second, HTTPWriteTimeout: 2 * time.Second,
		HTTPIdleTimeout: 30 * time.Second, ShutdownTimeout: 10 * time.Second,
		NodeName: "node-test", Environment: "test", OTLPMetricsEndpoint: "http://localhost:4318/v1/metrics",
		OTLPExportInterval: 10 * time.Second, OTLPExportTimeout: 2 * time.Second,
	}
	got, err := config.Load(source(nil))
	if err != nil || got != want {
		t.Fatalf("умолчания: получено %+v, ожидалось %+v, ошибка %v", got, want, err)
	}
	limits := memory.DefaultLimits()
	if want.MaxBodyBytes != httpapi.DefaultSettings().MaxBodyBytes ||
		want.MaxScenarios != limits.MaxScenarios || want.MaxEventsPerScenario != limits.MaxEventsPerScenario {
		t.Fatal("начальные лимиты конфигурации расходятся с существующими компонентами")
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Parallel()
	values := map[string]string{
		"USER_AUTH_LISTEN_ADDR": "127.0.0.1:0", "USER_AUTH_MAX_BODY_BYTES": "8192",
		"USER_AUTH_MAX_SCENARIOS": "20000", "USER_AUTH_MAX_EVENTS_PER_SCENARIO": "64",
		"USER_AUTH_MAINTENANCE_INTERVAL": "500ms", "USER_AUTH_HTTP_READ_HEADER_TIMEOUT": "750ms",
		"USER_AUTH_HTTP_READ_TIMEOUT": "3s", "USER_AUTH_HTTP_WRITE_TIMEOUT": "4s",
		"USER_AUTH_HTTP_IDLE_TIMEOUT": "1m", "USER_AUTH_SHUTDOWN_TIMEOUT": "15s",
		"USER_AUTH_OTLP_EXPORT_INTERVAL": "20s", "USER_AUTH_OTLP_EXPORT_TIMEOUT": "1s",
		"USER_AUTH_NODE_NAME":   "node-2",
		"USER_AUTH_ENVIRONMENT": "development", "USER_AUTH_OTLP_METRICS_ENDPOINT": "https://receiver/v1/metrics",
	}
	want := config.Config{
		ListenAddr: "127.0.0.1:0", MaxBodyBytes: 8192,
		MaxScenarios: 20_000, MaxEventsPerScenario: 64,
		MaintenanceInterval: 500 * time.Millisecond, HTTPReadHeaderTimeout: 750 * time.Millisecond,
		HTTPReadTimeout: 3 * time.Second, HTTPWriteTimeout: 4 * time.Second,
		HTTPIdleTimeout: time.Minute, ShutdownTimeout: 15 * time.Second,
		NodeName: "node-2", Environment: "development", OTLPMetricsEndpoint: "https://receiver/v1/metrics",
		OTLPExportInterval: 20 * time.Second, OTLPExportTimeout: time.Second,
	}
	got, err := config.Load(source(values))
	if err != nil || got != want {
		t.Fatalf("переопределения: получено %+v, ожидалось %+v, ошибка %v", got, want, err)
	}
	defaults, err := config.Load(source(nil))
	if err != nil || defaults != validDefault() {
		t.Fatalf("переопределения повлияли на следующую загрузку: %+v, %v", defaults, err)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, env, value string
	}{
		{"пустое значение", "USER_AUTH_MAX_BODY_BYTES", ""},
		{"формат целого", "USER_AUTH_MAX_SCENARIOS", "много"},
		{"нулевой лимит", "USER_AUTH_MAX_BODY_BYTES", "0"},
		{"отрицательный лимит", "USER_AUTH_MAX_EVENTS_PER_SCENARIO", "-1"},
		{"формат длительности", "USER_AUTH_HTTP_READ_TIMEOUT", "2"},
		{"нулевой timeout", "USER_AUTH_HTTP_IDLE_TIMEOUT", "0s"},
		{"отрицательная длительность", "USER_AUTH_MAINTENANCE_INTERVAL", "-1s"},
		{"адрес без порта", "USER_AUTH_LISTEN_ADDR", "localhost"},
		{"порт вне диапазона", "USER_AUTH_LISTEN_ADDR", "localhost:65536"},
		{"слишком редкое обслуживание", "USER_AUTH_MAINTENANCE_INTERVAL", "61s"},
		{"заголовки дольше запроса", "USER_AUTH_HTTP_READ_HEADER_TIMEOUT", "3s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := config.Load(source(map[string]string{tc.env: tc.value}))
			if err == nil || !strings.Contains(err.Error(), tc.env) {
				t.Fatalf("%s=%q: ожидалась ошибка с именем переменной, получено %v", tc.env, tc.value, err)
			}
			if got != (config.Config{}) {
				t.Fatal("при ошибке возвращена частичная конфигурация")
			}
		})
	}
}

func TestValidateBoundaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*config.Config)
	}{
		{"граница интервала", func(c *config.Config) { c.MaintenanceInterval = time.Minute }},
		{"граница заголовков", func(c *config.Config) { c.HTTPReadHeaderTimeout = c.HTTPReadTimeout }},
		{"бюджет остановки меньше HTTP-тайм-аутов", func(c *config.Config) {
			c.ShutdownTimeout = time.Millisecond
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := validDefault()
			tc.change(&cfg)
			if err := cfg.Validate(); err != nil {
				t.Fatalf("допустимая граница отклонена: %v", err)
			}
		})
	}
}

func TestOTLPConfiguration(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"USER_AUTH_NODE_NAME", "USER_AUTH_ENVIRONMENT", "USER_AUTH_OTLP_METRICS_ENDPOINT"} {
		t.Run(name, func(t *testing.T) {
			for _, missing := range []bool{true, false} {
				values := required()
				if missing {
					delete(values, name)
				} else {
					values[name] = ""
				}
				_, err := config.Load(func(key string) (string, bool) { v, ok := values[key]; return v, ok })
				if err == nil || !strings.Contains(err.Error(), name) {
					t.Fatalf("обязательное значение: %v", err)
				}
			}
		})
	}
	for _, name := range []string{"USER_AUTH_OTLP_EXPORT_INTERVAL", "USER_AUTH_OTLP_EXPORT_TIMEOUT"} {
		for _, value := range []string{"", "0", "0s", "-1s", "2", "secret-value"} {
			t.Run(name+"/"+value, func(t *testing.T) {
				_, err := config.Load(source(map[string]string{name: value}))
				if err == nil || !strings.Contains(err.Error(), name) || strings.Contains(err.Error(), "secret-value") {
					t.Fatalf("длительность: %v", err)
				}
			})
		}
	}
	for _, value := range []string{"node name", "\tnode", "node\n", "node\x00", "node\u00a0"} {
		for _, name := range []string{"USER_AUTH_NODE_NAME", "USER_AUTH_ENVIRONMENT"} {
			_, err := config.Load(source(map[string]string{name: value}))
			if err == nil {
				t.Fatalf("принято пробельное/управляющее значение %s", name)
			}
		}
	}
	for _, name := range otel.ConflictingEnvironment() {
		t.Run(name, func(t *testing.T) {
			for _, value := range []string{"", "synthetic-secret"} {
				_, err := config.Load(source(map[string]string{name: value}))
				if err == nil || !strings.Contains(err.Error(), name) ||
					strings.Contains(err.Error(), "synthetic-secret") {
					t.Fatalf("конфликт: %v", err)
				}
			}
		})
	}
	for _, name := range []string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_LOGS_HEADERS", "OTEL_UNKNOWN"} {
		if _, err := config.Load(source(map[string]string{name: "ignored"})); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEndpoint(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		"http://receiver/v1/metrics", "https://receiver:65535/v1/metrics", "http://127.0.0.1:1/v1/metrics", "http://[::1]:4318/v1/metrics",
	} {
		if _, err := config.Load(source(map[string]string{"USER_AUTH_OTLP_METRICS_ENDPOINT": value})); err != nil {
			t.Fatalf("URL %s: %v", value, err)
		}
	}
	for _, value := range []string{
		"http://receiver", "ftp://receiver/v1/metrics", "http:///v1/metrics", "http://receiver:0/v1/metrics",
		"http://receiver:65536/v1/metrics", "http://receiver:/v1/metrics", "http://receiver:abc/v1/metrics",
		"http://receiver/v1/metrics/", "http://receiver/v1/%6detrics", "http://receiver/v1%2fmetrics",
		"http://receiver/v1/../v1/metrics", "http://secret@receiver/v1/metrics", "http://receiver/v1/metrics?secret",
		"http://receiver/v1/metrics?", "http://receiver/v1/metrics#", "http://receiver/v1/metrics#secret",
		"http://bad_host/v1/metrics", "http://-bad/v1/metrics", "http://999.1.1.1/v1/metrics", "http://[bad]/v1/metrics",
	} {
		t.Run(value, func(t *testing.T) {
			_, err := config.Load(source(map[string]string{"USER_AUTH_OTLP_METRICS_ENDPOINT": value}))
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("небезопасный/некорректный URL принят: %v", err)
			}
		})
	}
}

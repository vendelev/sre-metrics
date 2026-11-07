package config_test

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/memory"
	"github.com/vendelev/sre-metrics/user-login/internal/presentation/config"
)

func setupEnvironment(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "USER_LOGIN_") || strings.HasPrefix(name, "OTEL_") {
			unsetEnvironment(t, name)
		}
	}
}

func unsetEnvironment(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
}

func validDefault() config.Config {
	return config.Default()
}

func TestLoadDefaults(t *testing.T) {
	setupEnvironment(t)
	want := config.Config{
		ListenAddr:   ":8080",
		MaxScenarios: 10_000, MaxEventsPerScenario: 32,
		MaintenanceInterval: time.Second, HTTPReadHeaderTimeout: time.Second,
		HTTPReadTimeout: 2 * time.Second, HTTPWriteTimeout: 2 * time.Second,
		HTTPIdleTimeout: 30 * time.Second, ShutdownTimeout: 10 * time.Second,
	}
	got, err := config.Load()
	if err != nil || got != want {
		t.Fatalf("умолчания: получено %+v, ожидалось %+v, ошибка %v", got, want, err)
	}
	limits := memory.DefaultLimits()
	if want.MaxScenarios != limits.MaxScenarios || want.MaxEventsPerScenario != limits.MaxEventsPerScenario {
		t.Fatal("начальные лимиты конфигурации расходятся с существующими компонентами")
	}
}

func TestLoadOverrides(t *testing.T) {
	setupEnvironment(t)
	values := map[string]string{
		"USER_LOGIN_LISTEN_ADDR":   "127.0.0.1:0",
		"USER_LOGIN_MAX_SCENARIOS": "20000", "USER_LOGIN_MAX_EVENTS_PER_SCENARIO": "64",
		"USER_LOGIN_MAINTENANCE_INTERVAL": "500ms", "USER_LOGIN_HTTP_READ_HEADER_TIMEOUT": "750ms",
		"USER_LOGIN_HTTP_READ_TIMEOUT": "3s", "USER_LOGIN_HTTP_WRITE_TIMEOUT": "4s",
		"USER_LOGIN_HTTP_IDLE_TIMEOUT": "1m", "USER_LOGIN_SHUTDOWN_TIMEOUT": "15s",
	}
	want := config.Config{
		ListenAddr:   "127.0.0.1:0",
		MaxScenarios: 20_000, MaxEventsPerScenario: 64,
		MaintenanceInterval: 500 * time.Millisecond, HTTPReadHeaderTimeout: 750 * time.Millisecond,
		HTTPReadTimeout: 3 * time.Second, HTTPWriteTimeout: 4 * time.Second,
		HTTPIdleTimeout: time.Minute, ShutdownTimeout: 15 * time.Second,
	}
	t.Run("переопределения", func(t *testing.T) {
		for name, value := range values {
			t.Setenv(name, value)
		}
		got, err := config.Load()
		if err != nil || got != want {
			t.Fatalf("переопределения: получено %+v, ожидалось %+v, ошибка %v", got, want, err)
		}
	})
	defaults, err := config.Load()
	if err != nil || defaults != validDefault() {
		t.Fatalf("переопределения повлияли на следующую загрузку: %+v, %v", defaults, err)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	setupEnvironment(t)
	for _, tc := range []struct {
		name, env, value string
	}{
		{"формат целого", "USER_LOGIN_MAX_SCENARIOS", "много"},
		{"значение не раскрывается", "USER_LOGIN_MAX_SCENARIOS", "synthetic-secret"},
		{"нулевой лимит", "USER_LOGIN_MAX_SCENARIOS", "0"},
		{"отрицательный лимит", "USER_LOGIN_MAX_EVENTS_PER_SCENARIO", "-1"},
		{"формат длительности", "USER_LOGIN_HTTP_READ_TIMEOUT", "2"},
		{"нулевой timeout", "USER_LOGIN_HTTP_IDLE_TIMEOUT", "0s"},
		{"отрицательная длительность", "USER_LOGIN_MAINTENANCE_INTERVAL", "-1s"},
		{"слишком редкое обслуживание", "USER_LOGIN_MAINTENANCE_INTERVAL", "61s"},
		{"заголовки дольше запроса", "USER_LOGIN_HTTP_READ_HEADER_TIMEOUT", "3s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.env, tc.value)
			got, err := config.Load()
			if err == nil || !strings.Contains(err.Error(), tc.env) {
				t.Fatalf("%s=%q: ожидалась ошибка с именем переменной, получено %v", tc.env, tc.value, err)
			}
			if got != (config.Config{}) {
				t.Fatal("при ошибке возвращена частичная конфигурация")
			}
			if strings.Contains(err.Error(), "synthetic-secret") {
				t.Fatal("ошибка раскрыла значение переменной")
			}
		})
	}
}

func TestLoadEmptyValuesUseDefaults(t *testing.T) {
	setupEnvironment(t)
	for _, name := range []string{
		"USER_LOGIN_LISTEN_ADDR", "USER_LOGIN_MAX_SCENARIOS", "USER_LOGIN_MAX_EVENTS_PER_SCENARIO",
		"USER_LOGIN_MAINTENANCE_INTERVAL", "USER_LOGIN_HTTP_READ_HEADER_TIMEOUT", "USER_LOGIN_HTTP_READ_TIMEOUT",
		"USER_LOGIN_HTTP_WRITE_TIMEOUT", "USER_LOGIN_HTTP_IDLE_TIMEOUT", "USER_LOGIN_SHUTDOWN_TIMEOUT",
	} {
		t.Setenv(name, "")
	}
	got, err := config.Load()
	if err != nil || got != validDefault() {
		t.Fatalf("пустые значения должны сохранять умолчания: %+v, %v", got, err)
	}
}

func TestLoadReportsAllParseErrors(t *testing.T) {
	setupEnvironment(t)
	t.Setenv("USER_LOGIN_MAX_SCENARIOS", "synthetic-secret-int")
	t.Setenv("USER_LOGIN_HTTP_READ_TIMEOUT", "synthetic-secret-duration")
	got, err := config.Load()
	if err == nil || got != (config.Config{}) {
		t.Fatalf("ожидался отказ загрузки: %+v, %v", got, err)
	}
	for _, name := range []string{"USER_LOGIN_MAX_SCENARIOS", "USER_LOGIN_HTTP_READ_TIMEOUT"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("ошибка не содержит %s: %v", name, err)
		}
	}
	if strings.Contains(err.Error(), "synthetic-secret") {
		t.Fatal("список ошибок раскрыл значения переменных")
	}
}

func TestValidatePositiveSettingsNames(t *testing.T) {
	t.Parallel()
	for _, field := range reflect.VisibleFields(reflect.TypeFor[config.Config]()) {
		if field.Type.Kind() != reflect.Int && field.Type.Kind() != reflect.Int64 {
			continue
		}
		t.Run(field.Name, func(t *testing.T) {
			t.Parallel()
			cfg := validDefault()
			reflect.ValueOf(&cfg).Elem().FieldByIndex(field.Index).SetInt(0)
			want := field.Tag.Get("env") + ": значение должно быть положительным"
			if err := cfg.Validate(); err == nil || err.Error() != want {
				t.Fatalf("ожидалась ошибка %q, получено %v", want, err)
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

func TestLoadAcceptsSDKEnvironment(t *testing.T) {
	setupEnvironment(t)
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "test.source=env,host.name=otel-node,deployment.environment.name=test")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_COMPRESSION", "gzip")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "https://receiver/v1/metrics")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TIMEOUT", "100")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "20000")
	t.Setenv("OTEL_METRIC_EXPORT_TIMEOUT", "3000")
	got, err := config.Load()
	if err != nil || got != validDefault() {
		t.Fatalf("SDK-env не должен отклоняться или изменять поля Config: %+v, %v", got, err)
	}
}

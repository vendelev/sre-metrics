package config_test

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vendelev/sre-metrics/core/config"
)

func setupEnvironment(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "CORE_HTTP_") || strings.HasPrefix(name, "USER_LOGIN_") ||
			strings.HasPrefix(name, "OTEL_") {
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

func validDefault() config.HTTP {
	return config.Default()
}

func TestLoadDefaults(t *testing.T) {
	setupEnvironment(t)
	want := config.HTTP{
		Addr:              ":8080",
		ReadHeaderTimeout: time.Second,
		ReadTimeout:       2 * time.Second,
		WriteTimeout:      2 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	got, err := config.Load()
	if err != nil || got != want {
		t.Fatalf("умолчания: получено %+v, ожидалось %+v, ошибка %v", got, want, err)
	}
}

// Настройки ядра и модулей независимы: модульные переменные не влияют на Load core.
func TestLoadIgnoresModuleEnvironment(t *testing.T) {
	setupEnvironment(t)
	t.Setenv("USER_LOGIN_LISTEN_ADDR", "127.0.0.1:9999")
	t.Setenv("USER_LOGIN_HTTP_READ_TIMEOUT", "5s")
	got, err := config.Load()
	if err != nil || got != validDefault() {
		t.Fatalf("модульные переменные повлияли на настройки ядра: %+v, %v", got, err)
	}
}

func TestLoadOverrides(t *testing.T) {
	setupEnvironment(t)
	values := map[string]string{
		"CORE_HTTP_LISTEN_ADDR":         "127.0.0.1:0",
		"CORE_HTTP_READ_HEADER_TIMEOUT": "750ms",
		"CORE_HTTP_READ_TIMEOUT":        "3s",
		"CORE_HTTP_WRITE_TIMEOUT":       "4s",
		"CORE_HTTP_IDLE_TIMEOUT":        "1m",
	}
	want := config.HTTP{
		Addr:              "127.0.0.1:0",
		ReadHeaderTimeout: 750 * time.Millisecond,
		ReadTimeout:       3 * time.Second,
		WriteTimeout:      4 * time.Second,
		IdleTimeout:       time.Minute,
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
		{"формат длительности", "CORE_HTTP_READ_TIMEOUT", "2"},
		{"нулевой timeout", "CORE_HTTP_IDLE_TIMEOUT", "0s"},
		{"отрицательная длительность", "CORE_HTTP_WRITE_TIMEOUT", "-1s"},
		{"значение не раскрывается", "CORE_HTTP_READ_TIMEOUT", "synthetic-secret"},
		{"заголовки дольше запроса", "CORE_HTTP_READ_HEADER_TIMEOUT", "3s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.env, tc.value)
			got, err := config.Load()
			if err == nil || !strings.Contains(err.Error(), tc.env) {
				t.Fatalf("%s=%q: ожидалась ошибка с именем переменной, получено %v", tc.env, tc.value, err)
			}
			if got != (config.HTTP{}) {
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
		"CORE_HTTP_LISTEN_ADDR", "CORE_HTTP_READ_HEADER_TIMEOUT", "CORE_HTTP_READ_TIMEOUT",
		"CORE_HTTP_WRITE_TIMEOUT", "CORE_HTTP_IDLE_TIMEOUT",
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
	t.Setenv("CORE_HTTP_READ_TIMEOUT", "synthetic-secret-duration")
	t.Setenv("CORE_HTTP_WRITE_TIMEOUT", "synthetic-secret-duration")
	got, err := config.Load()
	if err == nil || got != (config.HTTP{}) {
		t.Fatalf("ожидался отказ загрузки: %+v, %v", got, err)
	}
	for _, name := range []string{"CORE_HTTP_READ_TIMEOUT", "CORE_HTTP_WRITE_TIMEOUT"} {
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
	for _, field := range reflect.VisibleFields(reflect.TypeFor[config.HTTP]()) {
		if field.Type.Kind() != reflect.Int64 {
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
	cfg := validDefault()
	cfg.ReadHeaderTimeout = cfg.ReadTimeout
	if err := cfg.Validate(); err != nil {
		t.Fatalf("допустимая граница отклонена: %v", err)
	}
}

func TestLoadAcceptsSDKEnvironment(t *testing.T) {
	setupEnvironment(t)
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_COMPRESSION", "gzip")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "https://receiver/v1/metrics")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "20000")
	got, err := config.Load()
	if err != nil || got != validDefault() {
		t.Fatalf("SDK-env не должен отклоняться или изменять поля HTTP: %+v, %v", got, err)
	}
}

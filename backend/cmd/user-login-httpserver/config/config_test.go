package config_test

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vendelev/sre-metrics/cmd/user-login-httpserver/config"
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

func TestLoadDefaults(t *testing.T) {
	setupEnvironment(t)
	want := config.Config{ShutdownTimeout: 10 * time.Second}
	got, err := config.Load()
	if err != nil || got != want {
		t.Fatalf("умолчания: получено %+v, ожидалось %+v, ошибка %v", got, want, err)
	}
}

// Настройки процесса и функционального модуля независимы: переменные модуля
// не влияют на Load процесса.
func TestLoadIgnoresModuleEnvironment(t *testing.T) {
	setupEnvironment(t)
	t.Setenv("USER_LOGIN_EVENT_QUEUE_SIZE", "17")
	t.Setenv("USER_LOGIN_MAINTENANCE_INTERVAL", "500ms")
	got, err := config.Load()
	if err != nil || got != config.Default() {
		t.Fatalf("переменные модуля повлияли на настройки процесса: %+v, %v", got, err)
	}
}

func TestLoadOverride(t *testing.T) {
	setupEnvironment(t)
	t.Run("переопределение", func(t *testing.T) {
		t.Setenv("USER_LOGIN_SHUTDOWN_TIMEOUT", "15s")
		want := config.Config{ShutdownTimeout: 15 * time.Second}
		got, err := config.Load()
		if err != nil || got != want {
			t.Fatalf("переопределение: получено %+v, ожидалось %+v, ошибка %v", got, want, err)
		}
	})
	defaults, err := config.Load()
	if err != nil || defaults != config.Default() {
		t.Fatalf("переопределение повлияло на следующую загрузку: %+v, %v", defaults, err)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	setupEnvironment(t)
	for _, tc := range []struct {
		name, value string
	}{
		{"формат длительности", "2"},
		{"отрицательная длительность", "-1s"},
		{"нулевой бюджет", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("USER_LOGIN_SHUTDOWN_TIMEOUT", tc.value)
			got, err := config.Load()
			if err == nil || !strings.Contains(err.Error(), "USER_LOGIN_SHUTDOWN_TIMEOUT") {
				t.Fatalf("%q: ожидалась ошибка с именем переменной, получено %v", tc.value, err)
			}
			if got != (config.Config{}) {
				t.Fatal("при ошибке возвращена частичная конфигурация")
			}
		})
	}
}

func TestLoadEmptyValuesUseDefaults(t *testing.T) {
	setupEnvironment(t)
	t.Setenv("USER_LOGIN_SHUTDOWN_TIMEOUT", "")
	got, err := config.Load()
	if err != nil || got != config.Default() {
		t.Fatalf("пустые значения должны сохранять умолчания: %+v, %v", got, err)
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
			cfg := config.Default()
			reflect.ValueOf(&cfg).Elem().FieldByIndex(field.Index).SetInt(0)
			want := field.Tag.Get("env") + ": значение должно быть положительным"
			if err := cfg.Validate(); err == nil || err.Error() != want {
				t.Fatalf("ожидалась ошибка %q, получено %v", want, err)
			}
		})
	}
}

// Перекрёстная формула бюджета остановки относительно HTTP-тайм-аутов не
// проверяется: остановка короче HTTP-тайм-аутов допустима и завершается
// принудительным закрытием соединений.
func TestValidateAllowsShortBudget(t *testing.T) {
	t.Parallel()
	cfg := config.Config{ShutdownTimeout: time.Millisecond}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("короткий бюджет отклонён: %v", err)
	}
}

package config_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vendelev/sre-metrics/user-login/config"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/memory"
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
		Processing: config.Processing{
			EventQueueSize:       1024,
			MaxScenarios:         10_000,
			MaxEventsPerScenario: 32,
			MaintenanceInterval:  time.Second,
		},
		Inbox: config.Inbox{
			Dir:             "/mnt/metrics/user-login",
			ScanInterval:    time.Second,
			MaxFilesPerScan: 256,
			MaxFileBytes:    16 * 1024,
			MaxAttempts:     3,
		},
	}
	got, err := config.Load()
	if err != nil || got != want {
		t.Fatalf("умолчания: получено %+v, ожидалось %+v, ошибка %v", got, want, err)
	}
	limits := memory.DefaultLimits()
	if want.Processing.MaxScenarios != limits.MaxScenarios ||
		want.Processing.MaxEventsPerScenario != limits.MaxEventsPerScenario {
		t.Fatal("начальные лимиты конфигурации расходятся с существующими компонентами")
	}
}

// Настройки модуля и ядра независимы: переменные core не влияют на Load модуля.
func TestLoadIgnoresCoreEnvironment(t *testing.T) {
	setupEnvironment(t)
	t.Setenv("CORE_HTTP_LISTEN_ADDR", "127.0.0.1:9999")
	t.Setenv("CORE_HTTP_READ_TIMEOUT", "5s")
	got, err := config.Load()
	if err != nil || got != validDefault() {
		t.Fatalf("переменные ядра повлияли на настройки модуля: %+v, %v", got, err)
	}
}

func TestLoadOverrides(t *testing.T) {
	setupEnvironment(t)
	values := map[string]string{
		"USER_LOGIN_EVENT_QUEUE_SIZE":         "17",
		"USER_LOGIN_MAX_SCENARIOS":            "20000",
		"USER_LOGIN_MAX_EVENTS_PER_SCENARIO":  "64",
		"USER_LOGIN_MAINTENANCE_INTERVAL":     "500ms",
		"USER_LOGIN_INBOX_DIR":                "/var/tmp/user-login-inbox",
		"USER_LOGIN_INBOX_SCAN_INTERVAL":      "250ms",
		"USER_LOGIN_INBOX_MAX_FILES_PER_SCAN": "7",
		"USER_LOGIN_INBOX_MAX_FILE_BYTES":     "2147483648",
		"USER_LOGIN_INBOX_MAX_ATTEMPTS":       "5",
	}
	want := config.Config{
		Processing: config.Processing{
			EventQueueSize:       17,
			MaxScenarios:         20_000,
			MaxEventsPerScenario: 64,
			MaintenanceInterval:  500 * time.Millisecond,
		},
		Inbox: config.Inbox{
			Dir:             "/var/tmp/user-login-inbox",
			ScanInterval:    250 * time.Millisecond,
			MaxFilesPerScan: 7,
			MaxFileBytes:    1 << 31,
			MaxAttempts:     5,
		},
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
		{"формат очереди", "USER_LOGIN_EVENT_QUEUE_SIZE", "много"},
		{"нулевая очередь", "USER_LOGIN_EVENT_QUEUE_SIZE", "0"},
		{"отрицательная очередь", "USER_LOGIN_EVENT_QUEUE_SIZE", "-1"},
		{"отрицательный лимит", "USER_LOGIN_MAX_EVENTS_PER_SCENARIO", "-1"},
		{"формат длительности", "USER_LOGIN_MAINTENANCE_INTERVAL", "2"},
		{"отрицательная длительность", "USER_LOGIN_MAINTENANCE_INTERVAL", "-1s"},
		{"слишком редкое обслуживание", "USER_LOGIN_MAINTENANCE_INTERVAL", "61s"},
		{"пробельный путь inbox", "USER_LOGIN_INBOX_DIR", " \t "},
		{"интервал inbox без единиц", "USER_LOGIN_INBOX_SCAN_INTERVAL", "2"},
		{"отрицательный интервал inbox", "USER_LOGIN_INBOX_SCAN_INTERVAL", "-1s"},
		{"отрицательный лимит файлов", "USER_LOGIN_INBOX_MAX_FILES_PER_SCAN", "-1"},
		{"отрицательный размер файла", "USER_LOGIN_INBOX_MAX_FILE_BYTES", "-1"},
		{"размер файла вне int64", "USER_LOGIN_INBOX_MAX_FILE_BYTES", "9223372036854775808"},
		{"отрицательный лимит попыток", "USER_LOGIN_INBOX_MAX_ATTEMPTS", "-1"},
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
		"USER_LOGIN_MAX_SCENARIOS", "USER_LOGIN_MAX_EVENTS_PER_SCENARIO",
		"USER_LOGIN_EVENT_QUEUE_SIZE", "USER_LOGIN_MAINTENANCE_INTERVAL",
		"USER_LOGIN_INBOX_DIR", "USER_LOGIN_INBOX_SCAN_INTERVAL", "USER_LOGIN_INBOX_MAX_FILES_PER_SCAN",
		"USER_LOGIN_INBOX_MAX_FILE_BYTES", "USER_LOGIN_INBOX_MAX_ATTEMPTS",
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
	t.Setenv("USER_LOGIN_MAINTENANCE_INTERVAL", "synthetic-secret-duration")
	t.Setenv("USER_LOGIN_INBOX_SCAN_INTERVAL", "synthetic-secret-inbox")
	got, err := config.Load()
	if err == nil || got != (config.Config{}) {
		t.Fatalf("ожидался отказ загрузки: %+v, %v", got, err)
	}
	for _, name := range []string{
		"USER_LOGIN_MAX_SCENARIOS",
		"USER_LOGIN_MAINTENANCE_INTERVAL",
		"USER_LOGIN_INBOX_SCAN_INTERVAL",
	} {
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
	for _, tc := range []struct {
		env    string
		change func(*config.Config)
	}{
		{"USER_LOGIN_EVENT_QUEUE_SIZE", func(c *config.Config) { c.Processing.EventQueueSize = 0 }},
		{"USER_LOGIN_MAX_SCENARIOS", func(c *config.Config) { c.Processing.MaxScenarios = 0 }},
		{"USER_LOGIN_MAX_EVENTS_PER_SCENARIO", func(c *config.Config) { c.Processing.MaxEventsPerScenario = 0 }},
		{"USER_LOGIN_MAINTENANCE_INTERVAL", func(c *config.Config) { c.Processing.MaintenanceInterval = 0 }},
		{"USER_LOGIN_INBOX_SCAN_INTERVAL", func(c *config.Config) { c.Inbox.ScanInterval = 0 }},
		{"USER_LOGIN_INBOX_MAX_FILES_PER_SCAN", func(c *config.Config) { c.Inbox.MaxFilesPerScan = 0 }},
		{"USER_LOGIN_INBOX_MAX_FILE_BYTES", func(c *config.Config) { c.Inbox.MaxFileBytes = 0 }},
		{"USER_LOGIN_INBOX_MAX_ATTEMPTS", func(c *config.Config) { c.Inbox.MaxAttempts = 0 }},
	} {
		t.Run(tc.env, func(t *testing.T) {
			t.Parallel()
			cfg := validDefault()
			tc.change(&cfg)
			want := tc.env + ": значение должно быть положительным"
			if err := cfg.Validate(); err == nil || err.Error() != want {
				t.Fatalf("ожидалась ошибка %q, получено %v", want, err)
			}
		})
	}
}

func TestValidateRejectsEmptyInboxDir(t *testing.T) {
	t.Parallel()
	cfg := validDefault()
	cfg.Inbox.Dir = ""
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "USER_LOGIN_INBOX_DIR") {
		t.Fatalf("ожидалась ошибка пустого пути, получено %v", err)
	}
}

func TestValidateBoundaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*config.Config)
	}{
		{"граница интервала", func(c *config.Config) { c.Processing.MaintenanceInterval = time.Minute }},
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

func TestValidateReportsNonPositiveSettingBeforeUpperBound(t *testing.T) {
	t.Parallel()
	cfg := validDefault()
	cfg.Processing.MaintenanceInterval = time.Minute + time.Second
	cfg.Inbox.ScanInterval = 0
	want := "USER_LOGIN_INBOX_SCAN_INTERVAL: значение должно быть положительным"
	if err := cfg.Validate(); err == nil || err.Error() != want {
		t.Fatalf("ожидалась ошибка %q, получено %v", want, err)
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

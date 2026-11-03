package config

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/vendelev/sre-metrics/service/user-auth/internal/infrastructure/otel"
)

// Config содержит проверяемые настройки одного запуска сервиса.
// После сборки компонентов настройки не изменяются. SDK-env дополнительно
// проверяется при создании адаптера, чтобы прямой конструктор не обходил политику.
type Config struct {
	ListenAddr            string
	MaxBodyBytes          int64
	MaxScenarios          int
	MaxEventsPerScenario  int
	MaintenanceInterval   time.Duration
	HTTPReadHeaderTimeout time.Duration
	HTTPReadTimeout       time.Duration
	HTTPWriteTimeout      time.Duration
	HTTPIdleTimeout       time.Duration
	ShutdownTimeout       time.Duration
	OTLPMetricsEndpoint   string
	OTLPExportInterval    time.Duration
	OTLPExportTimeout     time.Duration
	NodeName              string
	Environment           string
}

// Default возвращает начальные лимиты контракта и согласованные настройки запуска.
func Default() Config {
	return Config{
		ListenAddr: ":8080", MaxBodyBytes: 4096,
		MaxScenarios: 10_000, MaxEventsPerScenario: 32,
		MaintenanceInterval: time.Second, HTTPReadHeaderTimeout: time.Second,
		HTTPReadTimeout: 2 * time.Second, HTTPWriteTimeout: 2 * time.Second,
		HTTPIdleTimeout: 30 * time.Second, ShutdownTimeout: 10 * time.Second,
		OTLPExportInterval: 10 * time.Second, OTLPExportTimeout: 2 * time.Second,
	}
}

// Load читает каждую известную переменную один раз через lookup, например os.LookupEnv.
// Необязательные значения имеют умолчания; обязательные должны присутствовать.
// Явно заданное пустое значение является ошибкой.
func Load(lookup func(string) (string, bool)) (Config, error) {
	if lookup == nil {
		return Config{}, fmt.Errorf("загрузка конфигурации: отсутствует источник env")
	}
	cfg := Default()
	if err := otel.ValidateEnvironment(lookup); err != nil {
		return Config{}, err
	}
	for _, setting := range []struct {
		name   string
		target *string
	}{
		{"USER_AUTH_OTLP_METRICS_ENDPOINT", &cfg.OTLPMetricsEndpoint},
		{"USER_AUTH_NODE_NAME", &cfg.NodeName}, {"USER_AUTH_ENVIRONMENT", &cfg.Environment},
	} {
		if value, ok := lookup(setting.name); ok {
			*setting.target = value
		}
	}
	if value, ok := lookup("USER_AUTH_LISTEN_ADDR"); ok {
		cfg.ListenAddr = value
	}
	if value, ok := lookup("USER_AUTH_MAX_BODY_BYTES"); ok {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return Config{}, fmt.Errorf("USER_AUTH_MAX_BODY_BYTES: требуется целое десятичное число в диапазоне int64")
		}
		cfg.MaxBodyBytes = parsed
	}
	for _, setting := range []struct {
		name   string
		target *int
	}{
		{"USER_AUTH_MAX_SCENARIOS", &cfg.MaxScenarios},
		{"USER_AUTH_MAX_EVENTS_PER_SCENARIO", &cfg.MaxEventsPerScenario},
	} {
		if value, ok := lookup(setting.name); ok {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return Config{}, fmt.Errorf("%s: требуется целое десятичное число в диапазоне int", setting.name)
			}
			*setting.target = parsed
		}
	}
	for _, setting := range []struct {
		name   string
		target *time.Duration
	}{
		{"USER_AUTH_MAINTENANCE_INTERVAL", &cfg.MaintenanceInterval},
		{"USER_AUTH_HTTP_READ_HEADER_TIMEOUT", &cfg.HTTPReadHeaderTimeout},
		{"USER_AUTH_HTTP_READ_TIMEOUT", &cfg.HTTPReadTimeout},
		{"USER_AUTH_HTTP_WRITE_TIMEOUT", &cfg.HTTPWriteTimeout},
		{"USER_AUTH_HTTP_IDLE_TIMEOUT", &cfg.HTTPIdleTimeout},
		{"USER_AUTH_SHUTDOWN_TIMEOUT", &cfg.ShutdownTimeout},
		{"USER_AUTH_OTLP_EXPORT_INTERVAL", &cfg.OTLPExportInterval},
		{"USER_AUTH_OTLP_EXPORT_TIMEOUT", &cfg.OTLPExportTimeout},
	} {
		if value, ok := lookup(setting.name); ok {
			parsed, err := time.ParseDuration(value)
			if err != nil {
				return Config{}, fmt.Errorf(
					"%s: требуется длительность с единицей времени, например 500ms или 2s",
					setting.name,
				)
			}
			*setting.target = parsed
		}
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate проверяет также настройки, созданные непосредственно в Go, без чтения env.
func (c Config) Validate() error {
	if err := (otel.ResourceSettings{NodeName: c.NodeName, Environment: c.Environment}).Validate(); err != nil {
		return err
	}
	if err := otel.ValidateEndpoint(c.OTLPMetricsEndpoint); err != nil {
		return err
	}
	if err := validateAddress(c.ListenAddr); err != nil {
		return err
	}
	for _, setting := range []struct {
		name  string
		value int64
	}{
		{"USER_AUTH_MAX_BODY_BYTES", c.MaxBodyBytes},
		{"USER_AUTH_MAX_SCENARIOS", int64(c.MaxScenarios)},
		{"USER_AUTH_MAX_EVENTS_PER_SCENARIO", int64(c.MaxEventsPerScenario)},
		{"USER_AUTH_MAINTENANCE_INTERVAL", int64(c.MaintenanceInterval)},
		{"USER_AUTH_HTTP_READ_HEADER_TIMEOUT", int64(c.HTTPReadHeaderTimeout)},
		{"USER_AUTH_HTTP_READ_TIMEOUT", int64(c.HTTPReadTimeout)},
		{"USER_AUTH_HTTP_WRITE_TIMEOUT", int64(c.HTTPWriteTimeout)},
		{"USER_AUTH_HTTP_IDLE_TIMEOUT", int64(c.HTTPIdleTimeout)},
		{"USER_AUTH_SHUTDOWN_TIMEOUT", int64(c.ShutdownTimeout)},
		{"USER_AUTH_OTLP_EXPORT_INTERVAL", int64(c.OTLPExportInterval)},
		{"USER_AUTH_OTLP_EXPORT_TIMEOUT", int64(c.OTLPExportTimeout)},
	} {
		if setting.value <= 0 {
			return fmt.Errorf("%s: значение должно быть положительным", setting.name)
		}
	}
	if c.MaintenanceInterval > time.Minute {
		return fmt.Errorf("USER_AUTH_MAINTENANCE_INTERVAL: интервал не должен превышать 1m")
	}
	if c.HTTPReadHeaderTimeout > c.HTTPReadTimeout {
		return fmt.Errorf(
			"USER_AUTH_HTTP_READ_HEADER_TIMEOUT: значение не должно превышать USER_AUTH_HTTP_READ_TIMEOUT",
		)
	}
	// Перекрёстная формула бюджета остановки относительно HTTP-тайм-аутов не проверяется:
	// HTTP-тайм-ауты ограничивают сетевые операции, а не произвольный обработчик;
	// остановка короче HTTP-тайм-аутов допустима и завершается принудительным закрытием.
	return nil
}

func validateAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil || strings.ContainsAny(host, " /\\\t\r\n[]") {
		return fmt.Errorf("USER_AUTH_LISTEN_ADDR: требуется адрес host:port, для IPv6 — [адрес]:port")
	}
	if strings.Contains(host, ":") {
		if _, err := netip.ParseAddr(host); err != nil {
			return fmt.Errorf("USER_AUTH_LISTEN_ADDR: некорректный IPv6-адрес")
		}
	}
	if port == "" || strings.IndexFunc(port, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return fmt.Errorf("USER_AUTH_LISTEN_ADDR: порт должен быть числом от 0 до 65535")
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("USER_AUTH_LISTEN_ADDR: порт должен быть числом от 0 до 65535")
	}
	return nil
}

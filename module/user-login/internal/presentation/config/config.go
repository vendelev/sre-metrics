package config

import (
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/caarlos0/env/v11"
)

// Config содержит проверяемые настройки одного запуска сервиса.
// После сборки компонентов настройки не изменяются.
type Config struct {
	ListenAddr            string        `env:"USER_LOGIN_LISTEN_ADDR"`
	MaxScenarios          int           `env:"USER_LOGIN_MAX_SCENARIOS"`
	MaxEventsPerScenario  int           `env:"USER_LOGIN_MAX_EVENTS_PER_SCENARIO"`
	MaintenanceInterval   time.Duration `env:"USER_LOGIN_MAINTENANCE_INTERVAL"`
	HTTPReadHeaderTimeout time.Duration `env:"USER_LOGIN_HTTP_READ_HEADER_TIMEOUT"`
	HTTPReadTimeout       time.Duration `env:"USER_LOGIN_HTTP_READ_TIMEOUT"`
	HTTPWriteTimeout      time.Duration `env:"USER_LOGIN_HTTP_WRITE_TIMEOUT"`
	HTTPIdleTimeout       time.Duration `env:"USER_LOGIN_HTTP_IDLE_TIMEOUT"`
	ShutdownTimeout       time.Duration `env:"USER_LOGIN_SHUTDOWN_TIMEOUT"`
}

// Default возвращает начальные лимиты контракта и согласованные настройки запуска.
func Default() Config {
	return Config{
		ListenAddr:            ":8080",
		MaxScenarios:          10_000,
		MaxEventsPerScenario:  32,
		MaintenanceInterval:   time.Second,
		HTTPReadHeaderTimeout: time.Second,
		HTTPReadTimeout:       2 * time.Second,
		HTTPWriteTimeout:      2 * time.Second,
		HTTPIdleTimeout:       30 * time.Second,
		ShutdownTimeout:       10 * time.Second,
	}
}

// Load читает настройки сервиса из окружения. Стандартные OTEL_* читает сам SDK.
func Load() (Config, error) {
	cfg := Default()
	if err := env.Parse(&cfg); err != nil {
		return Config{}, fmt.Errorf("загрузка конфигурации: %w", parseErrors(err))
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// parseErrors сохраняет список проблемных настроек без исходных значений из ошибок env.
func parseErrors(err error) error {
	items := []error{err}
	if aggregate, ok := errors.AsType[env.AggregateError](err); ok {
		items = aggregate.Errors
	}
	var result []error
	for _, item := range items {
		if parseErr, ok := errors.AsType[env.ParseError](item); ok {
			result = append(result, fmt.Errorf("%s: некорректный формат значения типа %s",
				envName(parseErr.Name), parseErr.Type))
		} else {
			result = append(result, fmt.Errorf("некорректное описание настройки env"))
		}
	}
	return errors.Join(result...)
}

// Validate проверяет также настройки, созданные непосредственно в Go, без чтения env.
func (c *Config) Validate() error {
	for _, setting := range []struct {
		name  string
		value int64
	}{
		{"MaxScenarios", int64(c.MaxScenarios)},
		{"MaxEventsPerScenario", int64(c.MaxEventsPerScenario)},
		{"MaintenanceInterval", int64(c.MaintenanceInterval)},
		{"HTTPReadHeaderTimeout", int64(c.HTTPReadHeaderTimeout)},
		{"HTTPReadTimeout", int64(c.HTTPReadTimeout)},
		{"HTTPWriteTimeout", int64(c.HTTPWriteTimeout)},
		{"HTTPIdleTimeout", int64(c.HTTPIdleTimeout)},
		{"ShutdownTimeout", int64(c.ShutdownTimeout)},
	} {
		if setting.value <= 0 {
			return fmt.Errorf("%s: значение должно быть положительным", envName(setting.name))
		}
	}

	if c.MaintenanceInterval > time.Minute {
		return fmt.Errorf("%s: интервал не должен превышать 1m", envName("MaintenanceInterval"))
	}

	if c.HTTPReadHeaderTimeout > c.HTTPReadTimeout {
		return fmt.Errorf(
			"%s: значение не должно превышать %s",
			envName("HTTPReadHeaderTimeout"),
			envName("HTTPReadTimeout"),
		)
	}

	return nil
}

// envName берёт имя настройки из тега; при отсутствии поля или тега сохраняет Go-имя для диагностики.
func envName(name string) string {
	field, ok := reflect.TypeFor[Config]().FieldByName(name)
	if !ok {
		return name
	}
	if value := field.Tag.Get("env"); value != "" {
		return value
	}
	return name
}

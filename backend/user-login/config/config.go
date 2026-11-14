package config

import (
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/caarlos0/env/v11"
)

// Config содержит проверяемые настройки одного запуска модуля.
// После сборки компонентов настройки не изменяются.
// Общий бюджет остановки процесса относится к приложению и загружается
// в bootstrap процесса.
// HTTP-адрес и тайм-ауты net/http принадлежат ядру core (CORE_HTTP_*).
type Config struct {
	EventQueueSize       int           `env:"USER_LOGIN_EVENT_QUEUE_SIZE"`
	MaxScenarios         int           `env:"USER_LOGIN_MAX_SCENARIOS"`
	MaxEventsPerScenario int           `env:"USER_LOGIN_MAX_EVENTS_PER_SCENARIO"`
	MaintenanceInterval  time.Duration `env:"USER_LOGIN_MAINTENANCE_INTERVAL"`
}

// Default возвращает начальные лимиты контракта и согласованные настройки запуска.
func Default() Config {
	return Config{
		EventQueueSize:       1024,
		MaxScenarios:         10_000,
		MaxEventsPerScenario: 32,
		MaintenanceInterval:  time.Second,
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
		{"EventQueueSize", int64(c.EventQueueSize)},
		{"MaxScenarios", int64(c.MaxScenarios)},
		{"MaxEventsPerScenario", int64(c.MaxEventsPerScenario)},
		{"MaintenanceInterval", int64(c.MaintenanceInterval)},
	} {
		if setting.value <= 0 {
			return fmt.Errorf("%s: значение должно быть положительным", envName(setting.name))
		}
	}

	if c.MaintenanceInterval > time.Minute {
		return fmt.Errorf("%s: интервал не должен превышать 1m", envName("MaintenanceInterval"))
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

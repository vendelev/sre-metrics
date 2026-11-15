// Package config содержит настройки процесса user-login-httpserver,
// которые не принадлежат функциональным модулям: общий бюджет остановки.
// Имя переменной USER_LOGIN_SHUTDOWN_TIMEOUT — временный внешний контракт;
// переименование согласуется отдельно.
package config

import (
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/caarlos0/env/v11"
)

// Config содержит проверяемые настройки одного запуска процесса.
// После сборки приложения настройки не изменяются.
type Config struct {
	ShutdownTimeout time.Duration `env:"USER_LOGIN_SHUTDOWN_TIMEOUT"`
}

// Default возвращает согласованные настройки запуска процесса.
func Default() Config {
	return Config{
		ShutdownTimeout: 10 * time.Second,
	}
}

// Load читает настройки процесса из окружения.
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
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("%s: значение должно быть положительным", envName("ShutdownTimeout"))
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

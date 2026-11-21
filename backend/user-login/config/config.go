package config

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/caarlos0/env/v11"
)

// Config содержит проверяемые настройки одного запуска модуля.
// После сборки компонентов настройки не изменяются.
// Общий бюджет остановки процесса относится к приложению и загружается
// в bootstrap процесса.
// HTTP-адрес и тайм-ауты net/http принадлежат ядру core (CORE_HTTP_*).
type Config struct {
	Processing Processing
	Inbox      Inbox
}

// Default возвращает начальные лимиты контракта и согласованные настройки запуска.
func Default() Config {
	return Config{
		Processing: defaultProcessing(),
		Inbox:      defaultInbox(),
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
	// Сначала проверяем положительность во всех группах, затем частные ограничения.
	if err := c.Processing.validatePositive(); err != nil {
		return err
	}
	if err := c.Inbox.validatePositive(); err != nil {
		return err
	}
	if err := c.Processing.validateMaintenanceInterval(); err != nil {
		return err
	}
	return c.Inbox.validateDir()
}

type numericSetting struct {
	name  string
	value int64
}

func validatePositive(settings ...numericSetting) error {
	for _, setting := range settings {
		if setting.value <= 0 {
			return fmt.Errorf("%s: значение должно быть положительным", envName(setting.name))
		}
	}
	return nil
}

// envName берёт имя настройки из тега. Ошибка env содержит Go-имя поля без имени группы.
func envName(name string) string {
	for _, group := range [...]reflect.Type{reflect.TypeFor[Processing](), reflect.TypeFor[Inbox]()} {
		if field, ok := group.FieldByName(name); ok {
			if value := field.Tag.Get("env"); value != "" {
				return value
			}
		}
	}
	return name
}

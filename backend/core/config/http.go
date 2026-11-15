package config

import (
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/caarlos0/env/v11"
)

// HTTP содержит параметры net/http Server. Настройки читаются из окружения
// ядра один раз при старте; формат адреса проверяет httpserver при открытии
// listener. Значения уже проверены Validate — компоненты доверяют им.
type HTTP struct {
	Addr              string        `env:"CORE_HTTP_LISTEN_ADDR"`
	ReadHeaderTimeout time.Duration `env:"CORE_HTTP_READ_HEADER_TIMEOUT"`
	ReadTimeout       time.Duration `env:"CORE_HTTP_READ_TIMEOUT"`
	WriteTimeout      time.Duration `env:"CORE_HTTP_WRITE_TIMEOUT"`
	IdleTimeout       time.Duration `env:"CORE_HTTP_IDLE_TIMEOUT"`
}

// Default возвращает стартовые настройки ядра: listener :8080 и тайм-ауты
// net/http по умолчанию контракта.
func Default() HTTP {
	return HTTP{
		Addr:              ":8080",
		ReadHeaderTimeout: time.Second,
		ReadTimeout:       2 * time.Second,
		WriteTimeout:      2 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
}

// Load читает настройки ядра из окружения. Стандартные OTEL_* читает сам SDK.
func Load() (HTTP, error) {
	cfg := Default()
	if err := env.Parse(&cfg); err != nil {
		return HTTP{}, fmt.Errorf("загрузка конфигурации core: %w", parseErrors(err))
	}
	if err := cfg.Validate(); err != nil {
		return HTTP{}, err
	}
	return cfg, nil
}

// Validate проверяет также настройки, созданные непосредственно в Go, без чтения env.
func (c *HTTP) Validate() error {
	for _, setting := range []struct {
		name  string
		value int64
	}{
		{"ReadHeaderTimeout", int64(c.ReadHeaderTimeout)},
		{"ReadTimeout", int64(c.ReadTimeout)},
		{"WriteTimeout", int64(c.WriteTimeout)},
		{"IdleTimeout", int64(c.IdleTimeout)},
	} {
		if setting.value <= 0 {
			return fmt.Errorf("%s: значение должно быть положительным", envName(setting.name))
		}
	}

	if c.ReadHeaderTimeout > c.ReadTimeout {
		return fmt.Errorf(
			"%s: значение не должно превышать %s",
			envName("ReadHeaderTimeout"),
			envName("ReadTimeout"),
		)
	}

	return nil
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

// envName берёт имя настройки из тега; при отсутствии поля или тега сохраняет Go-имя для диагностики.
func envName(name string) string {
	field, ok := reflect.TypeFor[HTTP]().FieldByName(name)
	if !ok {
		return name
	}
	if value := field.Tag.Get("env"); value != "" {
		return value
	}
	return name
}

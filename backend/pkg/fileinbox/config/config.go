// Package config задаёт настройки файлового входа.
package config

import (
	"errors"
	"strings"
	"time"
)

// Config задаёт каталог и границы обработки файлового входа.
type Config struct {
	// Dir — непустой путь единственного каталога. Разрешения нового каталога ограничивает umask.
	Dir string
	// ScanInterval задаёт период сканирования независимо от уведомлений.
	ScanInterval time.Duration
	// MaxFilesPerScan ограничивает число файлов, выбранных для обработки на один проход.
	MaxFilesPerScan int
	// MaxFileBytes задаёт максимальный допустимый размер файла включительно.
	MaxFileBytes int64
	// MaxAttempts ограничивает ошибки чтения и отдельно попытки удаления одной публикации.
	MaxAttempts int
}

// Validate проверяет настройки перед сборкой файлового входа.
func (c Config) Validate() error {
	switch {
	case strings.TrimSpace(c.Dir) == "":
		return errors.New("не задан каталог файлового входа")
	case c.ScanInterval <= 0:
		return errors.New("интервал сканирования должен быть положительным")
	case c.MaxFilesPerScan <= 0:
		return errors.New("лимит файлов за сканирование должен быть положительным")
	case c.MaxFileBytes <= 0:
		return errors.New("лимит размера файла должен быть положительным")
	case c.MaxAttempts <= 0:
		return errors.New("лимит попыток должен быть положительным")
	default:
		return nil
	}
}

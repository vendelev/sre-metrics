// Package contract определяет внутренние зависимости обработки файлового входа.
package contract

import (
	"errors"

	dPublication "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/domain/publication"
)

var (
	// ErrPublicationChanged означает исчезновение или замену выбранной публикации.
	ErrPublicationChanged = errors.New("публикация файла изменилась")
	// ErrFileTooLarge обозначает превышение лимита размера публикации.
	ErrFileTooLarge = errors.New("размер публикации превышает лимит")
)

// FileSystem изолирует операции каталога от правил обработки публикаций.
type FileSystem interface {
	// Entries возвращает имена записей текущего каталога.
	Entries() ([]string, error)
	// Prepare создаёт и проверяет доступность каталога.
	Prepare() error
	// VolumeUsage возвращает долю занятых блоков и признак доступности замера.
	VolumeUsage() (float64, bool, error)
	// Inspect возвращает метаданные публикации, не переходя по ссылке.
	Inspect(name string) (dPublication.Metadata, error)
	// Read читает ранее выбранную публикацию с ограничением размера.
	Read(file dPublication.Metadata, maxBytes int64) ([]byte, error)
	// Unlink перед удалением проверяет, что публикация не изменилась.
	Unlink(file dPublication.Metadata) error
}

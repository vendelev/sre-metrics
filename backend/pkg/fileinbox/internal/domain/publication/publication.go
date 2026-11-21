// Package publication содержит правила идентификации файловых публикаций.
package publication

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// Suffix — расширение опубликованного файла.
const Suffix = ".json"

// Identity отличает повторное использование имени и inode после исчезновения записи.
type Identity struct {
	Device   uint64
	Inode    uint64
	Size     int64
	Modified time.Time
}

// Metadata содержит только свойства публикации, необходимые правилам обработки.
type Metadata struct {
	Name      string
	Identity  Identity
	ValidName bool
	IsRegular bool
}

// NewMetadata проверяет формат имени и создаёт независимое описание файла.
func NewMetadata(name string, identity Identity, isRegular bool) Metadata {
	return Metadata{Name: name, Identity: identity, ValidName: canonicalName(name), IsRegular: isRegular}
}

// Same определяет, относится ли текущая запись к ранее выбранной публикации.
func Same(a, b Identity) bool {
	return a.Device == b.Device && a.Inode == b.Inode && a.Size == b.Size && a.Modified.Equal(b.Modified)
}

func canonicalName(name string) bool {
	stem, ok := strings.CutSuffix(name, Suffix)
	if !ok {
		return false
	}
	id, err := uuid.Parse(stem)
	return err == nil && id.String() == stem
}

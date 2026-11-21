// Package filesystem реализует операции с каталогом и обычными файлами публикаций.
package filesystem

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	dContract "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/domain/contract"
	dPublication "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/domain/publication"
)

// FileSystem выполняет операции в одном каталоге файлового входа.
type FileSystem struct {
	dir string
}

var _ dContract.FileSystem = (*FileSystem)(nil)

// New создаёт файловый адаптер для указанного каталога.
func New(dir string) *FileSystem {
	return &FileSystem{dir: dir}
}

// Prepare создаёт каталог и проверяет права, необходимые для всего цикла обработки.
func (f *FileSystem) Prepare() error {
	if err := os.MkdirAll(f.dir, 0o777); err != nil {
		return fmt.Errorf("создание каталога файлового входа: %w", err)
	}
	// Для листинга, доступа к записям и удаления нужны права r/w/x каталога.
	const directoryAccess = 0o7
	if err := syscall.Access(f.dir, directoryAccess); err != nil {
		return fmt.Errorf("проверка доступа к каталогу файлового входа: %w", err)
	}
	return nil
}

// Entries читает имена записей каталога.
func (f *FileSystem) Entries() ([]string, error) {
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}
	return names, nil
}

// Inspect получает метаданные записи без перехода по символической ссылке.
func (f *FileSystem) Inspect(name string) (dPublication.Metadata, error) {
	info, err := os.Lstat(filepath.Join(f.dir, name))
	if err != nil {
		return dPublication.Metadata{}, err
	}
	return dPublication.NewMetadata(name, identityOf(info), info.Mode().IsRegular()), nil
}

func identityOf(info fs.FileInfo) dPublication.Identity {
	identity := dPublication.Identity{Size: info.Size(), Modified: info.ModTime()}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		identity.Device = stat.Dev
		identity.Inode = stat.Ino
	}
	return identity
}

package filesystem

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	dContract "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/domain/contract"
	dPublication "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/domain/publication"
)

// Unlink проверяет идентичность записи перед удалением.
func (f *FileSystem) Unlink(file dPublication.Metadata) error {
	path := filepath.Join(f.dir, file.Name)
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) ||
		(err == nil && !dPublication.Same(file.Identity, identityOf(current))) {
		return dContract.ErrPublicationChanged
	}
	if err != nil {
		return err
	}
	// В отличие от os.Remove, unlink не удалит каталог при подмене после Lstat.
	err = syscall.Unlink(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

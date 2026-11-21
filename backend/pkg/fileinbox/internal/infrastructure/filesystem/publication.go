package filesystem

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	dContract "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/domain/contract"
	dPublication "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/domain/publication"
)

// Read читает только прежнюю публикацию, отклоняя подмену ссылкой или другим типом файла.
func (f *FileSystem) Read(file dPublication.Metadata, maxBytes int64) ([]byte, error) {
	if file.Identity.Size > maxBytes {
		return nil, dContract.ErrFileTooLarge
	}
	path := filepath.Join(f.dir, file.Name)
	// Не переходим по подставленной ссылке и не блокируем открытие подставленного FIFO.
	opened, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ELOOP) {
		return nil, dContract.ErrPublicationChanged
	}
	if err != nil {
		return nil, err
	}
	defer func() {
		// Дескриптор открыт только для чтения и не содержит данных для сброса.
		_ = opened.Close()
	}()
	current, err := opened.Stat()
	if err != nil {
		return nil, err
	}
	if !current.Mode().IsRegular() || !dPublication.Same(file.Identity, identityOf(current)) {
		return nil, dContract.ErrPublicationChanged
	}
	return readLimited(opened, maxBytes)
}

func readLimited(file io.Reader, limit int64) ([]byte, error) {
	reader := &io.LimitedReader{R: file, N: limit}
	contents, err := io.ReadAll(reader)
	if err != nil || reader.N > 0 {
		return contents, err
	}
	// Проверяем ещё байт отдельно, не переполняя limit + 1.
	var extra [1]byte
	_, err = io.ReadFull(file, extra[:])
	switch {
	case err == nil:
		return nil, dContract.ErrFileTooLarge
	case errors.Is(err, io.EOF):
		return contents, nil
	default:
		return nil, err
	}
}

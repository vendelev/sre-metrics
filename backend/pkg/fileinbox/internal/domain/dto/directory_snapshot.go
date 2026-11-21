// Package dto содержит данные наблюдения за файловым входом.
package dto

import (
	"math"
	"time"

	dPublication "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/domain/publication"
)

// DirectorySnapshot содержит данные о публикациях до обработки пачки.
type DirectorySnapshot struct {
	Files      int64
	Bytes      int64
	OldestTime time.Time
}

// Include добавляет метаданные одной публикации в снимок каталога.
func (s *DirectorySnapshot) Include(file dPublication.Metadata) {
	modified := file.Identity.Modified
	if s.Files == 0 || modified.Before(s.OldestTime) {
		s.OldestTime = modified
	}
	s.Files++
	// Логический размер разреженных файлов может превышать ёмкость ФС.
	s.Bytes += min(file.Identity.Size, math.MaxInt64-s.Bytes)
}

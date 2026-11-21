package filesystem

import "syscall"

// VolumeUsage измеряет долю занятых блоков всей файловой системы каталога.
func (f *FileSystem) VolumeUsage() (float64, bool, error) {
	// Statfs видит общий tmpfs, включая промежуточные файлы и соседние каталоги.
	var stat syscall.Statfs_t
	if err := syscall.Statfs(f.dir, &stat); err != nil {
		return 0, false, err
	}
	if stat.Blocks == 0 {
		return 0, false, nil
	}
	used := stat.Blocks - min(stat.Blocks, stat.Bfree)
	return float64(used) / float64(stat.Blocks), true, nil
}

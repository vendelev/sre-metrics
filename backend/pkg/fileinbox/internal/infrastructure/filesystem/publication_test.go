//go:build linux

package filesystem_test

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	dContract "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/domain/contract"
	iFilesystem "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/infrastructure/filesystem"
)

const fileA = "00000000-0000-4000-8000-00000000000a.json"

func TestReadRejectsReplacementsSinceListing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		replace func(*testing.T, string)
	}{
		{name: "другой обычный файл", replace: func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, fileA), "новая публикация")
		}},
		{name: "ссылка на исходный inode", replace: func(t *testing.T, dir string) {
			path := filepath.Join(dir, fileA)
			moved := filepath.Join(dir, "moved")
			if err := os.Rename(path, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "именованный канал", replace: func(t *testing.T, dir string) {
			tmp := filepath.Join(dir, "replacement")
			if err := syscall.Mkfifo(tmp, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(tmp, filepath.Join(dir, fileA)); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, fileA), "старая публикация")
			files := iFilesystem.New(dir)
			metadata, err := files.Inspect(fileA)
			if err != nil {
				t.Fatal(err)
			}
			tt.replace(t, dir)
			result := make(chan error, 1)
			go func() {
				_, readErr := files.Read(metadata, 1024)
				result <- readErr
			}()
			select {
			case err := <-result:
				if !errors.Is(err, dContract.ErrPublicationChanged) {
					t.Fatalf("подмена при открытии не обнаружена: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("чтение подменённой публикации заблокировалось")
			}
		})
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

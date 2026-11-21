//go:build linux

package usecase_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	pkgClock "github.com/vendelev/sre-metrics/pkg/clock"
	pkgApi "github.com/vendelev/sre-metrics/pkg/fileinbox/api"
	aUsecase "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/application/usecase"
	iFilesystem "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/infrastructure/filesystem"
)

const (
	fileA = "00000000-0000-4000-8000-00000000000a.json"
	fileB = "00000000-0000-4000-8000-00000000000b.json"
	fileC = "00000000-0000-4000-8000-00000000000c.json"
)

func TestScanFiltersEntriesAndRejectsWithoutLeakingInput(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t.TempDir())
	cfg.MaxFileBytes = 8
	var logs bytes.Buffer
	var handled []string
	s := newTestScanner(t, cfg, func(contents []byte, _ time.Time) error {
		value := string(contents)
		handled = append(handled, value)
		if value == "PII-body" {
			return errors.New("синтетическая ошибка PII-error: " + value)
		}
		return nil
	}, &logs, pkgClock.RealTime{})

	rejected := map[string]string{
		fileA:                    "PII-body",
		fileB:                    "oversized",
		"synthetic-private.json": "PII-name",
		"00000000-0000-4000-8000-00000000000A.json": "uppercase",
		"0000000000004000800000000000000a.json":     "short",
	}
	for name, contents := range rejected {
		publish(t, cfg.Dir, name, contents)
	}
	publish(t, cfg.Dir, fileC, "8-bytes!")

	link := "00000000-0000-4000-8000-00000000000d.json"
	directory := "00000000-0000-4000-8000-00000000000e.json"
	fifo := "00000000-0000-4000-8000-00000000000f.json"
	skipped := []string{fileA + ".tmp", "notes.txt", "notes.JSON", link, directory, fifo}
	writeFile(t, filepath.Join(cfg.Dir, skipped[0]), "tmp")
	writeFile(t, filepath.Join(cfg.Dir, skipped[1]), "text")
	writeFile(t, filepath.Join(cfg.Dir, skipped[2]), "extension")
	target := filepath.Join(t.TempDir(), "target")
	writeFile(t, target, "private")
	if err := os.Symlink(target, filepath.Join(cfg.Dir, link)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(cfg.Dir, directory), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(cfg.Dir, fifo), 0o600); err != nil {
		t.Fatal(err)
	}

	scanDir(t, s)
	for name := range rejected {
		requireMissing(t, filepath.Join(cfg.Dir, name))
	}
	requireMissing(t, filepath.Join(cfg.Dir, fileC))
	requirePresent(t, target)
	for _, name := range skipped {
		requirePresent(t, filepath.Join(cfg.Dir, name))
	}
	slices.Sort(handled)
	if !slices.Equal(handled, []string{"8-bytes!", "PII-body"}) {
		t.Errorf("неожиданные данные в обработчике: %v", handled)
	}
	for _, value := range []string{"invalid_content", "invalid_name", "file_too_large", fileA, `"size":8`} {
		if !strings.Contains(logs.String(), value) {
			t.Errorf("ожидалась диагностика %q: %s", value, &logs)
		}
	}
	for _, value := range []string{"PII-body", "PII-error", "synthetic-private", "0000000000004000800000000000000a"} {
		if strings.Contains(logs.String(), value) {
			t.Errorf("чувствительные входные данные %q попали в журнал: %s", value, &logs)
		}
	}
}

func TestScanOrdersByModificationTimeAndNameAndLimitsWork(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t.TempDir())
	cfg.MaxFilesPerScan = 2
	now := time.Unix(100, 0)
	var handled []string
	s := newTestScanner(t, cfg, func(contents []byte, _ time.Time) error {
		handled = append(handled, string(contents))
		return nil
	}, io.Discard, pkgClock.RealTime{})
	publish(t, cfg.Dir, fileB, "b")
	publish(t, cfg.Dir, fileA, "a")
	publish(t, cfg.Dir, fileC, "c")
	setMtime(t, filepath.Join(cfg.Dir, fileA), now)
	setMtime(t, filepath.Join(cfg.Dir, fileB), now)
	setMtime(t, filepath.Join(cfg.Dir, fileC), now.Add(-time.Second))

	scanDir(t, s)
	requirePresent(t, filepath.Join(cfg.Dir, fileB))
	scanDir(t, s)
	if !slices.Equal(handled, []string{"c", "a", "b"}) {
		t.Errorf("нарушен порядок обработки: %v", handled)
	}
	requireMissing(t, filepath.Join(cfg.Dir, fileB))
}

func TestScanRetriesWithCurrentClockBeyondAttemptBudget(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t.TempDir())
	cfg.MaxAttempts = 1
	start := time.Unix(100, 0)
	clk := &testClock{now: start}
	var times []time.Time
	s := newTestScanner(t, cfg, func(_ []byte, now time.Time) error {
		times = append(times, now)
		if now.Before(start.Add(2 * time.Second)) {
			return fmt.Errorf("отложено: %w", pkgApi.ErrRetry)
		}
		return nil
	}, io.Discard, clk)
	publish(t, cfg.Dir, fileA, "публикация")
	path := filepath.Join(cfg.Dir, fileA)
	for range 2 {
		scanDir(t, s)
		requirePresent(t, path)
		clk.now = clk.now.Add(time.Second)
	}
	scanDir(t, s)
	requireMissing(t, path)
	want := []time.Time{start, start.Add(time.Second), start.Add(2 * time.Second)}
	if !slices.EqualFunc(times, want, time.Time.Equal) {
		t.Errorf("неожиданные значения часов: %v; требовались %v", times, want)
	}
}

func TestScanExhaustsReadAttemptsThenRemovesFile(t *testing.T) {
	t.Parallel()
	requireUnprivileged(t)
	cfg := testConfig(t.TempDir())
	path := filepath.Join(cfg.Dir, fileA)
	publish(t, cfg.Dir, fileA, "публикация")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	s := newTestScanner(t, cfg, func([]byte, time.Time) error {
		t.Error("нечитаемый файл передан обработчику")
		return nil
	}, &logs, pkgClock.RealTime{})
	scanDir(t, s)
	requirePresent(t, path)
	scanDir(t, s)
	requireMissing(t, path)
	if count := strings.Count(logs.String(), `"reason":"read_failed"`); count != cfg.MaxAttempts {
		t.Errorf("число зарегистрированных ошибок чтения: %d", count)
	}
}

func TestScanRetriesOnlyUnlinkAfterFinalRejection(t *testing.T) {
	t.Parallel()
	requireUnprivileged(t)
	cfg := testConfig(t.TempDir())
	t.Cleanup(func() { setMode(t, cfg.Dir, 0o700) })
	publish(t, cfg.Dir, fileA, "битая публикация")
	calls := 0
	s := newTestScanner(t, cfg, func([]byte, time.Time) error {
		calls++
		setMode(t, cfg.Dir, 0o500)
		return errors.New("синтетический отказ")
	}, io.Discard, pkgClock.RealTime{})
	scanDir(t, s)
	path := filepath.Join(cfg.Dir, fileA)
	requirePresent(t, path)
	setMode(t, cfg.Dir, 0o700)
	scanDir(t, s)
	requireMissing(t, path)
	if calls != 1 {
		t.Errorf("отклонённая публикация передана обработчику %d раз", calls)
	}
}

func TestScanExhaustsUnlinkAttemptsWithoutBlockingOtherPublications(t *testing.T) {
	t.Parallel()
	requireUnprivileged(t)
	cfg := testConfig(t.TempDir())
	cfg.MaxFilesPerScan = 1
	t.Cleanup(func() { setMode(t, cfg.Dir, 0o700) })
	path := filepath.Join(cfg.Dir, fileA)
	publish(t, cfg.Dir, fileA, "первая")
	publish(t, cfg.Dir, fileB, "следующая")
	setMtime(t, path, time.Unix(100, 0))
	var handled []string
	var logs bytes.Buffer
	s := newTestScanner(t, cfg, func(contents []byte, _ time.Time) error {
		value := string(contents)
		handled = append(handled, value)
		if value == "первая" {
			setMode(t, cfg.Dir, 0o500)
		}
		return nil
	}, &logs, pkgClock.RealTime{})
	scanDir(t, s)
	scanDir(t, s)
	setMode(t, cfg.Dir, 0o700)
	scanDir(t, s)
	scanDir(t, s)
	requirePresent(t, path)
	requireMissing(t, filepath.Join(cfg.Dir, fileB))
	publish(t, cfg.Dir, fileA, "новая под тем же именем")
	scanDir(t, s)
	requireMissing(t, path)
	if want := []string{"первая", "следующая", "новая под тем же именем"}; !slices.Equal(handled, want) {
		t.Errorf("неожиданные вызовы обработчика: %v", handled)
	}
	if count := strings.Count(logs.String(), `"reason":"unlink_failed"`); count != cfg.MaxAttempts {
		t.Errorf("число зарегистрированных ошибок удаления: %d", count)
	}
}

func TestScanForgetsVanishedPublication(t *testing.T) {
	t.Parallel()
	requireUnprivileged(t)
	cfg := testConfig(t.TempDir())
	cfg.MaxAttempts = 1
	t.Cleanup(func() { setMode(t, cfg.Dir, 0o700) })
	publish(t, cfg.Dir, fileA, "публикация")
	s := newTestScanner(t, cfg, func([]byte, time.Time) error {
		setMode(t, cfg.Dir, 0o500)
		return nil
	}, io.Discard, pkgClock.RealTime{})
	scanDir(t, s)
	setMode(t, cfg.Dir, 0o700)
	if err := os.Remove(filepath.Join(cfg.Dir, fileA)); err != nil {
		t.Fatal(err)
	}
	scanDir(t, s)
	setMode(t, cfg.Dir, 0o500)
	scanDir(t, s)
	setMode(t, cfg.Dir, 0o700)
	scanDir(t, s)
	requireMissing(t, filepath.Join(cfg.Dir, fileA))
}

func TestScanDoesNotUnlinkReplacementPublishedDuringHandle(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t.TempDir())
	publish(t, cfg.Dir, fileA, "старая")
	var handled []string
	s := newTestScanner(t, cfg, func(contents []byte, _ time.Time) error {
		value := string(contents)
		handled = append(handled, value)
		if value == "старая" {
			publish(t, cfg.Dir, fileA, "новая")
		}
		return nil
	}, io.Discard, pkgClock.RealTime{})
	scanDir(t, s)
	path := filepath.Join(cfg.Dir, fileA)
	requirePresent(t, path)
	scanDir(t, s)
	requireMissing(t, path)
	if !slices.Equal(handled, []string{"старая", "новая"}) {
		t.Errorf("неожиданные публикации: %v", handled)
	}
}

type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time { return c.now }

type scannerConfig struct {
	Dir string
	aUsecase.Limits
}

func testConfig(dir string) scannerConfig {
	return scannerConfig{
		Dir:    dir,
		Limits: aUsecase.Limits{MaxFilesPerScan: 12, MaxFileBytes: 1024, MaxAttempts: 2},
	}
}

func newTestScanner(
	t *testing.T,
	cfg scannerConfig,
	handle pkgApi.Handle,
	out io.Writer,
	clk pkgClock.Clock,
) *aUsecase.Scanner {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(out, nil))
	return aUsecase.New(iFilesystem.New(cfg.Dir), handle, cfg.Limits, clk, logger)
}

func scanDir(t *testing.T, s *aUsecase.Scanner) {
	t.Helper()
	entries, err := s.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s.Scan(t.Context(), entries)
}

func publish(t *testing.T, dir, name, contents string) {
	t.Helper()
	tmp := filepath.Join(dir, name+".tmp")
	writeFile(t, tmp, contents)
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func requireMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ожидалось отсутствие %q, ошибка Lstat: %v", path, err)
	}
}

func requirePresent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("ожидалось наличие %q: %v", path, err)
	}
}

func requireUnprivileged(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("сценарий требует непривилегированного пользователя")
	}
}

func setMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func setMtime(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

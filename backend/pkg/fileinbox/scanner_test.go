//go:build linux

package fileinbox

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vendelev/sre-metrics/pkg/clock"
)

func TestScanFiltersEntriesAndRejectsWithoutLeakingInput(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t.TempDir())
	cfg.MaxFileBytes = 8
	var logs bytes.Buffer
	var handled []string
	i := newTestInbox(t, cfg, func(contents []byte, _ time.Time) error {
		value := string(contents)
		handled = append(handled, value)
		if value == "PII-body" {
			return errors.New("синтетическая ошибка PII-error: " + value)
		}
		return nil
	}, &logs, clock.RealTime{})

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

	scanDir(t, i)
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
	i := newTestInbox(t, cfg, func(contents []byte, _ time.Time) error {
		handled = append(handled, string(contents))
		return nil
	}, io.Discard, clock.RealTime{})
	publish(t, cfg.Dir, fileB, "b")
	publish(t, cfg.Dir, fileA, "a")
	publish(t, cfg.Dir, fileC, "c")
	setMtime(t, filepath.Join(cfg.Dir, fileA), now)
	setMtime(t, filepath.Join(cfg.Dir, fileB), now)
	setMtime(t, filepath.Join(cfg.Dir, fileC), now.Add(-time.Second))

	scanDir(t, i)
	requirePresent(t, filepath.Join(cfg.Dir, fileB))
	scanDir(t, i)
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
	i := newTestInbox(t, cfg, func(_ []byte, now time.Time) error {
		times = append(times, now)
		if now.Before(start.Add(2 * time.Second)) {
			return fmt.Errorf("отложено: %w", ErrRetry)
		}
		return nil
	}, io.Discard, clk)
	publish(t, cfg.Dir, fileA, "публикация")
	path := filepath.Join(cfg.Dir, fileA)
	for range 2 {
		scanDir(t, i)
		requirePresent(t, path)
		clk.now = clk.now.Add(time.Second)
	}
	scanDir(t, i)
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
	i := newTestInbox(t, cfg, func([]byte, time.Time) error {
		t.Error("нечитаемый файл передан обработчику")
		return nil
	}, &logs, clock.RealTime{})
	reader := attachDiagnostics(t, i)
	scanDir(t, i)
	requirePresent(t, path)
	scanDir(t, i)
	requireMissing(t, path)
	assertOutcomes(t, collectDiagnostics(t, reader), map[string]int64{"io_error": 2})
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
	i := newTestInbox(t, cfg, func([]byte, time.Time) error {
		calls++
		setMode(t, cfg.Dir, 0o500)
		return errors.New("синтетический отказ")
	}, io.Discard, clock.RealTime{})
	scanDir(t, i)
	path := filepath.Join(cfg.Dir, fileA)
	requirePresent(t, path)
	setMode(t, cfg.Dir, 0o700)
	scanDir(t, i)
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
	i := newTestInbox(t, cfg, func(contents []byte, _ time.Time) error {
		value := string(contents)
		handled = append(handled, value)
		if value == "первая" {
			setMode(t, cfg.Dir, 0o500)
		}
		return nil
	}, &logs, clock.RealTime{})
	reader := attachDiagnostics(t, i)

	scanDir(t, i)
	assertOutcomes(t, collectDiagnostics(t, reader), map[string]int64{"accepted": 1, "unlink_failed": 1})
	scanDir(t, i)
	assertOutcomes(t, collectDiagnostics(t, reader), map[string]int64{"accepted": 1, "unlink_failed": 2})
	setMode(t, cfg.Dir, 0o700)
	scanDir(t, i)
	m := collectDiagnostics(t, reader)
	assertOutcomes(t, m, map[string]int64{"accepted": 2, "unlink_failed": 2})
	assertDirectoryTotals(t, m, 2, int64(len("первая")+len("следующая")))
	scanDir(t, i)
	m = collectDiagnostics(t, reader)
	assertDirectoryTotals(t, m, 1, int64(len("первая")))
	if age := gaugeValue[float64](t, m[oldestAgeName], "s"); age <= 0 {
		t.Errorf("нет лага для зависшего файла: %v", age)
	}
	requirePresent(t, path)
	requireMissing(t, filepath.Join(cfg.Dir, fileB))
	publish(t, cfg.Dir, fileA, "новая под тем же именем")
	scanDir(t, i)
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
	i := newTestInbox(t, cfg, func([]byte, time.Time) error {
		setMode(t, cfg.Dir, 0o500)
		return nil
	}, io.Discard, clock.RealTime{})
	scanDir(t, i)
	if len(i.scanner.states) != 1 {
		t.Fatal("не сохранено состояние неудалённой публикации")
	}
	setMode(t, cfg.Dir, 0o700)
	if err := os.Remove(filepath.Join(cfg.Dir, fileA)); err != nil {
		t.Fatal(err)
	}
	scanDir(t, i)
	if len(i.scanner.states) != 0 {
		t.Error("сохранено состояние исчезнувшей публикации")
	}
}

func TestScanDoesNotUnlinkReplacementPublishedDuringHandle(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t.TempDir())
	publish(t, cfg.Dir, fileA, "старая")
	var handled []string
	i := newTestInbox(t, cfg, func(contents []byte, _ time.Time) error {
		value := string(contents)
		handled = append(handled, value)
		if value == "старая" {
			publish(t, cfg.Dir, fileA, "новая")
		}
		return nil
	}, io.Discard, clock.RealTime{})
	scanDir(t, i)
	path := filepath.Join(cfg.Dir, fileA)
	requirePresent(t, path)
	scanDir(t, i)
	requireMissing(t, path)
	if !slices.Equal(handled, []string{"старая", "новая"}) {
		t.Errorf("неожиданные публикации: %v", handled)
	}
}

func TestReadRejectsReplacementsSinceListing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		replace func(*testing.T, string)
	}{
		{name: "другой обычный файл", replace: func(t *testing.T, dir string) {
			publish(t, dir, fileA, "новая публикация")
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
			cfg := testConfig(t.TempDir())
			publish(t, cfg.Dir, fileA, "старая публикация")
			info, err := os.Lstat(filepath.Join(cfg.Dir, fileA))
			if err != nil {
				t.Fatal(err)
			}
			tt.replace(t, cfg.Dir)
			i := newTestInbox(t, cfg, nil, io.Discard, clock.RealTime{})
			result := make(chan error, 1)
			go func() {
				_, err := i.scanner.read(info)
				result <- err
			}()
			select {
			case err := <-result:
				if !errors.Is(err, errPublicationChanged) {
					t.Fatalf("подмена при открытии не обнаружена: %v", err)
				}
			case <-time.After(testTimeout):
				t.Fatal("чтение подменённой публикации заблокировалось")
			}
		})
	}
}

func TestReadLimitedDetectsExtraByte(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "oversized")
	writeFile(t, path, "123")
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := file.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	contents, err := readLimited(file, 2)
	if contents != nil || !errors.Is(err, errFileTooLarge) {
		t.Errorf("лишний байт не отклонён: %q, %v", contents, err)
	}
}

type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time { return c.now }

func scanDir(t *testing.T, i *Inbox) {
	t.Helper()
	entries, err := os.ReadDir(i.scanner.cfg.Dir)
	if err != nil {
		t.Fatal(err)
	}
	i.scanner.scan(t.Context(), entries)
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

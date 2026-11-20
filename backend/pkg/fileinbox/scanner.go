package fileinbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/google/uuid"
	"github.com/vendelev/sre-metrics/pkg/clock"
)

var (
	errPublicationChanged = errors.New("публикация файла изменилась")
	errFileTooLarge       = errors.New("размер публикации превышает лимит")
)

type scanner struct {
	cfg         Config
	handle      Handle
	logger      *slog.Logger
	clk         clock.Clock
	states      map[string]fileState
	diagnostics *diagnostics
}

type publication struct {
	info      os.FileInfo
	validName bool
}

type fileState struct {
	info           os.FileInfo
	readFailures   int
	unlinkFailures int
	terminal       bool
}

func (s *scanner) scan(ctx context.Context, entries []os.DirEntry) {
	files := s.candidates(ctx, entries)
	for _, file := range files[:min(len(files), s.cfg.MaxFilesPerScan)] {
		if ctx.Err() != nil {
			return
		}
		s.process(ctx, file)
	}
}

func (s *scanner) candidates(ctx context.Context, entries []os.DirEntry) []publication {
	var files []publication
	var snapshot directorySnapshot
	seen := make(map[string]struct{}, len(s.states))
	for _, entry := range entries {
		if ctx.Err() != nil {
			return nil
		}
		name := entry.Name()
		if !strings.HasSuffix(name, publishedSuffix) {
			continue
		}
		file, ok := s.candidate(ctx, name, seen, &snapshot)
		if ok {
			files = append(files, file)
		}
	}
	if s.diagnostics != nil {
		s.diagnostics.observeDirectory(ctx, snapshot)
		s.observeVolume(ctx)
	}
	for name := range s.states {
		if _, ok := seen[name]; !ok {
			delete(s.states, name)
		}
	}
	slices.SortFunc(files, func(a, b publication) int {
		if order := a.info.ModTime().Compare(b.info.ModTime()); order != 0 {
			return order
		}
		return strings.Compare(a.info.Name(), b.info.Name())
	})
	return files
}

func (s *scanner) candidate(
	ctx context.Context,
	name string,
	seen map[string]struct{},
	snapshot *directorySnapshot,
) (publication, bool) {
	state, tracked := s.states[name]
	info, err := os.Lstat(filepath.Join(s.cfg.Dir, name))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// Без метаданных нельзя отличить прежнюю публикацию от новой.
			if tracked {
				seen[name] = struct{}{}
			}
			s.diagnostics.add(ctx, ioError, 1)
			s.logger.Warn("не удалось проверить запись каталога файлового входа")
		}
		return publication{}, false
	}
	if !info.Mode().IsRegular() {
		return publication{}, false
	}
	if s.diagnostics != nil {
		snapshot.include(info)
	}
	if tracked && samePublication(state.info, info) {
		seen[name] = struct{}{}
		if state.unlinkFailures >= s.cfg.MaxAttempts {
			return publication{}, false
		}
	}
	return publication{info: info, validName: canonicalName(name)}, true
}

func canonicalName(name string) bool {
	stem := strings.TrimSuffix(name, publishedSuffix)
	id, err := uuid.Parse(stem)
	return err == nil && id.String() == stem
}

func samePublication(a, b os.FileInfo) bool {
	// Размер и mtime помогают отличить повторное использование inode после исчезновения записи.
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func (s *scanner) process(ctx context.Context, file publication) {
	name := file.info.Name()
	state, ok := s.states[name]
	if !ok {
		state.info = file.info
	}
	if !state.terminal {
		state = s.deliver(ctx, file, state)
	}
	if state.terminal {
		s.unlink(ctx, file, state)
		return
	}
	if state.readFailures > 0 {
		s.states[name] = state
	}
}

func (s *scanner) deliver(ctx context.Context, file publication, state fileState) fileState {
	if !file.validName {
		s.diagnostics.add(ctx, invalidEvent, 1)
		s.warnFile(file, "invalid_name", 0)
		state.terminal = true
		return state
	}
	contents, err := s.read(file.info)
	switch {
	case errors.Is(err, errPublicationChanged):
		return state
	case errors.Is(err, errFileTooLarge):
		s.diagnostics.add(ctx, invalidEvent, 1)
		s.warnFile(file, "file_too_large", 0)
		state.terminal = true
		return state
	case err != nil:
		s.diagnostics.add(ctx, ioError, 1)
		state.readFailures++
		s.warnFile(file, "read_failed", state.readFailures)
		state.terminal = state.readFailures >= s.cfg.MaxAttempts
		return state
	}
	if ctx.Err() != nil {
		return state
	}
	err = s.handle(contents, s.clk.Now())
	switch {
	case errors.Is(err, ErrRetry):
		s.diagnostics.add(ctx, retry, 1)
		return state
	case err != nil:
		s.diagnostics.add(ctx, invalidEvent, 1)
		s.warnFile(file, "invalid_content", 0)
	default:
		s.diagnostics.add(ctx, accepted, 1)
	}
	state.terminal = true
	return state
}

func (s *scanner) read(info os.FileInfo) ([]byte, error) {
	if info.Size() > s.cfg.MaxFileBytes {
		return nil, errFileTooLarge
	}
	path := filepath.Join(s.cfg.Dir, info.Name())
	// Не переходим по подставленной ссылке и не блокируем открытие подставленного FIFO.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ELOOP) {
		return nil, errPublicationChanged
	}
	if err != nil {
		return nil, err
	}
	defer func() {
		// Дескриптор открыт только для чтения и не содержит данных для сброса.
		_ = file.Close()
	}()
	current, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !current.Mode().IsRegular() || !samePublication(info, current) {
		return nil, errPublicationChanged
	}
	if current.Size() > s.cfg.MaxFileBytes {
		return nil, errFileTooLarge
	}
	return readLimited(file, s.cfg.MaxFileBytes)
}

func readLimited(file *os.File, limit int64) ([]byte, error) {
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
		return nil, errFileTooLarge
	case errors.Is(err, io.EOF):
		return contents, nil
	default:
		return nil, err
	}
}

func (s *scanner) unlink(ctx context.Context, file publication, state fileState) {
	name := file.info.Name()
	path := filepath.Join(s.cfg.Dir, name)
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && !samePublication(state.info, current)) {
		delete(s.states, name)
		return
	}
	if err == nil {
		// В отличие от os.Remove, unlink не удалит каталог при подмене после Lstat.
		err = syscall.Unlink(path)
	}
	if err == nil || errors.Is(err, os.ErrNotExist) {
		delete(s.states, name)
		return
	}

	state.unlinkFailures++
	s.states[name] = state
	s.diagnostics.add(ctx, unlinkFailed, 1)
	s.warnFile(file, "unlink_failed", state.unlinkFailures)
}

func (s *scanner) warnFile(file publication, reason string, attempt int) {
	// Имя вне строгого формата и ошибки обработчика/ФС могут содержать произвольные данные.
	attrs := []any{slog.String("reason", reason), slog.Int64("size", file.info.Size())}
	if file.validName {
		attrs = append(attrs, slog.String("file", file.info.Name()))
	}
	if attempt != 0 {
		attrs = append(attrs, slog.Int("attempt", attempt), slog.Int("max_attempts", s.cfg.MaxAttempts))
	}
	s.logger.Warn("сбой обработки публикации файлового входа", attrs...)
}

// Package usecase реализует правила последовательной обработки публикаций.
package usecase

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"

	pkgClock "github.com/vendelev/sre-metrics/pkg/clock"
	pkgApi "github.com/vendelev/sre-metrics/pkg/fileinbox/api"
	dContract "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/domain/contract"
	dDto "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/domain/dto"
	dPublication "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/domain/publication"
)

// Limits содержит проверенные в bootstrap границы обработки.
type Limits struct {
	MaxFilesPerScan int
	MaxFileBytes    int64
	MaxAttempts     int
}

// Scanner определяет порядок, исходы и повторы обработки одной пачки публикаций.
// Один владелец вызывает Scan последовательно; конкурентный доступ не поддерживается.
type Scanner struct {
	files       dContract.FileSystem
	diagnostics dContract.Diagnostics
	handle      pkgApi.Handle
	limits      Limits
	clk         pkgClock.Clock
	logger      *slog.Logger
	states      map[string]fileState
}

type fileState struct {
	publication    dPublication.Metadata
	readFailures   int
	unlinkFailures int
	terminal       bool
}

// New создаёт обработчик с проверенными настройками. Диагностика включается отдельно.
func New(
	files dContract.FileSystem,
	handle pkgApi.Handle,
	limits Limits,
	clk pkgClock.Clock,
	logger *slog.Logger,
) *Scanner {
	return &Scanner{
		files:  files,
		handle: handle,
		limits: limits,
		clk:    clk,
		logger: logger,
		states: make(map[string]fileState),
	}
}

// SetDiagnostics включает диагностику до начала обработки.
func (s *Scanner) SetDiagnostics(diagnostics dContract.Diagnostics) {
	s.diagnostics = diagnostics
}

// Clear освобождает состояние повторов после остановки приёма.
func (s *Scanner) Clear() {
	clear(s.states)
}

// Prepare проверяет возможность работы с каталогом до открытия наблюдения.
func (s *Scanner) Prepare(ctx context.Context) error {
	err := s.files.Prepare()
	if err != nil {
		s.add(ctx, dContract.IOError)
	}
	return err
}

// List читает каталог отдельно от обработки пачки, чтобы владелец мог объявить готовность.
func (s *Scanner) List(ctx context.Context) ([]string, error) {
	entries, err := s.files.Entries()
	if err != nil {
		s.add(ctx, dContract.IOError)
	}
	return entries, err
}

// Scan выбирает ограниченную пачку публикаций по mtime и имени и обрабатывает её.
func (s *Scanner) Scan(ctx context.Context, entries []string) {
	files := s.candidates(ctx, entries)
	for _, file := range files[:min(len(files), s.limits.MaxFilesPerScan)] {
		if ctx.Err() != nil {
			return
		}
		s.process(ctx, file)
	}
}

func (s *Scanner) candidates(ctx context.Context, entries []string) []dPublication.Metadata {
	var files []dPublication.Metadata
	var snapshot dDto.DirectorySnapshot
	seen := make(map[string]struct{}, len(s.states))
	for _, name := range entries {
		if ctx.Err() != nil {
			return nil
		}
		if !strings.HasSuffix(name, dPublication.Suffix) {
			continue
		}
		file, ok := s.candidate(ctx, name, seen, &snapshot)
		if ok {
			files = append(files, file)
		}
	}
	if s.diagnostics != nil {
		s.diagnostics.ObserveDirectory(ctx, snapshot)
		s.observeVolume(ctx)
	}
	for name := range s.states {
		if _, ok := seen[name]; !ok {
			delete(s.states, name)
		}
	}
	slices.SortFunc(files, func(a, b dPublication.Metadata) int {
		if order := a.Identity.Modified.Compare(b.Identity.Modified); order != 0 {
			return order
		}
		return strings.Compare(a.Name, b.Name)
	})
	return files
}

func (s *Scanner) candidate(
	ctx context.Context,
	name string,
	seen map[string]struct{},
	snapshot *dDto.DirectorySnapshot,
) (dPublication.Metadata, bool) {
	state, tracked := s.states[name]
	file, err := s.files.Inspect(name)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// Без метаданных нельзя отличить прежнюю публикацию от новой.
			if tracked {
				seen[name] = struct{}{}
			}
			s.add(ctx, dContract.IOError)
			s.logger.Warn("не удалось проверить запись каталога файлового входа")
		}
		return dPublication.Metadata{}, false
	}
	if !file.IsRegular {
		return dPublication.Metadata{}, false
	}
	if s.diagnostics != nil {
		snapshot.Include(file)
	}
	if tracked && dPublication.Same(state.publication.Identity, file.Identity) {
		seen[name] = struct{}{}
		if state.unlinkFailures >= s.limits.MaxAttempts {
			return dPublication.Metadata{}, false
		}
	}
	return file, true
}

func (s *Scanner) process(ctx context.Context, file dPublication.Metadata) {
	name := file.Name
	state, ok := s.states[name]
	if !ok {
		state.publication = file
	}
	if !state.terminal {
		s.deliver(ctx, file, &state)
	}
	if state.terminal {
		s.unlink(ctx, file, &state)
		return
	}
	if state.readFailures > 0 {
		s.states[name] = state
	}
}

func (s *Scanner) deliver(ctx context.Context, file dPublication.Metadata, state *fileState) {
	if !file.ValidName {
		s.add(ctx, dContract.InvalidEvent)
		s.warnFile(file, "invalid_name", 0)
		state.terminal = true
		return
	}
	contents, err := s.files.Read(file, s.limits.MaxFileBytes)
	switch {
	case errors.Is(err, dContract.ErrPublicationChanged):
		return
	case errors.Is(err, dContract.ErrFileTooLarge):
		s.add(ctx, dContract.InvalidEvent)
		s.warnFile(file, "file_too_large", 0)
		state.terminal = true
		return
	case err != nil:
		s.add(ctx, dContract.IOError)
		state.readFailures++
		s.warnFile(file, "read_failed", state.readFailures)
		state.terminal = state.readFailures >= s.limits.MaxAttempts
		return
	}
	if ctx.Err() != nil {
		return
	}
	err = s.handle(contents, s.clk.Now())
	switch {
	case errors.Is(err, pkgApi.ErrRetry):
		s.add(ctx, dContract.Retry)
		return
	case err != nil:
		s.add(ctx, dContract.InvalidEvent)
		s.warnFile(file, "invalid_content", 0)
	default:
		s.add(ctx, dContract.Accepted)
	}
	state.terminal = true
}

func (s *Scanner) unlink(ctx context.Context, file dPublication.Metadata, state *fileState) {
	name := file.Name
	err := s.files.Unlink(state.publication)
	if err == nil || errors.Is(err, dContract.ErrPublicationChanged) {
		delete(s.states, name)
		return
	}

	state.unlinkFailures++
	s.states[name] = *state
	s.add(ctx, dContract.UnlinkFailed)
	s.warnFile(file, "unlink_failed", state.unlinkFailures)
}

func (s *Scanner) observeVolume(ctx context.Context) {
	usage, ok, err := s.files.VolumeUsage()
	if err != nil {
		s.add(ctx, dContract.IOError)
		s.logger.WarnContext(ctx, "не удалось измерить заполнение тома файлового входа")
		return
	}
	if ok {
		s.diagnostics.ObserveVolume(ctx, usage)
	}
}

func (s *Scanner) add(ctx context.Context, outcome dContract.Outcome) {
	if s.diagnostics != nil {
		s.diagnostics.Add(ctx, outcome, 1)
	}
}

func (s *Scanner) warnFile(file dPublication.Metadata, reason string, attempt int) {
	// Имя вне строгого формата и ошибки обработчика/ФС могут содержать произвольные данные.
	attrs := []any{slog.String("reason", reason), slog.Int64("size", file.Identity.Size)}
	if file.ValidName {
		attrs = append(attrs, slog.String("file", file.Name))
	}
	if attempt != 0 {
		attrs = append(attrs, slog.Int("attempt", attempt), slog.Int("max_attempts", s.limits.MaxAttempts))
	}
	s.logger.Warn("сбой обработки публикации файлового входа", attrs...)
}

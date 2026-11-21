package contract

import (
	"context"

	dDto "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/domain/dto"
)

// Outcome определяет контрактные значения атрибута диагностического счётчика.
type Outcome string

const (
	// Accepted обозначает успешную передачу публикации обработчику.
	Accepted Outcome = "accepted"
	// InvalidEvent обозначает окончательный отказ во входных данных.
	InvalidEvent Outcome = "invalid_event"
	// IOError обозначает сбой файловой операции, кроме удаления.
	IOError Outcome = "io_error"
	// UnlinkFailed обозначает неудачную попытку удаления публикации.
	UnlinkFailed Outcome = "unlink_failed"
	// Retry обозначает отложенную попытку приёма.
	Retry Outcome = "retry"
)

// Diagnostics принимает результаты обработки и снимок каталога.
type Diagnostics interface {
	Add(ctx context.Context, value Outcome, count int64)
	ObserveDirectory(ctx context.Context, snapshot dDto.DirectorySnapshot)
	ObserveVolume(ctx context.Context, volume float64)
}

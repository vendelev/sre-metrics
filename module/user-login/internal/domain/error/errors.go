// Package error определяет общие ошибки приёма и обработки событий.
package error

import (
	"errors"
)

var (
	// ErrInvalidEvent означает недопустимые предметные данные или возраст события при приёме.
	ErrInvalidEvent = errors.New("невалидное событие")
	// ErrCapacityExceeded означает недостаток места без вытеснения принятых данных.
	ErrCapacityExceeded = errors.New("недостаточно вместимости")
	// ErrOverloaded означает переполнение очереди приёма, а не лимит сценариев или событий.
	ErrOverloaded = errors.New("очередь приёма переполнена")
	// ErrInternal означает неожиданный внутренний сбой, а не предметный отказ.
	ErrInternal = errors.New("внутренний сбой обработки")
	// ErrConflict означает противоречие ранее принятому факту сценария.
	ErrConflict = errors.New("противоречие ранее принятому факту сценария")
)

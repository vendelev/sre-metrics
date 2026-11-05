package domain

import "errors"

var (
	// ErrInvalidEvent означает недопустимые предметные данные или возраст нового события.
	ErrInvalidEvent = errors.New("невалидное событие")
	// ErrCapacityExceeded означает недостаток места без вытеснения принятых данных.
	ErrCapacityExceeded = errors.New("недостаточно вместимости")
	// ErrInternal означает неожиданный внутренний сбой, а не предметный отказ.
	ErrInternal = errors.New("внутренний сбой обработки")
)

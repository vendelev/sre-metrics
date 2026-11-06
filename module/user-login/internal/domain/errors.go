package domain

import (
	"errors"
	"time"
)

var (
	// ErrInvalidEvent означает недопустимые предметные данные или возраст нового события.
	ErrInvalidEvent = errors.New("невалидное событие")
	// ErrCapacityExceeded означает недостаток места без вытеснения принятых данных.
	ErrCapacityExceeded = errors.New("недостаточно вместимости")
	// ErrInternal означает неожиданный внутренний сбой, а не предметный отказ.
	ErrInternal = errors.New("внутренний сбой обработки")
)

// ErrEventTime означает выход исходного времени за допустимый диапазон нового события.
var ErrEventTime = errors.New("время нового события вне допустимого диапазона")

// CheckEventTime проверяет BR-37 без изменения состояния: обе границы включительны.
// Применяется только к новому event_id после дедупликации, с единым временем операции.
// Не проверяет wire-формат времени и не меняет deadline сценария.
func CheckEventTime(occurredAt, now time.Time) error {
	if occurredAt.Before(now.Add(-30*time.Minute)) || occurredAt.After(now.Add(30*time.Second)) {
		return ErrEventTime
	}
	return nil
}

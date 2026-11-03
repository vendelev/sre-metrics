package domain

import (
	"errors"
	"time"
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

// EventType обозначает тип факта входа.
type EventType string

const (
	EventTypeLoginStarted        EventType = "login_started"
	EventTypeLoginModeDetermined EventType = "login_mode_determined"
	EventTypeLoginSucceeded      EventType = "login_succeeded"
	EventTypeLoginFailed         EventType = "login_failed"
	EventTypeLoginRejected       EventType = "login_rejected"
)

// Source обозначает приложение, сообщившее факт.
type Source string

const (
	SourceMIS  Source = "mis"
	SourceAuth Source = "auth"
)

// LoginMode обозначает вариант входа.
type LoginMode string

const (
	LoginModeCredentials     LoginMode = "credentials"
	LoginModeExistingSession LoginMode = "existing_session"
)

// Event объединяет предметные данные одного факта входа.
// Само объявление структуры не обеспечивает проверку контракта.
type Event struct {
	EventID    string
	ScenarioID string
	OccurredAt time.Time
	Source     Source
	Type       EventType
	// Mode содержит вариант входа; nil означает его отсутствие.
	Mode *LoginMode
}

// Clone возвращает независимое событие, включая значение необязательного Mode.
func (e Event) Clone() Event {
	if e.Mode != nil {
		mode := *e.Mode
		e.Mode = &mode
	}
	return e
}

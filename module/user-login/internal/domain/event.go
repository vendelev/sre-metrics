package domain

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Source обозначает приложение, сообщившее факт.
type Source string

const (
	SourceMIS  Source = "mis"
	SourceAuth Source = "auth"
)

func ParseSource(value string) (Source, error) {
	s := Source(value)
	switch s {
	case SourceMIS,
		SourceAuth:
		return s, nil
	default:
		return "", errors.New("невалидное название приложения")
	}
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

func ParseEventType(value string, source Source, mode *OAuthMode) (EventType, error) {
	er := errors.New("невалидный тип события")
	et := EventType(value)
	switch et {
	case EventTypeLoginStarted,
		EventTypeLoginModeDetermined,
		EventTypeLoginSucceeded,
		EventTypeLoginFailed,
		EventTypeLoginRejected:
		break
	default:
		return "", er
	}

	isAuthService := mode != nil

	switch et {
	case EventTypeLoginStarted,
		EventTypeLoginSucceeded:
		if source != SourceMIS || isAuthService {
			return "", er
		}
	case EventTypeLoginFailed,
		EventTypeLoginRejected:
		if (source != SourceMIS && source != SourceAuth) || isAuthService {
			return "", er
		}
	case EventTypeLoginModeDetermined:
		if source != SourceAuth || mode == nil {
			return "", er
		}

		if *mode != LoginModeCredentials && *mode != LoginModeExistingSession {
			return "", er
		}
	}

	return et, nil
}

// OAuthMode обозначает вариант входа в сервисе авторизации.
type OAuthMode string

const (
	// LoginModeCredentials новый вход по логину и паролю
	LoginModeCredentials OAuthMode = "credentials"
	// LoginModeExistingSession повторный вход, используется сохраненная сессия
	LoginModeExistingSession OAuthMode = "existing_session"
)

// ParseMode разбирает вариант входа; пустая строка означает отсутствие значения.
func ParseMode(value *string) (*OAuthMode, error) {
	if value == nil || *value == "" {
		return nil, nil
	}

	m := OAuthMode(*value)

	switch m {
	case LoginModeCredentials, LoginModeExistingSession:
		return &m, nil
	default:
		return nil, errors.New("невалидный вариант входа")
	}
}

// Event объединяет предметные данные одного факта входа.
// Само объявление структуры не обеспечивает проверку контракта.
type Event struct {
	EventID    uuid.UUID
	ScenarioID uuid.UUID
	OccurredAt time.Time
	Source     Source
	Type       EventType
	// Mode содержит вариант входа; nil означает его отсутствие.
	Mode *OAuthMode
}

// Clone возвращает независимое событие, включая значение необязательного Mode.
func (e Event) Clone() Event {
	if e.Mode != nil {
		mode := *e.Mode
		e.Mode = &mode
	}
	return e
}

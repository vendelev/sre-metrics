// Package event определяет DTO входящего события, типы его полей
// и правила разбора источника, типа факта и варианта входа.
package event

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

// Type обозначает тип факта входа.
type Type string

const (
	LoginStarted        Type = "login_started"
	LoginModeDetermined Type = "login_mode_determined"
	LoginSucceeded      Type = "login_succeeded"
	LoginFailed         Type = "login_failed"
	LoginRejected       Type = "login_rejected"
)

func ParseType(value string, source Source, mode *OAuthMode) (Type, error) {
	er := errors.New("невалидный тип события")
	et := Type(value)
	switch et {
	case LoginStarted,
		LoginModeDetermined,
		LoginSucceeded,
		LoginFailed,
		LoginRejected:
		break
	default:
		return "", er
	}

	isAuthService := mode != nil

	switch et {
	case LoginStarted,
		LoginSucceeded:
		if source != SourceMIS || isAuthService {
			return "", er
		}
	case LoginFailed,
		LoginRejected:
		if (source != SourceMIS && source != SourceAuth) || isAuthService {
			return "", er
		}
	case LoginModeDetermined:
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

// ParseTime преобразует строку в Time и проверяет BR-37: обе границы включительны.
func ParseTime(value string, now time.Time) (time.Time, error) {
	occurredAt, err := time.Parse("2006-01-02T15:04:05.000Z", value)
	if err != nil {
		return now, errors.New("невалидное время события")
	}

	if occurredAt.Before(now.Add(-30*time.Minute)) || occurredAt.After(now.Add(30*time.Second)) {
		return now, errors.New("время события вне допустимого диапазона")
	}

	return occurredAt, nil
}

// Request объединяет предметные данные одного факта входа.
// Само объявление структуры не обеспечивает проверку контракта.
type Request struct {
	EventID    uuid.UUID
	ScenarioID uuid.UUID
	OccurredAt time.Time
	Source     Source
	Type       Type
	// AuthMode содержит вариант входа; nil означает его отсутствие.
	AuthMode *OAuthMode
}

// Clone возвращает независимое событие, включая значение необязательного AuthMode.
func (e *Request) Clone() Request {
	copy := *e
	if e.AuthMode != nil {
		mode := *e.AuthMode
		copy.AuthMode = &mode
	}
	return copy
}

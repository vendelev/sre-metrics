package application

import (
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/vendelev/sre-metrics/service/user-login/internal/domain"
)

// prepareEvent защищает прямой предметный вызов, но не заменяет транспортную проверку.
// Presentation проверяет version=1, однозначность JSON, закрытую схему и исходное
// представление occurred_at (UTC/Z, ровно три знака миллисекунд). Event представляет
// уже разобранное событие версии 1; часовой пояс time.Time не влияет на равенство.
// Отправитель не должен конкурентно изменять Mode во время вызова Process.
func prepareEvent(event domain.Event) (domain.Event, error) {
	if !isUUIDv7(event.EventID) || !isUUIDv7(event.ScenarioID) {
		return domain.Event{}, fmt.Errorf("идентификаторы должны быть UUID v7: %w", domain.ErrInvalidEvent)
	}
	validTime := event.OccurredAt.Year() >= 0 && event.OccurredAt.Year() <= 9999
	if !validTime || event.OccurredAt.Nanosecond()%1_000_000 != 0 {
		return domain.Event{}, fmt.Errorf("время должно иметь миллисекундную точность: %w", domain.ErrInvalidEvent)
	}
	if !validEventFields(event) {
		return domain.Event{}, fmt.Errorf(
			"недопустимое сочетание типа, источника и варианта: %w",
			domain.ErrInvalidEvent,
		)
	}
	event = event.Clone()
	event.EventID = strings.ToLower(event.EventID)
	event.ScenarioID = strings.ToLower(event.ScenarioID)
	return event, nil
}

func validEventFields(event domain.Event) bool {
	switch event.Type {
	case domain.EventTypeLoginStarted, domain.EventTypeLoginSucceeded:
		return event.Source == domain.SourceMIS && event.Mode == nil
	case domain.EventTypeLoginFailed, domain.EventTypeLoginRejected:
		validSource := event.Source == domain.SourceMIS || event.Source == domain.SourceAuth
		return validSource && event.Mode == nil
	case domain.EventTypeLoginModeDetermined:
		if event.Source != domain.SourceAuth || event.Mode == nil {
			return false
		}
		return *event.Mode == domain.LoginModeCredentials || *event.Mode == domain.LoginModeExistingSession
	default:
		return false
	}
}

func isUUIDv7(id string) bool {
	// Parse допускает также URN, скобки и запись без дефисов, а наш контракт — только 36 символов.
	if len(id) != 36 {
		return false
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		return false
	}
	return parsed.Version() == 7 && parsed.Variant() == uuid.RFC4122
}

func equalEvents(a, b domain.Event) bool {
	sameIDs := a.EventID == b.EventID && a.ScenarioID == b.ScenarioID
	sameFact := a.Source == b.Source && a.Type == b.Type && a.OccurredAt.Equal(b.OccurredAt)
	if !sameIDs || !sameFact {
		return false
	}
	if a.Mode == nil || b.Mode == nil {
		return a.Mode == nil && b.Mode == nil
	}
	return *a.Mode == *b.Mode
}

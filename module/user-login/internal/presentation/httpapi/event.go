package httpapi

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/vendelev/sre-metrics/user-login/internal/domain"
)

type eventRequest struct {
	EventID    string  `json:"event_id"`
	ScenarioID string  `json:"scenario_id"`
	OccurredAt string  `json:"occurred_at"`
	Source     string  `json:"source"`
	Type       string  `json:"type"`
	Mode       *string `json:"mode"`
}

func decodeEvent(body []byte) (domain.Event, error) {
	var request eventRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return domain.Event{}, err
	}

	eventID, err := uuid.Parse(request.EventID)
	if err != nil {
		return domain.Event{}, errors.New("невалидное id события")
	}

	scenarioID, err := uuid.Parse(request.ScenarioID)
	if err != nil {
		return domain.Event{}, errors.New("невалидное id сценария")
	}

	occurredAt, err := time.Parse("2006-01-02T15:04:05.000Z", request.OccurredAt)
	if err != nil {
		return domain.Event{}, errors.New("невалидное время события")
	}

	source, err := domain.ParseSource(request.Source)
	if err != nil {
		return domain.Event{}, err
	}

	mode, err := domain.ParseMode(request.Mode)
	if err != nil {
		return domain.Event{}, err
	}

	eventType, err := domain.ParseEventType(request.Type, source, mode)
	if err != nil {
		return domain.Event{}, err
	}

	event := domain.Event{
		EventID:    eventID,
		ScenarioID: scenarioID,
		OccurredAt: occurredAt,
		Source:     source,
		Type:       eventType,
		Mode:       mode,
	}

	return event, nil
}

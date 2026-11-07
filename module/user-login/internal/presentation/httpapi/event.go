package httpapi

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	dEvent "github.com/vendelev/sre-metrics/user-login/internal/domain/event"
)

type rawEvent struct {
	EventID    string  `json:"event_id"`
	ScenarioID string  `json:"scenario_id"`
	OccurredAt string  `json:"occurred_at"`
	Source     string  `json:"source"`
	Type       string  `json:"type"`
	Mode       *string `json:"mode"`
}

func decodeEvent(body []byte) (dEvent.Request, error) {
	var request rawEvent
	if err := json.Unmarshal(body, &request); err != nil {
		return dEvent.Request{}, err
	}

	eventID, err := uuid.Parse(request.EventID)
	if err != nil {
		return dEvent.Request{}, errors.New("невалидное id события")
	}

	scenarioID, err := uuid.Parse(request.ScenarioID)
	if err != nil {
		return dEvent.Request{}, errors.New("невалидное id сценария")
	}

	occurredAt, err := time.Parse("2006-01-02T15:04:05.000Z", request.OccurredAt)
	if err != nil {
		return dEvent.Request{}, errors.New("невалидное время события")
	}

	source, err := dEvent.ParseSource(request.Source)
	if err != nil {
		return dEvent.Request{}, err
	}

	mode, err := dEvent.ParseMode(request.Mode)
	if err != nil {
		return dEvent.Request{}, err
	}

	eventType, err := dEvent.ParseType(request.Type, source, mode)
	if err != nil {
		return dEvent.Request{}, err
	}

	event := dEvent.Request{
		EventID:    eventID,
		ScenarioID: scenarioID,
		OccurredAt: occurredAt,
		Source:     source,
		Type:       eventType,
		AuthMode:   mode,
	}

	return event, nil
}

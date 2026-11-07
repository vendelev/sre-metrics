package httpapi

import (
	"encoding/json"
	"errors"
	"io"
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

func decodeEvent(body io.Reader) (dEvent.Request, error) {
	decoder := json.NewDecoder(body)
	var request rawEvent
	if err := decoder.Decode(&request); err != nil {
		return dEvent.Request{}, err
	}
	// В теле допускается только один JSON-объект и пробелы после него.
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return dEvent.Request{}, errors.New("после события ожидается конец тела")
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

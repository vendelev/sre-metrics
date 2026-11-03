package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/vendelev/sre-metrics/service/user-auth/internal/domain"
)

var (
	uuidPattern = regexp.MustCompile(
		`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-7[0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`,
	)
	timePattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}Z$`)
)

// transportEvent сохраняет присутствие полей, отличая отсутствие от JSON null.
// Имена проверяются после декодирования escape-последовательностей, без подбора регистра.
type transportEvent map[string]json.RawMessage

func decodeEvent(body []byte) (domain.Event, error) {
	fields, err := decodeObject(body)
	if err != nil {
		return domain.Event{}, err
	}
	for _, name := range []string{"version", "event_id", "scenario_id", "occurred_at", "source", "type"} {
		if _, ok := fields[name]; !ok {
			return domain.Event{}, domain.ErrInvalidEvent
		}
	}
	if !versionOne(fields["version"]) {
		return domain.Event{}, domain.ErrInvalidEvent
	}
	values := make(map[string]string, len(fields)-1)
	for name, raw := range fields {
		if name == "version" {
			continue
		}
		var value string
		if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &value) != nil {
			return domain.Event{}, domain.ErrInvalidEvent
		}
		values[name] = value
	}
	if !uuidPattern.MatchString(values["event_id"]) || !uuidPattern.MatchString(values["scenario_id"]) {
		return domain.Event{}, domain.ErrInvalidEvent
	}
	eventID, err := uuid.Parse(values["event_id"])
	if err != nil {
		return domain.Event{}, domain.ErrInvalidEvent
	}
	scenarioID, err := uuid.Parse(values["scenario_id"])
	if err != nil {
		return domain.Event{}, domain.ErrInvalidEvent
	}
	if !timePattern.MatchString(values["occurred_at"]) {
		return domain.Event{}, domain.ErrInvalidEvent
	}
	occurredAt, err := time.Parse("2006-01-02T15:04:05.000Z", values["occurred_at"])
	if err != nil {
		return domain.Event{}, domain.ErrInvalidEvent
	}
	event := domain.Event{
		EventID: eventID.String(), ScenarioID: scenarioID.String(), OccurredAt: occurredAt,
		Source: domain.Source(values["source"]), Type: domain.EventType(values["type"]),
	}
	mode, hasMode := values["mode"]
	switch event.Type {
	case domain.EventTypeLoginStarted, domain.EventTypeLoginSucceeded:
		if event.Source != domain.SourceMIS || hasMode {
			return domain.Event{}, domain.ErrInvalidEvent
		}
	case domain.EventTypeLoginFailed, domain.EventTypeLoginRejected:
		if (event.Source != domain.SourceMIS && event.Source != domain.SourceAuth) || hasMode {
			return domain.Event{}, domain.ErrInvalidEvent
		}
	case domain.EventTypeLoginModeDetermined:
		if event.Source != domain.SourceAuth || !hasMode ||
			(mode != string(domain.LoginModeCredentials) && mode != string(domain.LoginModeExistingSession)) {
			return domain.Event{}, domain.ErrInvalidEvent
		}
		parsedMode := domain.LoginMode(mode)
		event.Mode = &parsedMode
	default:
		return domain.Event{}, domain.ErrInvalidEvent
	}
	return event, nil
}

func decodeObject(body []byte) (transportEvent, error) {
	if !utf8.Valid(body) {
		return nil, domain.ErrInvalidEvent
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, domain.ErrInvalidEvent
	}
	fields := make(transportEvent, 7)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, domain.ErrInvalidEvent
		}
		name, ok := token.(string)
		if !ok {
			return nil, domain.ErrInvalidEvent
		}
		switch name {
		case "version", "event_id", "scenario_id", "occurred_at", "source", "type", "mode":
		default:
			return nil, domain.ErrInvalidEvent
		}
		if _, exists := fields[name]; exists {
			return nil, domain.ErrInvalidEvent
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, domain.ErrInvalidEvent
		}
		fields[name] = raw
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return nil, domain.ErrInvalidEvent
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, domain.ErrInvalidEvent
	}
	return fields, nil
}

// versionOne сравнивает десятичное JSON-число с 1 точно, без float64 и разворачивания
// огромной степени десяти. Синтаксис числа предварительно проверен JSON-декодером.
func versionOne(raw json.RawMessage) bool {
	number := strings.ToLower(string(raw))
	mantissa, exponent, hasExponent := strings.Cut(number, "e")
	whole, fraction, _ := strings.Cut(mantissa, ".")
	digits := whole + fraction
	withoutZeros := strings.TrimRight(digits, "0")
	if strings.TrimLeft(withoutZeros, "0") != "1" {
		return false
	}
	wantExponent := int64(len(fraction) - (len(digits) - len(withoutZeros)))
	if !hasExponent {
		return wantExponent == 0
	}
	parsed, ok := new(big.Int).SetString(exponent, 10)
	return ok && parsed.Cmp(big.NewInt(wantExponent)) == 0
}

package factory_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vendelev/sre-metrics/user-login/internal/application/factory"
	"github.com/vendelev/sre-metrics/user-login/internal/domain/event"
)

var (
	scenarioID = uuid.MustParse("019aabcd-0000-7000-8000-000000000100")
	eventID    = uuid.MustParse("019aabcd-0000-7000-8000-000000000001")
	wireTime   = "2026-11-02T12:00:00.000Z"
)

func eventBody(id uuid.UUID, kind, source, at, mode string) string {
	body := fmt.Sprintf(`{"event_id":%q,"scenario_id":%q,"occurred_at":%q,"source":%q,"type":%q`,
		id.String(), scenarioID, at, source, kind)
	if mode != "" {
		body += fmt.Sprintf(`,"mode":%q`, mode)
	}
	return body + "}"
}

func startedBody() string { return eventBody(eventID, "login_started", "mis", wireTime, "") }

func decodeEvent(t *testing.T, body string) event.Request {
	t.Helper()
	got, err := factory.DecodeEvent(strings.NewReader(body), time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("разбор события: %v", err)
	}
	return got
}

func replaceField(body, name, raw string) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		panic(err)
	}
	if raw == "" {
		delete(fields, name)
	} else {
		fields[name] = json.RawMessage(raw)
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func TestEventInvalidJSON(t *testing.T) {
	valid := startedBody()
	cases := []struct{ name, body string }{
		{"пусто", ""}, {"пробелы", " \n\t"}, {"null", "null"},
		{"массив", "[" + valid + "]"}, {"строка", `"строка"`}, {"число", "1"}, {"bool", "true"},
		{"объект без полей", "{}"}, {"оборванный объект", valid[:len(valid)-1]},
		{"ключ без значения", `{"type":}`}, {"ключ без двоеточия", `{"type" "login_started"}`},
		{"незакрытая строка ключа", `{"type`},
		{"лишняя запятая", valid[:len(valid)-1] + ",}"},
		{"комментарий", valid + "// комментарий"},
		{"второй объект", valid + "{}"}, {"второй скаляр", valid + "null"},
		{"хвост", valid + " мусор"},
		{"вложенный объект", replaceField(valid, "source", `{"source":"mis","source":"mis"}`)},
		{"невалидный UTF-8", strings.Replace(valid, "mis", "mi\xff", 1)},
		{"невалидный UTF-8 в хвосте", valid + "\xff"},
		{"BOM", "\xef\xbb\xbf" + valid},
	}
	assertStructuralRejections(t, cases)
}

func TestEventFieldSchema(t *testing.T) {
	valid := startedBody()
	modeBody := eventBody(eventID, "login_mode_determined", "auth", wireTime, "credentials")
	var cases []struct{ name, body string }
	for _, field := range []string{"event_id", "scenario_id", "occurred_at", "source", "type", "mode"} {
		base := valid
		if field == "mode" {
			base = modeBody
		}
		for _, raw := range []string{"", "null", "true", "[]", "{}"} {
			cases = append(cases, struct{ name, body string }{field + "/" + raw, replaceField(base, field, raw)})
		}
		cases = append(cases, struct{ name, body string }{field + "/число", replaceField(base, field, "1")})
	}
	for _, field := range []string{"event_id", "scenario_id"} {
		for _, id := range []string{
			"", "не UUID", "019aabcd-0000-7000-8000-00000000000g",
			"019aabcdx0000-7000-8000-000000000001",
		} {
			cases = append(
				cases,
				struct{ name, body string }{field + "/" + id, replaceField(valid, field, fmt.Sprintf("%q", id))},
			)
		}
	}
	for _, at := range []string{
		"2026-02-29T12:00:00.000Z", "2026-11-31T12:00:00.000Z", "2026-13-02T12:00:00.000Z",
		"2026-11-02T24:00:00.000Z", "2026-11-02T12:60:00.000Z", "2026-11-02T12:00:60.000Z",
		"2026-11-02t12:00:00.000Z", "2026-11-02T12:00:00.000z", "2026-11-02T12:00:00.000+00:00",
		"2026-11-02T12:00:00Z", "2026-11-02T12:00:00.00Z", "2026-11-02T12:00:00.0000Z",
		"2026-1-02T12:00:00.000Z", "",
	} {
		cases = append(
			cases,
			struct{ name, body string }{"occurred_at/" + at, replaceField(valid, "occurred_at", fmt.Sprintf("%q", at))},
		)
	}
	for _, kind := range []string{"login_unknown", "LOGIN_STARTED", ""} {
		cases = append(
			cases,
			struct{ name, body string }{"type/" + kind, replaceField(valid, "type", fmt.Sprintf("%q", kind))},
		)
	}
	for _, source := range []string{"auth", "browser", "MIS", ""} {
		cases = append(
			cases,
			struct{ name, body string }{"source/" + source, replaceField(valid, "source", fmt.Sprintf("%q", source))},
		)
	}
	for _, kind := range []string{"login_started", "login_succeeded", "login_failed", "login_rejected"} {
		for _, mode := range []string{`"credentials"`, `"existing_session"`} {
			body := eventBody(eventID, kind, "mis", wireTime, "")
			cases = append(
				cases,
				struct{ name, body string }{kind + "/лишний mode/" + mode, replaceField(body, "mode", mode)},
			)
		}
	}
	for _, mode := range []string{"unknown", "Credentials", ""} {
		cases = append(
			cases,
			struct{ name, body string }{"mode/" + mode, replaceField(modeBody, "mode", fmt.Sprintf("%q", mode))},
		)
	}
	for _, kind := range []string{"login_mode_determined", "login_succeeded", "login_failed", "login_rejected"} {
		base := eventBody(eventID, kind, "browser", wireTime, "")
		if kind == "login_mode_determined" {
			base = replaceField(modeBody, "source", `"mis"`)
		}
		cases = append(cases, struct{ name, body string }{kind + "/неверный источник", base})
	}
	assertStructuralRejections(t, cases)
}

func assertStructuralRejections(t *testing.T, cases []struct{ name, body string }) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := factory.DecodeEvent(strings.NewReader(tc.body), time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC))
			if err == nil {
				t.Fatal("невалидное событие принято фабрикой")
			}
		})
	}
}

func TestEventJSONDecoding(t *testing.T) {
	valid := startedBody()
	cases := []struct{ name, body string }{
		{"неизвестный ключ", valid[:len(valid)-1] + `,"extra":{"value":true}}`},
		{"повтор ключа", valid[:len(valid)-1] + `,"type":"login_started"}`},
		{"последнее значение ключа", strings.Replace(valid, `"source":"mis"`, `"source":"auth","source":"mis"`, 1)},
		{"повтор escaped ключа", valid[:len(valid)-1] + `,"\u0074ype":"login_started"}`},
		{"регистр ключа", strings.Replace(valid, `"type"`, `"TYPE"`, 1)},
		{"разделитель миллисекунд", strings.Replace(valid, wireTime, "2026-11-02T12:00:00,000Z", 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := decodeEvent(t, valid)
			got := decodeEvent(t, tc.body)
			if !reflect.DeepEqual(got, want) {
				t.Fatal("эквивалентный JSON изменил разобранное событие")
			}
		})
	}
}

func TestEventUUIDRepresentations(t *testing.T) {
	for _, field := range []string{"event_id", "scenario_id"} {
		for _, id := range []string{
			"019aabcd-0000-4000-8000-000000000001",
			"019aabcd-0000-7000-7000-000000000001",
			"019aabcd-0000-7000-c000-000000000001",
			"019aabcd000070008000000000000001",
			"{019aabcd-0000-7000-8000-000000000001}",
			"urn:uuid:019aabcd-0000-7000-8000-000000000001",
		} {
			t.Run(field+"/"+id, func(t *testing.T) {
				body := replaceField(startedBody(), field, fmt.Sprintf("%q", id))
				got := decodeEvent(t, body)
				canonical := uuid.MustParse(id).String()
				want := decodeEvent(t, replaceField(body, field, fmt.Sprintf("%q", canonical)))
				if !reflect.DeepEqual(got, want) {
					t.Fatal("другая запись UUID изменила разобранное событие")
				}
			})
		}
	}
}

func TestEventAbsentMode(t *testing.T) {
	for _, kind := range []string{"login_started", "login_succeeded", "login_failed", "login_rejected"} {
		for _, raw := range []string{"", "null", `""`} {
			t.Run(kind+"/mode="+raw, func(t *testing.T) {
				body := eventBody(eventID, kind, "mis", wireTime, "")
				want := decodeEvent(t, body)
				got := decodeEvent(t, replaceField(body, "mode", raw))
				if !reflect.DeepEqual(got, want) {
					t.Fatal("пустой mode изменил разобранное событие")
				}
			})
		}
	}
}

func TestEventEscapedNamesAndLeapDate(t *testing.T) {
	now := time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC)
	body := strings.Replace(startedBody(), wireTime, "2024-02-29T12:00:00.000Z", 1)
	body = strings.Replace(body, `"type"`, `"\u0074ype"`, 1)
	got, err := factory.DecodeEvent(strings.NewReader(body), now)
	if err != nil {
		t.Fatalf("разбор события с високосной датой: %v", err)
	}
	if !got.OccurredAt.Equal(now) || got.Type != event.LoginStarted {
		t.Fatalf("неверное преобразование события: %+v", got)
	}
}

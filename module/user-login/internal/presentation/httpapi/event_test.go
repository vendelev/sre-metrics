package httpapi_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/memory"
)

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

func TestHandlerInvalidJSON(t *testing.T) {
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

func TestHandlerFieldSchema(t *testing.T) {
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
	f := newFixture(t, memory.DefaultLimits())
	post(t, f, startedBody(), 204, "")
	closed := eventBody(uuid.MustParse("019aabcd-0000-7000-8000-000000000002"), "login_succeeded", "mis", wireTime, "")
	closed = strings.Replace(closed, scenarioID.String(), "019aabcd-0000-7000-8000-000000000200", 1)
	post(t, f, closed, 204, "")
	// Одна запись ждёт expiry, другая удаления по retention; worker не запускался.
	f.clock.now = f.clock.now.Add(16 * time.Minute)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := capture(t, f)
			calls := f.calls
			post(t, f, tc.body, 400, "invalid_event")
			if f.calls != calls {
				t.Fatal("структурный отказ вызвал Process")
			}
			if !reflect.DeepEqual(before, capture(t, f)) {
				t.Fatal("структурный отказ изменил сценарии, события или агрегаты; запущено обслуживание")
			}
		})
	}
}

func TestHandlerJSONDecoding(t *testing.T) {
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
			f := newFixture(t, memory.DefaultLimits())
			post(t, f, valid, 204, "")
			before := capture(t, f)
			post(t, f, tc.body, 204, "")
			if !reflect.DeepEqual(before, capture(t, f)) {
				t.Fatal("эквивалентный JSON изменил принятый факт")
			}
		})
	}
}

func TestHandlerUUIDRepresentations(t *testing.T) {
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
				f := newFixture(t, memory.DefaultLimits())
				body := replaceField(startedBody(), field, fmt.Sprintf("%q", id))
				post(t, f, body, 204, "")
				before := capture(t, f)
				canonical := uuid.MustParse(id).String()
				post(t, f, replaceField(body, field, fmt.Sprintf("%q", canonical)), 204, "")
				if !reflect.DeepEqual(before, capture(t, f)) {
					t.Fatal("другая запись UUID изменила принятый факт")
				}
			})
		}
	}
}

func TestHandlerAbsentMode(t *testing.T) {
	for _, kind := range []string{"login_started", "login_succeeded", "login_failed", "login_rejected"} {
		for _, raw := range []string{"", "null", `""`} {
			t.Run(kind+"/mode="+raw, func(t *testing.T) {
				f := newFixture(t, memory.DefaultLimits())
				body := eventBody(eventID, kind, "mis", wireTime, "")
				post(t, f, body, 204, "")
				before := capture(t, f)
				post(t, f, replaceField(body, "mode", raw), 204, "")
				if !reflect.DeepEqual(before, capture(t, f)) {
					t.Fatal("пустой mode изменил принятый факт")
				}
			})
		}
	}
}

func TestHandlerEscapedNamesAndLeapDate(t *testing.T) {
	f := newFixture(t, memory.DefaultLimits())
	f.clock.now = time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC)
	body := strings.Replace(startedBody(), wireTime, "2024-02-29T12:00:00.000Z", 1)
	body = strings.Replace(body, `"type"`, `"\u0074ype"`, 1)
	post(t, f, body, 204, "")
}

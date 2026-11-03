package httpapi_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vendelev/sre-metrics/service/user-auth/internal/infrastructure/memory"
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

func TestHandlerStrictJSON(t *testing.T) {
	valid := startedBody()
	cases := []struct{ name, body string }{
		{"пусто", ""}, {"пробелы", " \n\t"}, {"null", "null"},
		{"массив", "[" + valid + "]"}, {"строка", `"строка"`}, {"число", "1"}, {"bool", "true"},
		{"объект без полей", "{}"}, {"оборванный объект", valid[:len(valid)-1]},
		{"ключ без значения", `{"version":}`}, {"ключ без двоеточия", `{"version" 1}`},
		{"незакрытая строка ключа", `{"version":1,"type`},
		{"лишняя запятая", valid[:len(valid)-1] + ",}"},
		{"комментарий", valid + "// комментарий"},
		{"второй объект", valid + "{}"}, {"второй скаляр", valid + "null"},
		{"хвост", valid + " мусор"},
		{"повтор ключа", valid[:len(valid)-1] + `,"type":"login_started"}`},
		{"повтор escaped ключа", valid[:len(valid)-1] + `,"\u0074ype":"login_started"}`},
		{"неизвестный ключ", valid[:len(valid)-1] + `,"secret":"синтетический секрет"}`},
		{"регистр ключа", strings.Replace(valid, `"type"`, `"Type"`, 1)},
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
	for _, field := range []string{"version", "event_id", "scenario_id", "occurred_at", "source", "type", "mode"} {
		base := valid
		if field == "mode" {
			base = modeBody
		}
		for _, raw := range []string{"", "null", "true", "[]", "{}"} {
			cases = append(cases, struct{ name, body string }{field + "/" + raw, replaceField(base, field, raw)})
		}
		if field != "version" {
			cases = append(cases, struct{ name, body string }{field + "/число", replaceField(base, field, "1")})
		}
	}
	for _, raw := range []string{`"1"`, "0", "2", "-1", "1.1", "1.00000000000000000000000001", "0.99999999999999999999999999", "1e1000000000", "1e-1000000000", "10e0", "1e" + strings.Repeat("9", 1000)} {
		cases = append(cases, struct{ name, body string }{"version/" + raw, replaceField(valid, "version", raw)})
	}
	for _, field := range []string{"event_id", "scenario_id"} {
		for _, id := range []string{
			"", "019aabcd-0000-4000-8000-000000000001", "019aabcd-0000-7000-7000-000000000001",
			"019aabcd-0000-7000-c000-000000000001", "019aabcd-0000-7000-8000-00000000000g",
			"019aabcd000070008000000000000001", "{019aabcd-0000-7000-8000-000000000001}",
			"urn:uuid:" + eventID, "019aabcdx0000-7000-8000-000000000001",
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
		"2026-11-02T12:00:00,000Z", "2026-1-02T12:00:00.000Z", "",
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
		for _, mode := range []string{"null", `"credentials"`} {
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
	closed := eventBody("019aabcd-0000-7000-8000-000000000002", "login_succeeded", "mis", wireTime, "")
	closed = strings.Replace(closed, scenarioID, "019aabcd-0000-7000-8000-000000000200", 1)
	post(t, f, closed, 204, "")
	// Одна запись ждёт expiry, другая удаления по retention; worker не запускался.
	f.clock.now = f.clock.now.Add(16 * time.Minute)
	before := capture(t, f)
	calls := f.calls
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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

func TestHandlerVersionNumberSemantics(t *testing.T) {
	for _, version := range []string{"1", "1.0", "1.000", "1e0", "1E+0", "10e-1", "0.1e1", "1000e-3", "0.00100e+3", "1e-0000", "1e+" + strings.Repeat("0", 1000)} {
		t.Run(version, func(t *testing.T) {
			f := newFixture(t, memory.DefaultLimits())
			post(t, f, replaceField(startedBody(), "version", version), 204, "")
		})
	}
}

func TestHandlerEscapedNamesAndLeapDate(t *testing.T) {
	f := newFixture(t, memory.DefaultLimits())
	f.clock.now = time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC)
	body := strings.Replace(startedBody(), wireTime, "2024-02-29T12:00:00.000Z", 1)
	body = strings.Replace(body, `"type"`, `"\u0074ype"`, 1)
	post(t, f, body, 204, "")
}

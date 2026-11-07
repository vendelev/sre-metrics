package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vendelev/sre-metrics/user-login/internal/domain/contract"
	"github.com/vendelev/sre-metrics/user-login/internal/domain/event"
	"github.com/vendelev/sre-metrics/user-login/internal/domain/scenario"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/memory"
	"github.com/vendelev/sre-metrics/user-login/internal/presentation/httpapi"
)

func TestHandlerDomainResults(t *testing.T) {
	t.Run("конфликт ID и факта", func(t *testing.T) {
		f := newFixture(t, memory.DefaultLimits())
		post(t, f, startedBody(), 204, "")
		before := capture(t, f)
		bodies := []string{
			strings.Replace(startedBody(), wireTime, "2026-11-02T12:00:01.000Z", 1),
			strings.Replace(startedBody(), scenarioID.String(), "019aabcd-0000-7000-8000-000000000200", 1),
			eventBody(
				uuid.MustParse("019aabcd-0000-7000-8000-000000000002"),
				"login_started",
				"mis",
				"2026-11-02T12:00:01.000Z",
				"",
			),
		}
		for _, body := range bodies {
			post(t, f, body, 409, "event_conflict")
			if !reflect.DeepEqual(before, capture(t, f)) {
				t.Fatal("конфликт изменил состояние")
			}
		}
	})
	t.Run("вместимость новой записи", func(t *testing.T) {
		f := newFixture(t, memory.Limits{MaxScenarios: 1, MaxEventsPerScenario: 32})
		post(t, f, startedBody(), 204, "")
		before := capture(t, f)
		body := eventBody(uuid.MustParse("019aabcd-0000-7000-8000-000000000002"), "login_started", "mis", wireTime, "")
		body = strings.Replace(body, scenarioID.String(), "019aabcd-0000-7000-8000-000000000200", 1)
		post(t, f, body, 503, "capacity_exceeded")
		if !reflect.DeepEqual(before, capture(t, f)) {
			t.Fatal("503 при создании записи изменил состояние")
		}
		post(t, f, startedBody(), 204, "")
	})
	t.Run("expiry с 410", func(t *testing.T) {
		f := newFixture(t, memory.DefaultLimits())
		post(t, f, startedBody(), 204, "")
		f.clock.now = f.clock.now.Add(15 * time.Minute)
		for _, kind := range []string{"login_succeeded", "login_failed", "login_rejected"} {
			post(
				t,
				f,
				eventBody(uuid.MustParse("019aabcd-0000-7000-8000-000000000002"), kind, "mis", wireTime, ""),
				410,
				"scenario_expired",
			)
		}
		snapshot := capture(t, f)
		if snapshot.aggregates.Completed[scenario.OutcomeUnknown] != 1 || len(snapshot.events) != 1 {
			t.Fatalf("expiry не сохранён однократно или сохранено отклонённое событие: %+v", snapshot)
		}
	})
	t.Run("expiry с 503 BR-55 AC-47", func(t *testing.T) {
		f := newFixture(t, memory.DefaultLimits())
		for i := 1; i <= 32; i++ {
			id := uuid.MustParse(fmt.Sprintf("019aabcd-0000-7000-8000-%012d", i))
			post(t, f, eventBody(id, "login_mode_determined", "auth", wireTime, "credentials"), 204, "")
		}
		f.clock.now = f.clock.now.Add(15 * time.Minute)
		before := capture(t, f)
		// Точный повтор завершается до проверки expiry даже при полном лимите.
		post(t, f, eventBody(eventID, "login_mode_determined", "auth", wireTime, "credentials"), 204, "")
		if !reflect.DeepEqual(before, capture(t, f)) {
			t.Fatal("точный повтор запустил expiry")
		}
		body := eventBody(
			uuid.MustParse("019aabcd-0000-7000-8000-000000000033"),
			"login_mode_determined",
			"auth",
			wireTime,
			"credentials",
		)
		post(t, f, body, 503, "capacity_exceeded")
		after := capture(t, f)
		if after.aggregates.Completed[scenario.OutcomeUnknown] != 1 || len(after.events) != 32 {
			t.Fatal("expiry при 503 не сохранён либо принят новый ID")
		}
		post(t, f, body, 503, "capacity_exceeded")
		if !reflect.DeepEqual(after, capture(t, f)) {
			t.Fatal("повторный 503 повторил измерение unknown")
		}
	})
}

func TestHandlerEventAgeBoundary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		delta  time.Duration
		status int
	}{
		{"ровно 30 минут назад", -30 * time.Minute, 204},
		{"старше на миллисекунду", -30*time.Minute - time.Millisecond, 400},
		{"ровно 30 секунд вперёд", 30 * time.Second, 204},
		{"дальше на миллисекунду", 30*time.Second + time.Millisecond, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, memory.DefaultLimits())
			at := f.clock.now.Add(tc.delta).Format("2006-01-02T15:04:05.000Z")
			code := ""
			if tc.status == 400 {
				code = "invalid_event"
			}
			// Исход без начала не закрывается немедленно как старое начало.
			post(t, f, eventBody(eventID, "login_succeeded", "mis", at, ""), tc.status, code)
			if tc.status == 400 && len(capture(t, f).scenarios) != 0 {
				t.Fatal("отказ по возрасту создал сценарий")
			}
		})
	}
}

func TestHandlerRetentionAndOldDuplicate(t *testing.T) {
	t.Run("сохранённый ID до возраста", func(t *testing.T) {
		f := newFixture(t, memory.DefaultLimits())
		// Успех ещё хранится, хотя его исходная метка уже почти на границе возраста.
		body := eventBody(eventID, "login_succeeded", "mis", "2026-11-02T11:31:00.000Z", "")
		post(t, f, body, 204, "")
		f.clock.now = f.clock.now.Add(2 * time.Minute)
		before := capture(t, f)
		post(t, f, body, 204, "")
		post(t, f, strings.Replace(body, "11:31:00", "11:31:01", 1), 409, "event_conflict")
		if !reflect.DeepEqual(before, capture(t, f)) {
			t.Fatal("старый повтор изменил состояние")
		}
	})
	for _, delta := range []time.Duration{15*time.Minute - time.Millisecond, 15 * time.Minute, 15*time.Minute + time.Millisecond} {
		t.Run(delta.String(), func(t *testing.T) {
			f := newFixture(t, memory.Limits{MaxScenarios: 1, MaxEventsPerScenario: 32})
			post(t, f, startedBody(), 204, "")
			post(
				t,
				f,
				eventBody(
					uuid.MustParse("019aabcd-0000-7000-8000-000000000002"),
					"login_succeeded",
					"mis",
					wireTime,
					"",
				),
				204,
				"",
			)
			f.clock.now = f.clock.now.Add(delta)
			post(
				t,
				f,
				eventBody(
					uuid.MustParse("019aabcd-0000-7000-8000-000000000003"),
					"login_mode_determined",
					"auth",
					wireTime,
					"credentials",
				),
				204,
				"",
			)
			a := capture(t, f)
			wantCount, wantEvents := uint64(0), 1
			if delta < 15*time.Minute {
				wantCount, wantEvents = 1, 3
			}
			if a.aggregates.Completed[scenario.OutcomeSuccess] != 1 ||
				a.aggregates.Durations[event.LoginModeCredentials].Count != wantCount || len(a.events) != wantEvents {
				t.Fatalf("неверная граница retention: %+v", a)
			}
		})
	}
}

// snapshotLogHandler проверяет возможность повторно войти в хранилище во время лога.
type snapshotLogHandler struct {
	slog.Handler
	checkStore func() error
	err        error
}

func (h *snapshotLogHandler) Handle(ctx context.Context, record slog.Record) error {
	h.err = h.checkStore()
	return h.Handler.Handle(ctx, record)
}

func TestHandlerNegativeDurationDiagnostic(t *testing.T) {
	for _, successFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("успех первым=%t", successFirst), func(t *testing.T) {
			f := newFixture(t, memory.DefaultLimits())
			logHandler := &snapshotLogHandler{
				Handler:    slog.NewJSONHandler(&f.logs, nil),
				checkStore: func() error { return f.store.Within(func(contract.State) error { return nil }) },
			}
			var err error
			f.handler, err = httpapi.NewHandler(f.observer.Process, slog.New(logHandler), httpapi.DefaultSettings())
			if err != nil {
				t.Fatal(err)
			}
			start := eventBody(eventID, "login_started", "mis", "2026-11-02T12:00:05.000Z", "")
			success := eventBody(
				uuid.MustParse("019aabcd-0000-7000-8000-000000000002"),
				"login_succeeded",
				"mis",
				"2026-11-02T12:00:03.000Z",
				"",
			)
			bodies := []string{start, success}
			if successFirst {
				bodies = []string{success, start}
			}
			post(
				t,
				f,
				eventBody(
					uuid.MustParse("019aabcd-0000-7000-8000-000000000003"),
					"login_mode_determined",
					"auth",
					wireTime,
					"credentials",
				),
				204,
				"",
			)
			post(t, f, bodies[0], 204, "")
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- request(f.handler, "POST", "application/json", bodies[1]) }()
			select {
			case w := <-done:
				assertResponse(t, w, 204, "")
			case <-time.After(2 * time.Second):
				t.Fatal("логирование удерживает защищённую операцию")
			}
			if logHandler.err != nil {
				t.Fatal(logHandler.err)
			}
			var entry map[string]any
			if err := json.Unmarshal(f.logs.Bytes(), &entry); err != nil {
				t.Fatal(err)
			}
			if len(entry) != 4 || entry["scenario_id"] != scenarioID.String() || entry["level"] != "WARN" ||
				!strings.Contains(fmt.Sprint(entry["msg"]), "раньше начала") {
				t.Fatalf("неверный диагностический лог: %s", f.logs.String())
			}
			for _, forbidden := range []string{"event_id", "occurred_at", "credentials", eventID.String(), "login_succeeded"} {
				if strings.Contains(f.logs.String(), forbidden) {
					t.Fatalf("диагностика раскрыла событие: %s", f.logs.String())
				}
			}
			before := capture(t, f)
			if before.aggregates.Completed[scenario.OutcomeSuccess] != 1 ||
				before.aggregates.Durations[event.LoginModeCredentials].Count != 0 {
				t.Fatal("диагностика изменила учёт успеха или длительности")
			}
			logBefore := f.logs.String()
			post(t, f, bodies[1], 204, "")
			if f.logs.String() != logBefore || !reflect.DeepEqual(before, capture(t, f)) {
				t.Fatal("повтор изменил агрегаты или повторил диагностику")
			}
		})
	}
}

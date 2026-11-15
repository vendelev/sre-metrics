package controller_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vendelev/sre-metrics/pkg/clock"
	dError "github.com/vendelev/sre-metrics/user-login/internal/domain/error"
	"github.com/vendelev/sre-metrics/user-login/internal/domain/event"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/queue"
	"github.com/vendelev/sre-metrics/user-login/internal/presentation/http/controller"
)

var (
	scenarioID = uuid.MustParse("019aabcd-0000-7000-8000-000000000100")
	eventID    = uuid.MustParse("019aabcd-0000-7000-8000-000000000001")
	wireTime   = "2026-11-02T12:00:00.000Z"
)

type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time { return c.now }

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
}

// fixture наблюдает передачу разобранных событий на рабочей границе accept.
// Реальные очередь, обработка и SDK проверяются вместе в тестах сервиса и bootstrap.
type fixture struct {
	controller *controller.LoginEvent
	accepted   []event.Request
	logs       bytes.Buffer
	calls      int
	clock      *testClock
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{clock: newTestClock()}
	var err error
	f.controller, err = controller.NewLoginEvent(func(e *event.Request) error {
		f.calls++
		f.accepted = append(f.accepted, e.Clone())
		return nil
	}, slog.New(slog.NewJSONHandler(&f.logs, nil)), f.clock)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func eventBody(id uuid.UUID, kind, source, at, mode string) string {
	body := fmt.Sprintf(`{"event_id":%q,"scenario_id":%q,"occurred_at":%q,"source":%q,"type":%q`,
		id.String(), scenarioID, at, source, kind)
	if mode != "" {
		body += fmt.Sprintf(`,"mode":%q`, mode)
	}
	return body + "}"
}

func startedBody() string { return eventBody(eventID, "login_started", "mis", wireTime, "") }

func request(handler http.HandlerFunc, method, mediaType, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/metrics/b/user-login/events", strings.NewReader(body))
	if mediaType != "" {
		r.Header.Set("Content-Type", mediaType)
	}
	w := httptest.NewRecorder()
	handler(w, r)
	return w
}

func assertResponse(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("статус %d, ожидался %d; тело %s", w.Code, status, w.Body.String())
	}
	if status == http.StatusAccepted {
		if w.Body.Len() != 0 {
			t.Fatalf("202 содержит тело: %s", w.Body.String())
		}
		return
	}
	if w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("неверный Content-Type: %v", w.Header())
	}
	var fields map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 1 || fields["error"] != code {
		t.Fatalf("неверная или небезопасная ошибка: %s", w.Body.String())
	}
}

func post(t *testing.T, f *fixture, body string, status int, code string) {
	t.Helper()
	assertResponse(t, request(f.controller.AcceptEvent, "POST", "application/json", body), status, code)
}

func TestNewController(t *testing.T) {
	accept := func(*event.Request) error { return nil }
	logger := slog.New(slog.DiscardHandler)
	for _, tc := range []struct {
		name   string
		accept func(*event.Request) error
		logger *slog.Logger
		clock  clock.Clock
	}{
		{"нет операции", nil, logger, newTestClock()},
		{"нет логгера", accept, nil, newTestClock()},
		{"нет часов", accept, logger, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := controller.NewLoginEvent(tc.accept, tc.logger, tc.clock); err == nil {
				t.Fatal("невалидная зависимость принята")
			}
		})
	}
	f := newFixture(t)
	post(t, f, startedBody(), 202, "")
}

// BR-57: 503 overloaded означает только полную очередь, не остановленный приём.
func TestControllerClosedQueue(t *testing.T) {
	q := queue.NewEventQueue(1)
	q.Close()
	var logs bytes.Buffer
	c, err := controller.NewLoginEvent(q.Enqueue, slog.New(slog.NewJSONHandler(&logs, nil)), newTestClock())
	if err != nil {
		t.Fatal(err)
	}
	assertResponse(t, request(c.AcceptEvent, "POST", "application/json", startedBody()), 500, "internal_error")
	if len(q.Events()) != 0 {
		t.Fatal("закрытая очередь приняла событие")
	}
	if !strings.Contains(logs.String(), queue.ErrClosed.Error()) {
		t.Fatal("отказ закрытой очереди не диагностирован")
	}
}

func TestControllerAcceptsEventTypes(t *testing.T) {
	for _, tc := range []struct{ kind, source, mode string }{
		{"login_started", "mis", ""}, {"login_succeeded", "mis", ""},
		{"login_mode_determined", "auth", "credentials"}, {"login_mode_determined", "auth", "existing_session"},
		{"login_failed", "mis", ""}, {"login_failed", "auth", ""},
		{"login_rejected", "mis", ""}, {"login_rejected", "auth", ""},
	} {
		t.Run(tc.kind+"/"+tc.source+"/"+tc.mode, func(t *testing.T) {
			f := newFixture(t)
			post(t, f, eventBody(eventID, tc.kind, tc.source, wireTime, tc.mode), 202, "")
			if len(f.accepted) != 1 {
				t.Fatal("событие не передано в приём")
			}
			e := f.accepted[0]
			if e.EventID != eventID || e.ScenarioID != scenarioID || string(e.Type) != tc.kind ||
				string(e.Source) != tc.source || e.OccurredAt.Format("2006-01-02T15:04:05.000Z") != wireTime {
				t.Fatalf("неверное преобразование события: %+v", e)
			}
			if (e.AuthMode == nil) != (tc.mode == "") || (e.AuthMode != nil && string(*e.AuthMode) != tc.mode) {
				t.Fatal("неверное преобразование mode")
			}
		})
	}
}

// Контроллер отвечает согласованно на любой метод, переданный ему владельцем
// маршрутизации; сами правила регистрации проверяет bootstrap.
func TestControllerMethodNotAllowed(t *testing.T) {
	for _, method := range []string{"GET", "HEAD", "PUT", "PATCH", "DELETE", "OPTIONS", "TRACE", "CONNECT", "OTHER"} {
		t.Run(method, func(t *testing.T) {
			f := newFixture(t)
			w := request(f.controller.MethodNotAllowed, method, "text/plain", strings.Repeat("!", 4097))
			if w.Header().Get("Allow") != "POST" {
				t.Fatal("нет Allow: POST")
			}
			if method == "HEAD" {
				if w.Code != 405 || w.Body.Len() != 0 || w.Header().Get("Content-Type") != "application/json" {
					t.Fatalf("HEAD: %d %q", w.Code, w.Body.String())
				}
			} else {
				assertResponse(t, w, 405, "method_not_allowed")
			}
			if f.calls != 0 {
				t.Fatal("неподдерживаемый метод вызвал приём")
			}
		})
	}
}

func TestControllerMediaTypes(t *testing.T) {
	for _, mediaType := range []string{"", "text/plain", "application/jsonx", "application/problem+json", "application/json; charset", "application/json; charset=utf-8; charset=ascii"} {
		t.Run("отказ MIME/"+mediaType, func(t *testing.T) {
			f := newFixture(t)
			assertResponse(t, request(f.controller.AcceptEvent, "POST", mediaType, "!"), 415, "unsupported_media_type")
			if f.calls != 0 {
				t.Fatal("отказ MIME вызвал приём")
			}
		})
	}
	for _, mediaType := range []string{"application/json", "Application/JSON", "application/json;", "application/json; charset=utf-8", `application/json; charset="utf-8"; profile="test"`} {
		t.Run("приём MIME/"+mediaType, func(t *testing.T) {
			f := newFixture(t)
			assertResponse(t, request(f.controller.AcceptEvent, "POST", mediaType, startedBody()), 202, "")
		})
	}
}

func TestControllerLargeBody(t *testing.T) {
	for _, unknownLength := range []bool{false, true} {
		for _, size := range []int{4097, 65536} {
			t.Run(fmt.Sprintf("байт=%d/неизвестная длина=%t", size, unknownLength), func(t *testing.T) {
				f := newFixture(t)
				body := startedBody() + strings.Repeat(" ", size-len(startedBody()))
				r := httptest.NewRequest("POST", "/metrics/b/user-login/events", strings.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				if unknownLength {
					r.ContentLength = -1
				}
				w := httptest.NewRecorder()
				f.controller.AcceptEvent(w, r)
				assertResponse(t, w, 202, "")
				if f.calls != 1 {
					t.Fatal("большое тело не передано в приём")
				}
			})
		}
	}
	for _, tc := range []struct{ name, body string }{
		{"большой невалидный JSON", strings.Repeat("!", 65536)},
		{"большой хвост", startedBody() + strings.Repeat("!", 65536)},
		{"второй объект после пробелов", startedBody() + strings.Repeat(" ", 65536) + `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			post(t, f, tc.body, 400, "invalid_event")
			if f.calls != 0 {
				t.Fatal("структурный отказ вызвал приём")
			}
		})
	}
}

type failedReader struct{}

func (failedReader) Read([]byte) (int, error) {
	return 0, errors.New("ошибка чтения синтетического тела")
}

func TestControllerBodyReadFailure(t *testing.T) {
	f := newFixture(t)
	r := httptest.NewRequest("POST", "/metrics/b/user-login/events", failedReader{})
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.controller.AcceptEvent(w, r)
	assertResponse(t, w, 400, "invalid_event")
	if f.calls != 0 {
		t.Fatal("ошибка чтения вызвала приём")
	}
}

func TestControllerAcceptErrorsAndLogging(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"успех", nil, 202, ""},
		{"недопустимое событие", dError.ErrInvalidEvent, 400, "invalid_event"},
		{"переполнение", dError.ErrOverloaded, 503, "overloaded"},
		{"доменный лимит не является переполнением очереди", dError.ErrCapacityExceeded, 500, "internal_error"},
		{"внутренняя", errors.New("синтетический сбой приёма"), 500, "internal_error"},
	} {
		for _, wrapped := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/обёртка=%t", tc.name, wrapped), func(t *testing.T) {
				cause := tc.err
				if wrapped && cause != nil {
					cause = fmt.Errorf("контекст: %w", cause)
				}
				var logs bytes.Buffer
				c, err := controller.NewLoginEvent(
					func(*event.Request) error { return cause },
					slog.New(slog.NewJSONHandler(&logs, nil)),
					newTestClock(),
				)
				if err != nil {
					t.Fatal(err)
				}
				w := request(c.AcceptEvent, "POST", "application/json", startedBody())
				assertResponse(t, w, tc.status, tc.code)
				if tc.status != 500 {
					if logs.Len() != 0 {
						t.Fatalf("штатный результат создал лог: %s", logs.String())
					}
					return
				}
				var record map[string]any
				if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
					t.Fatal(err)
				}
				if record["level"] != "ERROR" || record["error"] != cause.Error() {
					t.Fatalf("неверная диагностика: %s", logs.String())
				}
				if strings.Contains(w.Body.String(), cause.Error()) ||
					strings.Contains(logs.String(), eventID.String()) ||
					strings.Contains(logs.String(), scenarioID.String()) {
					t.Fatal("раскрыты детали ошибки или входящего события")
				}
			})
		}
	}
}

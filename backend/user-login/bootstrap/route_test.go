package bootstrap

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/queue"
)

var (
	routeScenarioID = uuid.MustParse("019aabcd-0000-7000-8000-000000000100")
	routeEventID    = uuid.MustParse("019aabcd-0000-7000-8000-000000000001")
	routeWireTime   = "2026-11-02T12:00:00.000Z"
)

func routeStartedBody() string {
	return `{"event_id":"` + routeEventID.String() + `","scenario_id":"` + routeScenarioID.String() +
		`","occurred_at":"` + routeWireTime + `","source":"mis","type":"login_started"}`
}

// routeFixture собирает рабочую маршрутизацию с настоящей очередью приёма.
func routeFixture(t *testing.T) (http.Handler, *queue.EventQueue, *bytes.Buffer) {
	t.Helper()
	q := queue.NewEventQueue(2)
	logs := &bytes.Buffer{}
	handler, err := routeOf(q, slog.New(slog.NewJSONHandler(logs, nil)), &testClock{
		now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler, q, logs
}

func routeRequest(handler http.Handler, method, mediaType, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/events", strings.NewReader(body))
	if mediaType != "" {
		r.Header.Set("Content-Type", mediaType)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func assertRouteResponse(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
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
	var fields map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 1 || fields["error"] != code {
		t.Fatalf("неверная ошибка маршрутизации: %s", w.Body.String())
	}
}

func TestRouteRegistration(t *testing.T) {
	t.Parallel()
	handler, _, _ := routeFixture(t)
	for _, tc := range []struct {
		path   string
		status int
	}{
		{"/events", 202}, {"/events?test=1", 202}, {"/", 404}, {"/other", 404},
		{"/events/", 404}, {"/events/extra", 404}, {"/Events", 404}, {"//events", 404}, {"/other/../events", 404},
	} {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			w := routeRequestAt(handler, tc.path)
			if w.Code != tc.status || w.Header().Get("Location") != "" {
				t.Fatalf("путь %s: статус %d, заголовки %v", tc.path, w.Code, w.Header())
			}
			if tc.status == http.StatusAccepted {
				assertRouteResponse(t, w, 202, "")
			}
		})
	}
}

func routeRequestAt(handler http.Handler, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(routeStartedBody()))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

// Bootstrap регистрирует POST /events и передаёт неподдерживаемые методы
// контроллеру; транспортные правила самого контроллера проверяет его пакет.
func TestRouteMethods(t *testing.T) {
	handler, q, _ := routeFixture(t)
	for _, method := range []string{"GET", "HEAD", "PUT", "PATCH", "DELETE", "OPTIONS", "TRACE", "CONNECT", "OTHER"} {
		t.Run(method, func(t *testing.T) {
			w := routeRequest(handler, method, "text/plain", strings.Repeat("!", 4097))
			if w.Header().Get("Allow") != "POST" {
				t.Fatal("нет Allow: POST")
			}
			if method == http.MethodHead {
				if w.Code != 405 || w.Body.Len() != 0 || w.Header().Get("Content-Type") != "application/json" {
					t.Fatalf("HEAD: %d %q", w.Code, w.Body.String())
				}
				return
			}
			assertRouteResponse(t, w, 405, "method_not_allowed")
		})
	}
	assertRouteResponse(t, routeRequest(handler, "POST", "text/plain", "!"), 415, "unsupported_media_type")
	assertRouteResponse(t, routeRequest(handler, "POST", "application/json", routeStartedBody()), 202, "")
	if len(q.Events()) != 1 {
		t.Fatalf("маршрут не передал событие в очередь: %d", len(q.Events()))
	}
}

// BR-57: 503 overloaded означает только полную очередь, не остановленный приём.
func TestRouteClosedQueue(t *testing.T) {
	q := queue.NewEventQueue(1)
	q.Close()
	var logs bytes.Buffer
	handler, err := routeOf(q, slog.New(slog.NewJSONHandler(&logs, nil)), &testClock{
		now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertRouteResponse(t, routeRequest(handler, "POST", "application/json", routeStartedBody()), 500, "internal_error")
	if len(q.Events()) != 0 {
		t.Fatal("закрытая очередь приняла событие")
	}
	if !strings.Contains(logs.String(), queue.ErrClosed.Error()) {
		t.Fatal("отказ закрытой очереди не диагностирован")
	}
}

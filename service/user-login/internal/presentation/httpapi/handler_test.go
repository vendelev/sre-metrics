package httpapi_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vendelev/sre-metrics/service/user-login/internal/application"
	"github.com/vendelev/sre-metrics/service/user-login/internal/domain"
	"github.com/vendelev/sre-metrics/service/user-login/internal/infrastructure/memory"
	"github.com/vendelev/sre-metrics/service/user-login/internal/infrastructure/otel"
	"github.com/vendelev/sre-metrics/service/user-login/internal/infrastructure/otel/oteltest"
	"github.com/vendelev/sre-metrics/service/user-login/internal/presentation/httpapi"
)

const (
	scenarioID = "019aabcd-0000-7000-8000-000000000100"
	eventID    = "019aabcd-0000-7000-8000-000000000001"
	wireTime   = "2026-11-02T12:00:00.000Z"
)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

type fixture struct {
	store    *memory.Store
	observer *application.LoginObserver
	clock    *clock
	handler  *httpapi.Handler
	logs     bytes.Buffer
	calls    int
	recorder *otel.Recorder
}

func newFixture(t *testing.T, limits memory.Limits) *fixture {
	t.Helper()
	f := &fixture{clock: &clock{now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}}
	var err error
	f.recorder = oteltest.New(t)
	f.store, err = memory.NewStore(limits, f.recorder)
	if err != nil {
		t.Fatal(err)
	}
	f.observer, err = application.NewLoginObserver(f.store, f.clock)
	if err != nil {
		t.Fatal(err)
	}
	f.handler, err = httpapi.NewHandler(func(event domain.Event) (application.ProcessResult, error) {
		f.calls++
		return f.observer.Process(event)
	}, slog.New(slog.NewJSONHandler(&f.logs, nil)), httpapi.DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestHandlerErrorLogging(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "внутренняя ошибка", err: errors.New("сбой хранилища"), status: 500, code: "internal_error"},
		{name: "конфликт", err: domain.ErrConflict, status: 409, code: "event_conflict"},
		{name: "лимит", err: domain.ErrCapacityExceeded, status: 503, code: "capacity_exceeded"},
		{name: "успех", status: 204},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil)).With("service", "user-login", "component", "http")
			handler, err := httpapi.NewHandler(
				func(domain.Event) (application.ProcessResult, error) {
					return application.ProcessResult{}, tt.err
				},
				logger,
				httpapi.DefaultSettings(),
			)
			if err != nil {
				t.Fatal(err)
			}
			response := request(handler, http.MethodPost, "application/json", startedBody())
			assertResponse(t, response, tt.status, tt.code)
			if tt.status != http.StatusInternalServerError {
				if logs.Len() != 0 {
					t.Fatalf("ожидаемый результат создал лог: %s", logs.String())
				}
				return
			}
			var record map[string]any
			if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
				t.Fatalf("ожидалась одна JSON-запись: %v", err)
			}
			for key, want := range map[string]string{
				"level": "ERROR", "service": "user-login", "component": "http", "error": tt.err.Error(),
			} {
				if record[key] != want {
					t.Errorf("поле %s: получено %v, ожидалось %s", key, record[key], want)
				}
			}
			if strings.Contains(logs.String(), eventID) || strings.Contains(logs.String(), scenarioID) {
				t.Fatal("лог содержит данные входящего события")
			}
			if strings.Contains(response.Body.String(), tt.err.Error()) {
				t.Fatal("HTTP-ответ раскрывает внутреннюю ошибку")
			}
		})
	}
}

func eventBody(id, kind, source, at, mode string) string {
	body := fmt.Sprintf(`{"version":1,"event_id":%q,"scenario_id":%q,"occurred_at":%q,"source":%q,"type":%q`,
		id, scenarioID, at, source, kind)
	if mode != "" {
		body += fmt.Sprintf(`,"mode":%q`, mode)
	}
	return body + "}"
}

func startedBody() string { return eventBody(eventID, "login_started", "mis", wireTime, "") }

func request(handler http.Handler, method, mediaType, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/events", strings.NewReader(body))
	if mediaType != "" {
		r.Header.Set("Content-Type", mediaType)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func assertResponse(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("статус %d, ожидался %d; тело %s", w.Code, status, w.Body.String())
	}
	if status == http.StatusNoContent {
		if w.Body.Len() != 0 {
			t.Fatalf("204 содержит тело: %s", w.Body.String())
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
	assertResponse(t, request(f.handler, "POST", "application/json", body), status, code)
}

func TestNewHandler(t *testing.T) {
	process := func(domain.Event) (application.ProcessResult, error) { return application.ProcessResult{}, nil }
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range []struct {
		name     string
		process  func(domain.Event) (application.ProcessResult, error)
		logger   *slog.Logger
		settings httpapi.Settings
	}{
		{"нет операции", nil, logger, httpapi.DefaultSettings()},
		{"нет логгера", process, nil, httpapi.DefaultSettings()},
		{"нулевой лимит", process, logger, httpapi.Settings{}},
		{"отрицательный лимит", process, logger, httpapi.Settings{MaxBodyBytes: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := httpapi.NewHandler(tc.process, tc.logger, tc.settings); err == nil {
				t.Fatal("невалидная зависимость или настройка принята")
			}
		})
	}
	if httpapi.DefaultSettings().MaxBodyBytes != 4096 {
		t.Fatal("начальный лимит отличается от BR-36")
	}
	h, err := httpapi.NewHandler(process, logger, httpapi.Settings{MaxBodyBytes: 2})
	if err != nil {
		t.Fatal(err)
	}
	assertResponse(t, request(h, "POST", "application/json", "{} "), 413, "payload_too_large")
}

func TestHandlerAcceptsEventTypes(t *testing.T) {
	for _, tc := range []struct{ kind, source, mode string }{
		{"login_started", "mis", ""}, {"login_succeeded", "mis", ""},
		{"login_mode_determined", "auth", "credentials"}, {"login_mode_determined", "auth", "existing_session"},
		{"login_failed", "mis", ""}, {"login_failed", "auth", ""},
		{"login_rejected", "mis", ""}, {"login_rejected", "auth", ""},
	} {
		t.Run(tc.kind+"/"+tc.source+"/"+tc.mode, func(t *testing.T) {
			f := newFixture(t, memory.DefaultLimits())
			post(t, f, eventBody(eventID, tc.kind, tc.source, wireTime, tc.mode), 204, "")
			if err := f.store.Within(func(s domain.State) error {
				e, ok := s.FindEvent(eventID)
				if !ok || e.Type != domain.EventType(tc.kind) || e.Source != domain.Source(tc.source) ||
					e.ScenarioID != scenarioID || !e.OccurredAt.Equal(f.clock.now) {
					t.Fatalf("событие не сохранено или преобразовано неверно: %+v", e)
				}
				if (e.Mode == nil) != (tc.mode == "") || (e.Mode != nil && string(*e.Mode) != tc.mode) {
					t.Fatal("неверное преобразование mode")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHandlerSuccessAndEquivalentDuplicates(t *testing.T) {
	f := newFixture(t, memory.DefaultLimits())
	post(t, f, startedBody(), 204, "")
	post(
		t,
		f,
		eventBody("019aabcd-0000-7000-8000-000000000002", "login_mode_determined", "auth", wireTime, "credentials"),
		204,
		"",
	)
	success := eventBody(
		"019aabcd-0000-7000-8000-000000000003",
		"login_succeeded",
		"mis",
		"2026-11-02T12:00:20.000Z",
		"",
	)
	post(t, f, success, 204, "")
	before := capture(t, f)
	variants := []string{
		" \n\t" + success + "\r\n",
		strings.ReplaceAll(success, "019aabcd", "019AABCD"),
		strings.ReplaceAll(success, "login_succeeded", `login_\u0073ucceeded`),
		`{"type":"login_succeeded","source":"mis","occurred_at":"2026-11-02T12:00:20.000Z","scenario_id":"` + scenarioID + `","event_id":"019aabcd-0000-7000-8000-000000000003","version":1.0}`,
	}
	for _, body := range variants {
		post(t, f, body, 204, "")
		if !reflect.DeepEqual(before, capture(t, f)) {
			t.Fatal("эквивалентный повтор изменил состояние")
		}
	}
	a := oteltest.Read(t, f.recorder)
	h := a.Durations[domain.LoginModeCredentials]
	if a.Completed[domain.OutcomeSuccess] != 1 || h.Count != 1 || h.SumSeconds != 20 {
		t.Fatalf("неверные агрегаты успешного пути: %+v", a)
	}
}

// capture читает состояние без обслуживания, включая сохранённые ID и агрегаты.
type stateSnapshot struct {
	scenarios  map[string]*domain.Scenario
	events     map[string]domain.Event
	aggregates oteltest.Values
}

func capture(t *testing.T, f *fixture) stateSnapshot {
	t.Helper()
	snapshot := stateSnapshot{scenarios: make(map[string]*domain.Scenario), events: make(map[string]domain.Event)}
	if err := f.store.Within(func(s domain.State) error {
		if err := s.VisitScenarios(func(scenario *domain.Scenario) error {
			snapshot.scenarios[scenario.ID()] = scenario
			return nil
		}); err != nil {
			return err
		}
		for i := 1; i <= 40; i++ {
			id := fmt.Sprintf("019aabcd-0000-7000-8000-%012d", i)
			if event, ok := s.FindEvent(id); ok {
				snapshot.events[id] = event
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	snapshot.aggregates = oteltest.Read(t, f.recorder)
	return snapshot
}

func TestHandlerMethodsAndMIME(t *testing.T) {
	for _, method := range []string{"GET", "HEAD", "PUT", "PATCH", "DELETE", "OPTIONS", "TRACE", "CONNECT", "OTHER"} {
		t.Run(method, func(t *testing.T) {
			f := newFixture(t, memory.DefaultLimits())
			for _, useMux := range []bool{false, true} {
				var h http.Handler = f.handler
				if useMux {
					mux := http.NewServeMux()
					mux.Handle("/events", h)
					h = mux
				}
				w := request(h, method, "text/plain", strings.Repeat("!", 4097))
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
			}
			if f.calls != 0 {
				t.Fatal("метод вызвал Process")
			}
		})
	}
	for _, mediaType := range []string{"", "text/plain", "application/jsonx", "application/problem+json", "application/json; charset", "application/json; charset=utf-8; charset=ascii"} {
		t.Run("отказ MIME/"+mediaType, func(t *testing.T) {
			f := newFixture(t, memory.DefaultLimits())
			// Невалидный JSON сверх лимита не должен опередить MIME.
			assertResponse(
				t,
				request(f.handler, "POST", mediaType, strings.Repeat("!", 4097)),
				415,
				"unsupported_media_type",
			)
			if f.calls != 0 {
				t.Fatal("MIME вызвал Process")
			}
		})
	}
	for _, mediaType := range []string{"application/json", "Application/JSON", "application/json;", "application/json; charset=utf-8", `application/json; charset="utf-8"; profile="test"`} {
		t.Run("приём MIME/"+mediaType, func(t *testing.T) {
			f := newFixture(t, memory.DefaultLimits())
			assertResponse(t, request(f.handler, "POST", mediaType, startedBody()), 204, "")
		})
	}
}

type countingReader struct {
	remaining int
	read      int
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.remaining)
	for i := range p[:n] {
		p[i] = '!'
	}
	r.remaining -= n
	r.read += n
	return n, nil
}

func TestHandlerBodyLimit(t *testing.T) {
	for _, unknownLength := range []bool{false, true} {
		for _, size := range []int{4096, 4097} {
			t.Run(fmt.Sprintf("байт=%d/неизвестная длина=%t", size, unknownLength), func(t *testing.T) {
				f := newFixture(t, memory.DefaultLimits())
				body := startedBody() + strings.Repeat(" ", size-len(startedBody()))
				r := httptest.NewRequest("POST", "/events", strings.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				if unknownLength {
					r.ContentLength = -1
				}
				w := httptest.NewRecorder()
				f.handler.ServeHTTP(w, r)
				if size == 4096 {
					assertResponse(t, w, 204, "")
				} else {
					assertResponse(t, w, 413, "payload_too_large")
					if f.calls != 0 || len(capture(t, f).scenarios) != 0 {
						t.Fatal("превышение изменило состояние")
					}
				}
			})
		}
	}
	for _, body := range []string{
		strings.Repeat("!", 4097), startedBody() + strings.Repeat("!", 4097),
		startedBody() + `{}` + strings.Repeat(" ", 4097),
		// Размер считается в байтах, хотя число Unicode-символов меньше лимита.
		`{"type":"` + strings.Repeat("я", 2048) + `"}`,
	} {
		f := newFixture(t, memory.DefaultLimits())
		post(t, f, body, 413, "payload_too_large")
		if f.calls != 0 {
			t.Fatal("превышение после объекта вызвало Process")
		}
	}
	f := newFixture(t, memory.DefaultLimits())
	reader := &countingReader{remaining: 1_000_000}
	r := httptest.NewRequest("POST", "/events", reader)
	r.Header.Set("Content-Type", "application/json")
	r.ContentLength = 1 // Недостоверный заголовок не заменяет проверку фактических байтов.
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	assertResponse(t, w, 413, "payload_too_large")
	if reader.read != 4097 {
		t.Fatalf("прочитано %d байт вместо ограниченных 4097", reader.read)
	}
}

type failedReader struct{}

func (failedReader) Read([]byte) (int, error) {
	return 0, errors.New("ошибка чтения синтетического тела")
}

func TestHandlerBodyReadFailure(t *testing.T) {
	f := newFixture(t, memory.DefaultLimits())
	r := httptest.NewRequest("POST", "/events", failedReader{})
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	assertResponse(t, w, 400, "invalid_event")
	if f.calls != 0 {
		t.Fatal("ошибка чтения вызвала Process")
	}
}

func TestHandlerProcessErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"схема", domain.ErrInvalidEvent, 400, "invalid_event"},
		{"конфликт", domain.ErrConflict, 409, "event_conflict"},
		{"срок", domain.ErrScenarioExpired, 410, "scenario_expired"},
		{"вместимость", domain.ErrCapacityExceeded, 503, "capacity_exceeded"},
		{"внутренняя", domain.ErrInternal, 500, "internal_error"},
		{"неожиданная", errors.New("секретный внутренний текст " + eventID), 500, "internal_error"},
	} {
		for _, wrapped := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/обёртка=%t", tc.name, wrapped), func(t *testing.T) {
				err := tc.err
				if wrapped {
					err = fmt.Errorf("контекст с секретом: %w", err)
				}
				h, createErr := httpapi.NewHandler(func(domain.Event) (application.ProcessResult, error) {
					return application.ProcessResult{}, err
				}, slog.New(slog.NewTextHandler(io.Discard, nil)), httpapi.DefaultSettings())
				if createErr != nil {
					t.Fatal(createErr)
				}
				assertResponse(t, request(h, "POST", "application/json", startedBody()), tc.status, tc.code)
			})
		}
	}
}

type failingStore struct{}

func (failingStore) Within(func(domain.State) error) error {
	return errors.New("внутренний сбой " + scenarioID)
}

func TestHandlerUnexpectedStoreError(t *testing.T) {
	f := newFixture(t, memory.DefaultLimits())
	o, err := application.NewLoginObserver(failingStore{}, f.clock)
	if err != nil {
		t.Fatal(err)
	}
	h, err := httpapi.NewHandler(o.Process, slog.New(slog.NewTextHandler(io.Discard, nil)), httpapi.DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	assertResponse(t, request(h, "POST", "application/json", startedBody()), 500, "internal_error")
}

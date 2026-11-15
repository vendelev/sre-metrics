package controller_test

import (
	"fmt"
	"log/slog"
	"testing"
	"time"

	dError "github.com/vendelev/sre-metrics/user-login/internal/domain/error"
	"github.com/vendelev/sre-metrics/user-login/internal/domain/event"
	"github.com/vendelev/sre-metrics/user-login/internal/presentation/http/controller"
)

// BR-51: транспортные отказы контроллера предшествуют вызову приёма,
// даже при полной очереди. Метод пути проверяет маршрутизация bootstrap.
func TestControllerTransportChecksBeforeAdmission(t *testing.T) {
	calls := 0
	c, err := controller.NewLoginEvent(func(*event.Request) error {
		calls++
		return dError.ErrOverloaded
	}, slog.New(slog.DiscardHandler), newTestClock())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, mediaType, body string
		status                int
		code                  string
	}{
		{"MIME", "text/plain", "!", 415, "unsupported_media_type"},
		{"JSON", "application/json", "!", 400, "invalid_event"},
		{"схема", "application/json", `{}`, 400, "invalid_event"},
		{"пустое тело", "application/json", "", 400, "invalid_event"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertResponse(t, request(c.AcceptEvent, "POST", tc.mediaType, tc.body), tc.status, tc.code)
			if calls != 0 {
				t.Fatal("транспортный отказ вызвал приём")
			}
		})
	}
	assertResponse(t, request(c.AcceptEvent, "POST", "application/json", startedBody()), 503, "overloaded")
	if calls != 1 {
		t.Fatal("допустимый запрос не передан в приём")
	}
}

// BR-57: контроллер не распознаёт повторы и передаёт каждое событие операции приёма.
func TestControllerForwardsDuplicates(t *testing.T) {
	f := newFixture(t)
	post(t, f, startedBody(), 202, "")
	post(t, f, startedBody(), 202, "")
	if f.calls != 2 || len(f.accepted) != 2 {
		t.Fatal("контроллер выполнил дедупликацию вместо передачи в приём")
	}
}

// BR-37, BR-51: возраст проверяется до accept, включая случай переполнения очереди.
func TestControllerAgeBeforeAdmission(t *testing.T) {
	for _, overloaded := range []bool{false, true} {
		for _, delta := range []time.Duration{
			-30*time.Minute - time.Millisecond, -30 * time.Minute, -30*time.Minute + time.Millisecond,
			30*time.Second - time.Millisecond, 30 * time.Second, 30*time.Second + time.Millisecond,
		} {
			t.Run(fmt.Sprintf("переполнение=%t/возраст=%s", overloaded, delta), func(t *testing.T) {
				clock := newTestClock()
				calls := 0
				c, err := controller.NewLoginEvent(func(*event.Request) error {
					calls++
					if overloaded {
						return dError.ErrOverloaded
					}
					return nil
				}, slog.New(slog.DiscardHandler), clock)
				if err != nil {
					t.Fatal(err)
				}
				status, code, wantCalls := 202, "", 1
				if delta < -30*time.Minute || delta > 30*time.Second {
					status, code, wantCalls = 400, "invalid_event", 0
				} else if overloaded {
					status, code = 503, "overloaded"
				}
				at := clock.Now().Add(delta).Format("2006-01-02T15:04:05.000Z")
				body := eventBody(eventID, "login_succeeded", "mis", at, "")
				assertResponse(t, request(c.AcceptEvent, "POST", "application/json", body), status, code)
				if calls != wantCalls {
					t.Fatalf("вызовы приёма: %d, ожидалось %d", calls, wantCalls)
				}
			})
		}
	}
}

// AC-39: повтор ранее переданного ID снова проходит проверку возраста.
func TestControllerRejectsOldDuplicateBeforeAdmission(t *testing.T) {
	f := newFixture(t)
	at := f.clock.Now().Add(-29 * time.Minute).Format("2006-01-02T15:04:05.000Z")
	body := eventBody(eventID, "login_succeeded", "mis", at, "")
	post(t, f, body, 202, "")
	f.clock.now = f.clock.Now().Add(2 * time.Minute)
	post(t, f, body, 400, "invalid_event")
	if f.calls != 1 || len(f.accepted) != 1 {
		t.Fatal("старый повтор передан в очередь")
	}
}

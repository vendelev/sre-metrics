package listener_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vendelev/sre-metrics/pkg/fileinbox"
	dError "github.com/vendelev/sre-metrics/user-login/internal/domain/error"
	dEvent "github.com/vendelev/sre-metrics/user-login/internal/domain/event"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/queue"
	"github.com/vendelev/sre-metrics/user-login/internal/presentation/listener"
)

const (
	eventID    = "019a0000-0000-7000-8000-000000000101"
	scenarioID = "019a0000-0000-7000-8000-000000000001"
)

func eventJSON(at time.Time) string {
	return fmt.Sprintf(`{
		"event_id": %q, "scenario_id": %q, "occurred_at": %q,
		"source": "mis", "type": "login_started"
	}`, eventID, scenarioID, at.Format("2006-01-02T15:04:05.000Z"))
}

// BR-57: полная очередь не принимает событие; после освобождения места приём возможен.
func TestNewHandlerWithQueue(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.October, 31, 12, 0, 0, 0, time.UTC)
	contents := []byte(eventJSON(now))
	q := queue.NewEventQueue(1)
	t.Cleanup(q.Close)
	handle := listener.NewInboxHandler(func(event *dEvent.Request) error {
		// Оборачиваем реальные ошибки очереди, чтобы проверить сопоставление по цепочке.
		if err := q.Enqueue(event); err != nil {
			return fmt.Errorf("приём события: %w", err)
		}
		return nil
	})

	if err := handle(contents, now); err != nil {
		t.Fatalf("первый приём: %v", err)
	}
	if err := handle(contents, now); !errors.Is(err, fileinbox.ErrRetry) {
		t.Fatalf("полная очередь: ожидался повтор, получено %v", err)
	}
	select {
	case got := <-q.Events():
		if got == nil || got.EventID.String() != eventID || got.ScenarioID.String() != scenarioID ||
			!got.OccurredAt.Equal(now) || got.Source != dEvent.SourceMIS || got.Type != dEvent.LoginStarted ||
			got.AuthMode != nil {
			t.Fatalf("в очередь передано неверное событие: %+v", got)
		}
	default:
		t.Fatal("успешный приём не поставил событие в очередь")
	}
	if err := handle(contents, now); err != nil {
		t.Fatalf("повтор после освобождения места: %v", err)
	}
	q.Close()
	if err := handle(contents, now); !errors.Is(err, fileinbox.ErrRetry) {
		t.Fatalf("закрытая очередь: ожидался повтор, получено %v", err)
	}
}

// BR-37, AC-26: возраст проверяется по переданным часам до постановки.
func TestNewHandlerAge(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.October, 31, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		delta  time.Duration
		accept bool
	}{
		{"старше нижней границы", -30*time.Minute - time.Millisecond, false},
		{"ровно нижняя граница", -30 * time.Minute, true},
		{"внутри у нижней границы", -30*time.Minute + time.Millisecond, true},
		{"внутри у верхней границы", 30*time.Second - time.Millisecond, true},
		{"ровно верхняя граница", 30 * time.Second, true},
		{"дальше верхней границы", 30*time.Second + time.Millisecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			called := false
			handle := listener.NewInboxHandler(func(*dEvent.Request) error {
				called = true
				return nil
			})
			err := handle([]byte(eventJSON(now.Add(tc.delta))), now)
			if tc.accept {
				if err != nil || !called {
					t.Fatalf("допустимое событие не принято: вызов=%t, ошибка=%v", called, err)
				}
				return
			}
			if err == nil || errors.Is(err, fileinbox.ErrRetry) || called {
				t.Fatalf("ожидался окончательный отказ до приёма: вызов=%t, ошибка=%v", called, err)
			}
		})
	}
}

// AC-16, AC-43: файловый вход применяет тот же разбор JSON и предметной схемы.
func TestNewHandlerRejectsInvalidContentBeforeEnqueue(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.October, 31, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, body string
	}{
		{"невалидный JSON", "{"},
		{"второй объект", eventJSON(now) + "{}"},
		{"источник не соответствует типу", strings.Replace(eventJSON(now), `"mis"`, `"auth"`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			handle := listener.NewInboxHandler(func(*dEvent.Request) error {
				t.Fatal("невалидное событие дошло до постановки")
				return nil
			})
			if err := handle([]byte(tc.body), now); err == nil || errors.Is(err, fileinbox.ErrRetry) {
				t.Fatalf("ожидался окончательный отказ, получено %v", err)
			}
		})
	}
}

func TestNewHandlerRejectsOtherEnqueueErrors(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.October, 31, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"внутренняя ошибка приёма", dError.ErrInternal},
		{"чужой сигнал повтора", fileinbox.ErrRetry},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Реальная очередь не возвращает эти ошибки; задаём их на границе операции.
			handle := listener.NewInboxHandler(func(*dEvent.Request) error { return tc.err })
			if err := handle([]byte(eventJSON(now)), now); err == nil || errors.Is(err, fileinbox.ErrRetry) {
				t.Fatalf("ожидался окончательный отказ, получено %v", err)
			}
		})
	}
}

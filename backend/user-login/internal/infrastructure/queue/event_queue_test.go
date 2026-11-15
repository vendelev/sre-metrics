package queue

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	dError "github.com/vendelev/sre-metrics/user-login/internal/domain/error"
	dEvent "github.com/vendelev/sre-metrics/user-login/internal/domain/event"
)

func ingestEvent(id int, kind dEvent.Type, at time.Time) *dEvent.Request {
	e := &dEvent.Request{
		EventID:    uuid.MustParse(fmt.Sprintf("019aabcd-0000-7000-8000-%012x", id)),
		ScenarioID: uuid.MustParse("019aabcd-0000-7000-9000-000000000100"),
		OccurredAt: at,
		Type:       kind,
		Source:     dEvent.SourceMIS,
	}
	if kind == dEvent.LoginModeDetermined {
		mode := dEvent.LoginModeCredentials
		e.Source = dEvent.SourceAuth
		e.AuthMode = &mode
	}
	return e
}

// BR-57: переполнение не меняет очередь; закрытие сохраняет принятые события и FIFO.
func TestIngestCapacityAndFIFO(t *testing.T) {
	now := time.Now()
	i := NewEventQueue(2)
	defer i.Close()
	events := []*dEvent.Request{
		ingestEvent(1, dEvent.LoginStarted, now),
		ingestEvent(2, dEvent.LoginSucceeded, now),
	}
	for _, e := range events {
		if err := i.Enqueue(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := i.Enqueue(ingestEvent(3, dEvent.LoginFailed, now)); !errors.Is(err, dError.ErrOverloaded) {
		t.Fatalf("полная очередь: %v", err)
	}
	i.Close()
	for _, want := range events {
		if got, ok := <-i.Events(); !ok || got != want {
			t.Fatal("нарушен FIFO или потеряно принятое событие")
		}
	}
	if _, ok := <-i.Events(); ok {
		t.Fatal("переполнение сохранило отклонённое событие")
	}
}

func TestIngestForwardsDuplicates(t *testing.T) {
	now := time.Now()
	i := NewEventQueue(2)
	defer i.Close()
	e := ingestEvent(1, dEvent.LoginSucceeded, now)
	for range 2 {
		if err := i.Enqueue(e); err != nil {
			t.Fatal(err)
		}
	}
	if len(i.Events()) != 2 {
		t.Fatal("очередь выполнила дедупликацию вместо постановки")
	}
}

func TestIngestCloseAgainstConcurrentAccept(t *testing.T) {
	now := time.Now()
	i := NewEventQueue(128)
	start := make(chan struct{})
	var accepted atomic.Int64
	var wg sync.WaitGroup
	for id := 1; id <= 128; id++ {
		wg.Go(func() {
			<-start
			if err := i.Enqueue(ingestEvent(id, dEvent.LoginSucceeded, now)); err == nil {
				accepted.Add(1)
			}
		})
	}
	wg.Go(func() { <-start; i.Close() })
	close(start)
	wg.Wait()
	i.Close()
	if err := i.Enqueue(
		ingestEvent(129, dEvent.LoginSucceeded, now),
	); !errors.Is(err, ErrClosed) ||
		errors.Is(err, dError.ErrOverloaded) {
		t.Fatalf("закрытый приём выдал успех или переполнение: %v", err)
	}
	count := 0
	for range i.Events() {
		count++
	}
	if int64(count) != accepted.Load() {
		t.Fatal("закрытие потеряло принятое событие")
	}
}

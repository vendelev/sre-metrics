package application

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vendelev/sre-metrics/user-login/internal/domain/contract"
	dError "github.com/vendelev/sre-metrics/user-login/internal/domain/error"
	"github.com/vendelev/sre-metrics/user-login/internal/domain/event"
)

type failingStore struct {
	err error
}

func (s failingStore) Within(func(contract.State) error) error { return s.err }

type fixedClock struct{}

func (fixedClock) Now() time.Time { return validEvent().OccurredAt }

func TestNewLoginObserverRequiresDependencies(t *testing.T) {
	if _, err := NewLoginObserver(nil, fixedClock{}); !errors.Is(err, dError.ErrInternal) {
		t.Fatal("отсутствие хранилища не проверено")
	}
	if _, err := NewLoginObserver(failingStore{}, nil); !errors.Is(err, dError.ErrInternal) {
		t.Fatal("отсутствие часов не проверено")
	}
}

func TestUnexpectedStoreError(t *testing.T) {
	cause := errors.New("сбой тестового хранилища")
	o, err := NewLoginObserver(failingStore{err: cause}, fixedClock{})
	if err != nil {
		t.Fatal(err)
	}
	operations := []struct {
		name string
		run  func() error
	}{
		{name: "событие", run: func() error { _, err := o.Process(validEvent()); return err }},
		{name: "обслуживание", run: o.Maintain},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			err := operation.run()
			if !errors.Is(err, cause) || !errors.Is(err, dError.ErrInternal) {
				t.Fatalf("неожиданный сбой потерял категорию или причину: %v", err)
			}
		})
	}
}

func validEvent() event.Request {
	return event.Request{
		EventID:    uuid.MustParse("019aabcd-0000-7000-8000-000000000001"),
		ScenarioID: uuid.MustParse("019aabcd-0000-7000-8000-000000000100"),
		OccurredAt: time.Date(2026, time.November, 2, 12, 0, 0, 0, time.UTC),
		Type:       event.LoginStarted,
		Source:     event.SourceMIS,
	}
}

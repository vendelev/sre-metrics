package application

import (
	"errors"
	"testing"
	"time"

	"github.com/vendelev/sre-metrics/service/user-login/internal/domain"
)

type failingStore struct {
	err error
}

func (s failingStore) Within(func(domain.State) error) error { return s.err }

type fixedClock struct{}

func (fixedClock) Now() time.Time { return validEvent().OccurredAt }

func TestNewLoginObserverRequiresDependencies(t *testing.T) {
	if _, err := NewLoginObserver(nil, fixedClock{}); !errors.Is(err, domain.ErrInternal) {
		t.Fatal("отсутствие хранилища не проверено")
	}
	if _, err := NewLoginObserver(failingStore{}, nil); !errors.Is(err, domain.ErrInternal) {
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
			if !errors.Is(err, cause) || !errors.Is(err, domain.ErrInternal) {
				t.Fatalf("неожиданный сбой потерял категорию или причину: %v", err)
			}
		})
	}
}

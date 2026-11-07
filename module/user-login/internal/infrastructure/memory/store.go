package memory

import (
	"errors"
	"fmt"
	"iter"
	"sync"
	"time"

	"github.com/google/uuid"
	dContract "github.com/vendelev/sre-metrics/user-login/internal/domain/contract"
	dError "github.com/vendelev/sre-metrics/user-login/internal/domain/error"
	dEvent "github.com/vendelev/sre-metrics/user-login/internal/domain/event"
	dScenario "github.com/vendelev/sre-metrics/user-login/internal/domain/scenario"
)

// Limits задаёт положительные пределы памяти без вытеснения действующих записей.
type Limits struct {
	MaxScenarios         int
	MaxEventsPerScenario int
}

// DefaultLimits возвращает начальные ограничения BR-31 и BR-35.
func DefaultLimits() Limits {
	return Limits{MaxScenarios: 10_000, MaxEventsPerScenario: 32}
}

// Store хранит единое состояние процесса. После создания его нельзя копировать.
// Сценарии, индекс, ограничения и регистрация измерений защищены одной блокировкой.
type Store struct {
	mu        sync.Mutex
	limits    Limits
	scenarios map[uuid.UUID]*record
	events    map[uuid.UUID]dEvent.Request
	recorder  dContract.MeasurementRecorder
}

type record struct {
	scenario *dScenario.Scenario
	eventIDs map[uuid.UUID]struct{}
}

var _ dContract.Store = (*Store)(nil)

// NewStore создаёт пустое хранилище с готовым локальным регистратором измерений.
// Нулевые и отрицательные ограничения отклоняются, а не заменяются умолчаниями.
// Зависимость должна быть ненулевой, в том числе не содержать типизированный nil.
func NewStore(limits Limits, recorder dContract.MeasurementRecorder) (*Store, error) {
	if limits.MaxScenarios <= 0 || limits.MaxEventsPerScenario <= 0 {
		return nil, errors.New("лимиты сценариев и событий должны быть положительными")
	}
	if recorder == nil {
		return nil, errors.New("отсутствует регистратор измерений")
	}
	return &Store{
		limits: limits, scenarios: make(map[uuid.UUID]*record),
		events: make(map[uuid.UUID]dEvent.Request), recorder: recorder,
	}, nil
}

// Within предоставляет синхронный доступ без отката ранее выполненных фиксаций.
// Обращения к State вне callback и вложенные операции с этим Store запрещены.
func (s *Store) Within(operation func(dContract.State) error) error {
	if operation == nil {
		return fmt.Errorf("отсутствует операция: %w", dError.ErrInternal)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	access := &state{store: s}
	defer func() { access.store = nil }()
	return operation(access)
}

// state не выдаёт внутренних указателей. После callback доступ отзывается.
// Как и весь State, этот объект нельзя использовать конкурентно или сохранять.
type state struct {
	store *Store
}

func (s *state) RemoveRetained(now time.Time) error {
	if s.store == nil {
		return dError.ErrInternal
	}
	for id, record := range s.store.scenarios {
		if record.scenario.RetentionExpired(now) {
			if err := s.DeleteScenario(id); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *state) FindScenario(id uuid.UUID) (*dScenario.Scenario, bool) {
	if s.store == nil {
		return nil, false
	}
	record, ok := s.store.scenarios[id]
	if !ok {
		return nil, false
	}
	return record.scenario.Clone(), true
}

func (s *state) FindEvent(id uuid.UUID) (dEvent.Request, bool) {
	if s.store == nil {
		return dEvent.Request{}, false
	}
	storeEvent, ok := s.store.events[id]
	return storeEvent.Clone(), ok
}

func (s *state) Scenarios() iter.Seq[*dScenario.Scenario] {
	return func(yield func(*dScenario.Scenario) bool) {
		if s.store == nil {
			return
		}
		// Удаление и обновление существующих записей допустимы при обходе map.
		// Добавление записей во время обхода запрещено контрактом.
		for _, record := range s.store.scenarios {
			if !yield(record.scenario.Clone()) {
				return
			}
		}
	}
}

func (s *state) CheckCapacity(scenarioID uuid.UUID) error {
	if s.store == nil {
		return dError.ErrInternal
	}
	if record, ok := s.store.scenarios[scenarioID]; ok {
		if len(record.eventIDs) >= s.store.limits.MaxEventsPerScenario {
			return dError.ErrCapacityExceeded
		}
		return nil
	}
	if len(s.store.scenarios) >= s.store.limits.MaxScenarios {
		return dError.ErrCapacityExceeded
	}
	return nil
}

func (s *state) Commit(
	loginScenario *dScenario.Scenario,
	event *dEvent.Request,
	measurements dScenario.Measurements,
) error {
	if s.store == nil {
		return dError.ErrInternal
	}
	entry, exists := s.store.scenarios[loginScenario.ID()]
	// Предусловия комплекта обеспечивает application. После сохранения отметок
	// вызывается только локальный SDK. Между инструментами нет транзакции или механизма отката.
	if !exists {
		entry = &record{eventIDs: make(map[uuid.UUID]struct{})}
		s.store.scenarios[loginScenario.ID()] = entry
	}
	entry.scenario = loginScenario.Clone()
	if event != nil {
		s.store.events[event.EventID] = event.Clone()
		entry.eventIDs[event.EventID] = struct{}{}
	}
	s.store.recorder.Record(measurements)
	return nil
}

func (s *state) DeleteScenario(id uuid.UUID) error {
	if s.store == nil {
		return dError.ErrInternal
	}
	if record, ok := s.store.scenarios[id]; ok {
		for eventID := range record.eventIDs {
			delete(s.store.events, eventID)
		}
		delete(s.store.scenarios, id)
	}
	return nil
}

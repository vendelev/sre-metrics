package memory

import (
	"errors"
	"fmt"
	"sync"

	"github.com/vendelev/sre-metrics/service/user-auth/internal/domain"
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
	scenarios map[string]*record
	events    map[string]domain.Event
	recorder  domain.MeasurementRecorder
}

type record struct {
	scenario *domain.Scenario
	eventIDs map[string]struct{}
}

var _ domain.Store = (*Store)(nil)

// NewStore создаёт пустое хранилище с готовым локальным регистратором измерений.
// Нулевые и отрицательные ограничения отклоняются, а не заменяются умолчаниями.
// Зависимость должна быть ненулевой, в том числе не содержать типизированный nil.
func NewStore(limits Limits, recorder domain.MeasurementRecorder) (*Store, error) {
	if limits.MaxScenarios <= 0 || limits.MaxEventsPerScenario <= 0 {
		return nil, errors.New("лимиты сценариев и событий должны быть положительными")
	}
	if recorder == nil {
		return nil, errors.New("отсутствует регистратор измерений")
	}
	return &Store{
		limits: limits, scenarios: make(map[string]*record),
		events: make(map[string]domain.Event), recorder: recorder,
	}, nil
}

// Within предоставляет синхронный доступ без отката ранее выполненных фиксаций.
// Обращения к State вне callback и вложенные операции с этим Store запрещены.
func (s *Store) Within(operation func(domain.State) error) error {
	if operation == nil {
		return fmt.Errorf("отсутствует операция: %w", domain.ErrInternal)
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

func (s *state) FindScenario(id string) (*domain.Scenario, bool) {
	if s.store == nil {
		return nil, false
	}
	record, ok := s.store.scenarios[id]
	if !ok {
		return nil, false
	}
	return record.scenario.Clone(), true
}

func (s *state) FindEvent(id string) (domain.Event, bool) {
	if s.store == nil {
		return domain.Event{}, false
	}
	event, ok := s.store.events[id]
	return event.Clone(), ok
}

func (s *state) VisitScenarios(visit func(*domain.Scenario) error) error {
	if s.store == nil || visit == nil {
		return domain.ErrInternal
	}
	// Удаление текущей записи и обновление существующей допустимы при обходе map.
	// Добавление записей из visit не допускается контрактом обхода.
	for _, record := range s.store.scenarios {
		if err := visit(record.scenario.Clone()); err != nil {
			return err
		}
	}
	return nil
}

func (s *state) CheckCapacity(scenarioID string) error {
	if s.store == nil {
		return domain.ErrInternal
	}
	if record, ok := s.store.scenarios[scenarioID]; ok {
		if len(record.eventIDs) >= s.store.limits.MaxEventsPerScenario {
			return domain.ErrCapacityExceeded
		}
		return nil
	}
	if len(s.store.scenarios) >= s.store.limits.MaxScenarios {
		return domain.ErrCapacityExceeded
	}
	return nil
}

func (s *state) Commit(scenario *domain.Scenario, event *domain.Event, measurements domain.Measurements) error {
	if s.store == nil || scenario == nil {
		return domain.ErrInternal
	}
	if scenario.ID() == "" || !validMeasurements(measurements) {
		return domain.ErrInternal
	}
	entry, exists := s.store.scenarios[scenario.ID()]
	if event == nil {
		if !exists {
			return domain.ErrInternal
		}
	} else {
		if event.EventID == "" || event.ScenarioID != scenario.ID() {
			return domain.ErrInternal
		}
		if _, ok := s.store.events[event.EventID]; ok {
			return domain.ErrConflict
		}
		if err := s.CheckCapacity(scenario.ID()); err != nil {
			return err
		}
	}
	// Все проверки выполнены до первой записи. После сохранения отметок вызывается
	// только локальный SDK. Между инструментами нет транзакции или механизма отката.
	if !exists {
		entry = &record{eventIDs: make(map[string]struct{})}
		s.store.scenarios[scenario.ID()] = entry
	}
	entry.scenario = scenario.Clone()
	if event != nil {
		s.store.events[event.EventID] = event.Clone()
		entry.eventIDs[event.EventID] = struct{}{}
	}
	s.store.recorder.Record(measurements)
	return nil
}

func (s *state) DeleteScenario(id string) error {
	if s.store == nil {
		return domain.ErrInternal
	}
	if record, ok := s.store.scenarios[id]; ok {
		for eventID := range record.eventIDs {
			delete(s.store.events, eventID)
		}
		delete(s.store.scenarios, id)
	}
	return nil
}

package memory

import (
	"errors"
	"iter"
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
// Единственный владелец — воркер; конкурентный доступ не поддерживается.
type Store struct {
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

// RemoveRetained удаляет истёкшие закрытые сценарии вместе с индексом событий.
func (s *Store) RemoveRetained(now time.Time) error {
	for id, record := range s.scenarios {
		if record.scenario.RetentionExpired(now) {
			if err := s.DeleteScenario(id); err != nil {
				return err
			}
		}
	}
	return nil
}

// FindScenario возвращает хранимый сценарий без копирования.
func (s *Store) FindScenario(id uuid.UUID) (*dScenario.Scenario, bool) {
	record, ok := s.scenarios[id]
	if !ok {
		return nil, false
	}
	return record.scenario, true
}

// FindEvent возвращает независимую копию сохранённого события.
func (s *Store) FindEvent(id uuid.UUID) (dEvent.Request, bool) {
	storeEvent, ok := s.events[id]
	return storeEvent.Clone(), ok
}

// Scenarios перебирает ссылки на сценарии без гарантии порядка.
func (s *Store) Scenarios() iter.Seq[*dScenario.Scenario] {
	return func(yield func(*dScenario.Scenario) bool) {
		// Удаление и обновление существующих записей допустимы при обходе map.
		// Добавление записей во время обхода запрещено контрактом.
		for _, record := range s.scenarios {
			if !yield(record.scenario) {
				return
			}
		}
	}
}

// CheckCapacity проверяет место для нового события и, при необходимости, сценария.
func (s *Store) CheckCapacity(scenarioID uuid.UUID) error {
	if record, ok := s.scenarios[scenarioID]; ok {
		if len(record.eventIDs) >= s.limits.MaxEventsPerScenario {
			return dError.ErrCapacityExceeded
		}
		return nil
	}
	if len(s.scenarios) >= s.limits.MaxScenarios {
		return dError.ErrCapacityExceeded
	}
	return nil
}

// Commit сохраняет сценарий и копию нового события, затем регистрирует измерения.
// Проверки факта и вместимости выполняет application до изменения сценария.
func (s *Store) Commit(
	loginScenario *dScenario.Scenario,
	event *dEvent.Request,
	measurements dScenario.Measurements,
) error {
	entry, exists := s.scenarios[loginScenario.ID()]
	// Предусловия комплекта обеспечивает application. После сохранения отметок
	// вызывается только локальный SDK. Между инструментами нет транзакции или механизма отката.
	if !exists {
		entry = &record{eventIDs: make(map[uuid.UUID]struct{})}
		s.scenarios[loginScenario.ID()] = entry
	}
	entry.scenario = loginScenario
	if event != nil {
		s.events[event.EventID] = event.Clone()
		entry.eventIDs[event.EventID] = struct{}{}
	}
	s.recorder.Record(measurements)
	return nil
}

// DeleteScenario удаляет сценарий и его события, не уменьшая накопленные метрики.
func (s *Store) DeleteScenario(id uuid.UUID) error {
	if record, ok := s.scenarios[id]; ok {
		for eventID := range record.eventIDs {
			delete(s.events, eventID)
		}
		delete(s.scenarios, id)
	}
	return nil
}

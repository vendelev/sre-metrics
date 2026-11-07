package contract

import (
	"iter"
	"time"

	"github.com/google/uuid"
	dEvent "github.com/vendelev/sre-metrics/user-login/internal/domain/event"
	dScenario "github.com/vendelev/sre-metrics/user-login/internal/domain/scenario"
)

// Store сериализует доступ к сценариям, событиям и регистрации новых измерений.
type Store interface {
	// Within вызывает operation синхронно в защищённом участке.
	// Ошибка operation НЕ откатывает успешные Commit, RemoveRetained и DeleteScenario: самостоятельные
	// эффекты deadline и retention сохраняются даже при отказе входящему событию.
	// State действителен только до возврата operation; его нельзя сохранять, передавать
	// другой goroutine или использовать для вложенного Within того же хранилища.
	Within(operation func(State) error) error
}

// State предоставляет операции хранения, но не выбирает порядок предметных решений.
// Сущности и события копируются в обе стороны; карты и изменяемые ссылки хранилища не выдаются.
type State interface {
	// RemoveRetained удаляет сценарии с истёкшим retention вместе с их событиями,
	// не изменяя накопленные метрики.
	RemoveRetained(now time.Time) error

	// FindScenario возвращает независимую копию сценария и признак его наличия.
	FindScenario(id uuid.UUID) (*dScenario.Scenario, bool)

	// FindEvent возвращает независимую копию принятого события из глобального индекса.
	FindEvent(id uuid.UUID) (dEvent.Request, bool)

	// Scenarios перебирает независимые копии сценариев без гарантии порядка.
	// Во время обхода допускаются обновление и удаление существующих записей,
	// но не создание новых. Итератор действителен только внутри текущего Within.
	Scenarios() iter.Seq[*dScenario.Scenario]

	// CheckCapacity проверяет место для нового ID и, при необходимости, нового сценария.
	CheckCapacity(scenarioID uuid.UUID) error

	// Commit под блокировкой сохраняет сущность с отметками TakeMeasurements,
	// необязательное новое событие и синхронно регистрирует новые измерения.
	// Предусловия комплекта обеспечивает application до вызова Commit.
	// При ошибке этот комплект не сохраняется.
	// Сбор SDK не является транзакционным снимком нескольких инструментов.
	// event == nil допустим только для самостоятельного изменения существующей записи.
	// scenario должен быть ненулевым; у нового события ScenarioID должен совпадать
	// с ID сценария, а EventID должен отсутствовать в хранилище.
	// Переданные измерения должны быть результатом TakeMeasurements переданного scenario.
	// ID должны быть нормализованы application; перед приёмом нового ID вызывающий
	// обязан успешно выполнить CheckCapacity в том же Within, без промежуточных
	// изменений вместимости. Commit повторно вместимость не проверяет.
	Commit(scenario *dScenario.Scenario, event *dEvent.Request, measurements dScenario.Measurements) error

	// DeleteScenario удаляет сценарий и все его события, не уменьшая метрики.
	DeleteScenario(id uuid.UUID) error
}

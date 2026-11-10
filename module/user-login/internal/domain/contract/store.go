package contract

import (
	"iter"
	"time"

	"github.com/google/uuid"
	dEvent "github.com/vendelev/sre-metrics/user-login/internal/domain/event"
	dScenario "github.com/vendelev/sre-metrics/user-login/internal/domain/scenario"
)

// Store предоставляет операции хранения единственному владельцу состояния — воркеру.
// Доступ не синхронизируется: обработка и обслуживание должны выполняться последовательно.
// Сценарии выдаются по ссылке без копирования; ссылки нельзя передавать другим goroutine.
// Ошибка обработки не откатывает самостоятельные эффекты deadline и retention.
type Store interface {
	// RemoveRetained удаляет сценарии с истёкшим retention вместе с их событиями,
	// не изменяя накопленные метрики.
	RemoveRetained(now time.Time) error

	// FindScenario возвращает хранимый сценарий и признак его наличия.
	FindScenario(id uuid.UUID) (*dScenario.Scenario, bool)

	// FindEvent возвращает независимую копию принятого события из глобального индекса.
	FindEvent(id uuid.UUID) (dEvent.Request, bool)

	// Scenarios перебирает хранимые сценарии без гарантии порядка.
	// Во время обхода допускаются обновление и удаление существующих записей,
	// но не создание новых. Обход выполняется владельцем состояния синхронно.
	Scenarios() iter.Seq[*dScenario.Scenario]

	// CheckCapacity проверяет место для нового ID и, при необходимости, нового сценария.
	CheckCapacity(scenarioID uuid.UUID) error

	// Commit сохраняет ссылку на сущность с отметками TakeMeasurements,
	// необязательное новое событие и синхронно регистрирует новые измерения.
	// Предусловия комплекта обеспечивает application до вызова Commit.
	// Изменения уже хранимой сущности видны до Commit; изоляции и отката нет.
	// Новое событие копируется, включая необязательный вариант входа.
	// Сбор SDK не является транзакционным снимком нескольких инструментов.
	// event == nil допустим только для самостоятельного изменения существующей записи.
	// scenario должен быть ненулевым; у нового события ScenarioID должен совпадать
	// с ID сценария, а EventID должен отсутствовать в хранилище.
	// Переданные измерения должны быть результатом TakeMeasurements переданного scenario.
	// ID должны быть нормализованы application; перед приёмом нового ID вызывающий
	// обязан успешно выполнить CheckCapacity в той же операции, без промежуточных
	// изменений вместимости. Commit повторно вместимость не проверяет.
	Commit(scenario *dScenario.Scenario, event *dEvent.Request, measurements dScenario.Measurements) error

	// DeleteScenario удаляет сценарий и все его события, не уменьшая метрики.
	DeleteScenario(id uuid.UUID) error
}

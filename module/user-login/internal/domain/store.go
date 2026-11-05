package domain

// Store сериализует доступ к сценариям, событиям и регистрации новых измерений.
type Store interface {
	// Within вызывает operation синхронно в защищённом участке.
	// Ошибка operation НЕ откатывает успешные Commit и DeleteScenario: самостоятельные
	// эффекты deadline и retention сохраняются даже при отказе входящему событию.
	// State действителен только до возврата operation; его нельзя сохранять, передавать
	// другой goroutine или использовать для вложенного Within того же хранилища.
	Within(operation func(State) error) error
}

// State предоставляет операции хранения, но не выбирает порядок предметных решений.
// Сущности и события копируются в обе стороны; карты и изменяемые ссылки хранилища не выдаются.
type State interface {
	// FindScenario возвращает независимую копию сценария и признак его наличия.
	FindScenario(id string) (*Scenario, bool)

	// FindEvent возвращает независимую копию принятого события из глобального индекса.
	FindEvent(id string) (Event, bool)

	// VisitScenarios допускает обновление и удаление существующих записей из visit,
	// но не создание новых. Каждый вызов visit получает независимую копию.
	VisitScenarios(visit func(*Scenario) error) error

	// CheckCapacity проверяет место для нового ID и, при необходимости, нового сценария.
	CheckCapacity(scenarioID string) error

	// Commit под блокировкой сохраняет сущность с отметками TakeMeasurements,
	// необязательное новое событие и синхронно регистрирует новые измерения.
	// Все проверки завершаются до записи; при ошибке этот комплект не сохраняется.
	// Сбор SDK не является транзакционным снимком нескольких инструментов.
	// event == nil допустим только для самостоятельного изменения существующей записи.
	// Переданные измерения должны быть результатом TakeMeasurements переданного scenario.
	// ID должны быть нормализованы application; приём нового ID требует CheckCapacity.
	Commit(scenario *Scenario, event *Event, measurements Measurements) error

	// DeleteScenario удаляет сценарий и все его события, не уменьшая метрики.
	DeleteScenario(id string) error
}

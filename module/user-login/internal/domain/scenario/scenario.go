package scenario

import (
	"errors"
	"time"

	"github.com/google/uuid"
	dError "github.com/vendelev/sre-metrics/user-login/internal/domain/error"
	dEvent "github.com/vendelev/sre-metrics/user-login/internal/domain/event"
)

// ErrScenarioExpired означает, что новый факт не принимается после срока ожидания.
// Это не истечение retention: удалением записи управляет вызывающая операция.
var ErrScenarioExpired = errors.New("сценарий закрыт по сроку ожидания")

const (
	waitingPeriod   = 15 * time.Minute
	retentionPeriod = 15 * time.Minute
)

// Outcome обозначает окончательный исход сценария; пустое значение означает отсутствие исхода.
type Outcome string

const (
	// OutcomeSuccess обозначает подтверждённый успех входа.
	OutcomeSuccess Outcome = "success"
	// OutcomeTechnicalError обозначает окончательный технический сбой входа.
	OutcomeTechnicalError Outcome = "technical_error"
	// OutcomeRejected обозначает окончательный штатный отказ во входе.
	OutcomeRejected Outcome = "rejected"
	// OutcomeUnknown назначается Go при истечении ожидания без исхода приложения.
	OutcomeUnknown Outcome = "unknown"
)

// Scenario хранит факты одного сценария входа.
// Первые факты неизменяемы; подтверждение может закрыть истёкшее ожидание,
// но не заменяет факты. Время Go передаётся явно, в рамках одной операции оно едино.
// До обращения к сущности вызывающий код обязан исключить истёкшую по retention запись.
// Возраст и транспортная допустимость событий проверяются снаружи.
// Scenario не обеспечивает потокобезопасность.
type Scenario struct {
	id              uuid.UUID
	startedAt       *time.Time
	authMode        *dEvent.OAuthMode
	outcome         Outcome
	succeededAt     *time.Time
	firstReceivedAt *time.Time
	closedAt        *time.Time
	measurements    measurementState
}

// New создаёт сценарий с идентификатором, пока без фактов входа.
func New(id uuid.UUID) *Scenario {
	return &Scenario{id: id}
}

// Clone возвращает независимую копию фактов и отметок выданных измерений.
// Изменение копии не меняет сохранённую сущность.
func (s *Scenario) Clone() *Scenario {
	scenario := *s
	scenario.startedAt = cloneValue(s.startedAt)
	scenario.authMode = cloneValue(s.authMode)
	scenario.succeededAt = cloneValue(s.succeededAt)
	scenario.firstReceivedAt = cloneValue(s.firstReceivedAt)
	scenario.closedAt = cloneValue(s.closedAt)
	return &scenario
}

func cloneValue[T any](value *T) *T {
	if value == nil {
		return nil
	}
	t := *value
	return &t
}

// Start сохраняет исходный момент начала входа.
// Сначала проверяет конфликт, затем закрывает прежнее истёкшее ожидание.
// Недостающее начало после unknown даёт ErrScenarioExpired.
// Принятое первое просроченное начало сразу закрывает сценарий как unknown.
func (s *Scenario) Start(occurredAt, now time.Time) error {
	if err := s.prepareFact(s.CheckStart(occurredAt, now), now); err != nil {
		return err
	}
	if s.startedAt == nil {
		s.receiveFirst(now)
		s.startedAt = &occurredAt
		s.CloseIfExpired(now)
	}
	return nil
}

// DetermineMode сохраняет сообщённый вариант входа, не выводя его из длительности.
// Повтор подтверждает факт, другой вариант даёт ErrConflict.
// Закрывает истёкшее ожидание; недостающий вариант после unknown даёт ErrScenarioExpired.
// mode должен быть одним из двух вариантов контракта событий.
func (s *Scenario) DetermineMode(mode dEvent.OAuthMode, now time.Time) error {
	if err := s.prepareFact(s.CheckMode(mode, now), now); err != nil {
		return err
	}
	if s.authMode == nil {
		s.receiveFirst(now)
		s.authMode = &mode
	}
	return nil
}

// Succeed сохраняет успешный исход и исходный момент успеха.
// Подтверждение не заменяет время; другой исход приложения даёт ErrConflict.
// При истёкшем ожидании закрывает сценарий как unknown и возвращает ErrScenarioExpired.
func (s *Scenario) Succeed(occurredAt, now time.Time) error {
	if err := s.finish(OutcomeSuccess, now); err != nil {
		return err
	}
	if s.succeededAt == nil {
		s.succeededAt = &occurredAt
	}
	return nil
}

// Fail сохраняет техническую ошибку как окончательный исход.
// Повтор подтверждает исход, другой исход приложения даёт ErrConflict.
// При истёкшем ожидании закрывает как unknown и возвращает ErrScenarioExpired.
func (s *Scenario) Fail(now time.Time) error {
	return s.finish(OutcomeTechnicalError, now)
}

// Reject сохраняет штатный отказ как окончательный исход.
// Повтор подтверждает исход, другой исход приложения даёт ErrConflict.
// При истёкшем ожидании закрывает как unknown и возвращает ErrScenarioExpired.
func (s *Scenario) Reject(now time.Time) error {
	return s.finish(OutcomeRejected, now)
}

// CheckStart проверяет факт без изменений состояния, включая эффективный unknown.
// Противоречие началу имеет приоритет перед истечением ожидания.
func (s *Scenario) CheckStart(occurredAt, now time.Time) error {
	if s.startedAt != nil {
		if !s.startedAt.Equal(occurredAt) {
			return dError.ErrConflict
		}
		return nil
	}
	if s.isUnknownAt(now) {
		return ErrScenarioExpired
	}
	return nil
}

// CheckMode проверяет вариант без изменений состояния; известный факт можно подтвердить
// даже после unknown. Противоречие имеет приоритет перед истечением ожидания.
func (s *Scenario) CheckMode(mode dEvent.OAuthMode, now time.Time) error {
	if s.authMode != nil {
		if *s.authMode != mode {
			return dError.ErrConflict
		}
		return nil
	}
	if s.isUnknownAt(now) {
		return ErrScenarioExpired
	}
	return nil
}

// CheckOutcome проверяет исход приложения без изменения состояния.
// outcome должен быть success, technical_error или rejected, но не unknown.
// Подтверждение исхода приложения допустимо независимо от прежнего deadline ожидания.
func (s *Scenario) CheckOutcome(outcome Outcome, now time.Time) error {
	if s.outcome != "" && s.outcome != OutcomeUnknown && s.outcome != outcome {
		return dError.ErrConflict
	}
	if s.isUnknownAt(now) {
		return ErrScenarioExpired
	}
	return nil
}

// CloseIfExpired закрывает принятое ожидание при now >= deadline и сообщает о переходе.
// Закрытие самостоятельно: вызывающая операция сохраняет его и регистрирует завершение в метриках,
// даже если затем не принимает входящий факт из-за вместимости.
// Пустая сущность и принятые исходы не изменяются; closedAt равен now, а не deadline.
func (s *Scenario) CloseIfExpired(now time.Time) bool {
	if !s.WaitingExpired(now) {
		return false
	}
	s.outcome = OutcomeUnknown
	s.closedAt = &now
	return true
}

// FirstReceivedAt возвращает время Go первого принятого факта, не начало входа.
func (s *Scenario) FirstReceivedAt() (time.Time, bool) {
	if s.firstReceivedAt == nil {
		return time.Time{}, false
	}
	return *s.firstReceivedAt, true
}

// Deadline возвращает срок ожидания по началу либо первому получению.
// Сохранённый срок доступен и после закрытия, но больше не ограничивает исход приложения.
func (s *Scenario) Deadline() (time.Time, bool) {
	if s.startedAt != nil {
		return s.startedAt.Add(waitingPeriod), true
	}
	if s.firstReceivedAt != nil {
		return s.firstReceivedAt.Add(waitingPeriod), true
	}
	return time.Time{}, false
}

// WaitingExpired сообщает об истечении только активного ожидания, не меняя сущность.
func (s *Scenario) WaitingExpired(now time.Time) bool {
	deadline, ok := s.Deadline()
	return s.outcome == "" && ok && !now.Before(deadline)
}

// ClosedAt возвращает момент фиксации исхода Go, не исходное время события.
func (s *Scenario) ClosedAt() (time.Time, bool) {
	if s.closedAt == nil {
		return time.Time{}, false
	}
	return *s.closedAt, true
}

// RetentionDeadline возвращает границу хранения: закрытие Go плюс 15 минут.
func (s *Scenario) RetentionDeadline() (time.Time, bool) {
	if s.closedAt == nil {
		return time.Time{}, false
	}
	return s.closedAt.Add(retentionPeriod), true
}

// RetentionExpired сообщает о логическом истечении хранения при now >= границе.
// Вызывающая операция должна исключить запись и её ID до дедупликации и проверки фактов.
// Метод не удаляет запись, не формирует измерения и не изменяет агрегаты.
func (s *Scenario) RetentionExpired(now time.Time) bool {
	deadline, ok := s.RetentionDeadline()
	return ok && !now.Before(deadline)
}

func (s *Scenario) isUnknownAt(now time.Time) bool {
	return s.outcome == OutcomeUnknown || s.WaitingExpired(now)
}

func (s *Scenario) receiveFirst(now time.Time) {
	if s.firstReceivedAt == nil {
		s.firstReceivedAt = &now
	}
}

// prepareFact не связывает приём факта с вместимостью: вызывающая операция сначала
// использует чистую проверку, затем самостоятельное закрытие и проверку места.
func (s *Scenario) prepareFact(err error, now time.Time) error {
	if errors.Is(err, dError.ErrConflict) {
		return err
	}
	s.CloseIfExpired(now)
	return err
}

func (s *Scenario) finish(outcome Outcome, now time.Time) error {
	if err := s.prepareFact(s.CheckOutcome(outcome, now), now); err != nil {
		return err
	}
	if s.outcome == "" {
		s.receiveFirst(now)
		s.outcome = outcome
		s.closedAt = &now
	}
	return nil
}

// ID возвращает идентификатор сценария.
func (s *Scenario) ID() uuid.UUID {
	return s.id
}

// StartedAt возвращает исходный момент начала и признак его наличия.
func (s *Scenario) StartedAt() (time.Time, bool) {
	if s.startedAt == nil {
		return time.Time{}, false
	}
	return *s.startedAt, true
}

// Mode возвращает сообщённый вариант входа и признак его наличия.
func (s *Scenario) Mode() (dEvent.OAuthMode, bool) {
	if s.authMode == nil {
		return "", false
	}

	return *s.authMode, true
}

// Outcome возвращает окончательный исход; пустое значение означает отсутствие исхода.
func (s *Scenario) Outcome() Outcome {
	return s.outcome
}

// SucceededAt возвращает исходный момент успеха и признак его наличия.
func (s *Scenario) SucceededAt() (time.Time, bool) {
	if s.succeededAt == nil {
		return time.Time{}, false
	}

	return *s.succeededAt, true
}

// SuccessfulDuration возвращает длительность по исходным временам и признак её доступности.
// Отсутствующие факты и отрицательный интервал дают false, не меняя исход сценария.
// Нулевой интервал допустим; наличие варианта проверяется отдельно при учёте измерений.
func (s *Scenario) SuccessfulDuration() (time.Duration, bool) {
	if s.outcome != OutcomeSuccess {
		return 0, false
	}
	if s.startedAt == nil || s.succeededAt == nil {
		return 0, false
	}

	duration := s.succeededAt.Sub(*s.startedAt)
	if duration < 0 {
		return 0, false
	}

	return duration, true
}

// HasNegativeDuration отличает дефект измерения от недостатка фактов.
// Не зависит от варианта, не меняет успех и не выполняет диагностическое логирование.
func (s *Scenario) HasNegativeDuration() bool {
	return s.outcome == OutcomeSuccess && s.startedAt != nil && s.succeededAt.Before(*s.startedAt)
}

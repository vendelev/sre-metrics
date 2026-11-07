package scenario

import (
	"time"

	dEvent "github.com/vendelev/sre-metrics/user-login/internal/domain/event"
)

// Measurements содержит новые измерения для регистрации в метриках:
// одно завершение, одну техническую ошибку известного варианта
// или одну успешную длительность известного варианта. Идентификаторов и SDK здесь нет.
// Пустые поля означают отсутствие нового измерения; нулевая длительность допустима.
type Measurements struct {
	Completion         Outcome
	TechnicalErrorMode dEvent.OAuthMode
	SuccessfulDuration *DurationObservation
}

// DurationObservation описывает корректную успешную длительность известного варианта.
type DurationObservation struct {
	Mode     dEvent.OAuthMode
	Duration time.Duration
}

type measurementState struct {
	hasCompletion         bool
	hasTechnicalError     bool
	hasSuccessfulDuration bool
}

// TakeMeasurements возвращает доступные измерения и отмечает их выданными.
// Повторный вызов не возвращает уже выданные измерения; поздние факты могут добавить новые.
// Сам метод не записывает их в метрики.
// Это не подтверждение регистрации или экспорта. Вызывающая защищённая операция
// обязана согласовать фиксацию сущности и этих отметок с локальной регистрацией;
// простой вызов метода такой гарантии не даёт. До вызова проверяется retention.
func (s *Scenario) TakeMeasurements() Measurements {
	var result Measurements
	if s.outcome != "" && !s.measurements.hasCompletion {
		result.Completion = s.outcome
		s.measurements.hasCompletion = true
	}
	if s.outcome == OutcomeTechnicalError && s.authMode != nil && !s.measurements.hasTechnicalError {
		result.TechnicalErrorMode = *s.authMode
		s.measurements.hasTechnicalError = true
	}
	if s.authMode != nil && !s.measurements.hasSuccessfulDuration {
		if duration, ok := s.SuccessfulDuration(); ok {
			result.SuccessfulDuration = &DurationObservation{Mode: *s.authMode, Duration: duration}
			s.measurements.hasSuccessfulDuration = true
		}
	}
	return result
}

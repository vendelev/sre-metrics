package domain

import "time"

// Measurements содержит новые измерения для регистрации в метриках:
// одно завершение, одну техническую ошибку известного варианта
// или одну успешную длительность известного варианта. Идентификаторов и SDK здесь нет.
// Пустые поля означают отсутствие нового измерения; нулевая длительность допустима.
type Measurements struct {
	Completion         Outcome
	TechnicalErrorMode OAuthMode
	SuccessfulDuration *DurationObservation
}

// MeasurementRecorder синхронно регистрирует новые проверенные измерения локально.
// Реализация не выполняет сеть, не запускает задачи и не обращается обратно к Store.
// Инструменты должны быть готовы до первого вызова; Record не возвращает ошибок.
type MeasurementRecorder interface {
	Record(measurements Measurements)
}

// DurationObservation описывает корректную успешную длительность известного варианта.
type DurationObservation struct {
	Mode     OAuthMode
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
	if s.outcome == OutcomeTechnicalError && s.mode != nil && !s.measurements.hasTechnicalError {
		result.TechnicalErrorMode = *s.mode
		s.measurements.hasTechnicalError = true
	}
	if s.mode != nil && !s.measurements.hasSuccessfulDuration {
		if duration, ok := s.SuccessfulDuration(); ok {
			result.SuccessfulDuration = &DurationObservation{Mode: *s.mode, Duration: duration}
			s.measurements.hasSuccessfulDuration = true
		}
	}
	return result
}

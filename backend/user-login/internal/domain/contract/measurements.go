package contract

import dScenario "github.com/vendelev/sre-metrics/user-login/internal/domain/scenario"

// MeasurementRecorder синхронно регистрирует новые проверенные измерения локально.
// Реализация не выполняет сеть, не запускает задачи и не обращается обратно к Store.
// Инструменты должны быть готовы до первого вызова; Record не возвращает ошибок.
type MeasurementRecorder interface {
	Record(measurements dScenario.Measurements)
}

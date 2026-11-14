package bootstrap

import (
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/otel"
)

// Контрактные имена области и инструментов модуля. Значения определены
// контрактом событий и измерений и не переводятся; приложение передаёт имя
// области в Meter общего pipeline, а тесты сборки приложения разбирают
// OTLP-пакеты по этим именам.
const (
	// ScopeName — постоянное имя области инструментирования без предметных данных.
	ScopeName = otel.ScopeName
	// CompletedName — имя счётчика завершённых сценариев.
	CompletedName = otel.CompletedName
	// TechnicalErrorsName — имя счётчика классифицированных технических ошибок.
	TechnicalErrorsName = otel.TechnicalErrorsName
	// DurationName — имя гистограммы успешных длительностей.
	DurationName = otel.DurationName
)

// HistogramView задаёт явные границы и отключение min/max для гистограммы
// длительностей. View применяется при создании provider, поэтому приложение
// передаёт её в общий pipeline при сборке.
func HistogramView() sdkmetric.View {
	return otel.HistogramView()
}

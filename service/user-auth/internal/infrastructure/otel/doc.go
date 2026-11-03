// Package otel отображает новые предметные измерения на локальные инструменты
// OpenTelemetry SDK. Локальный вариант использует ManualReader, явно запускаемый
// Pipeline — PeriodicReader и официальный OTLP/HTTP exporter. Сбор не сбрасывает
// cumulative-значения. Сборка сервиса подготавливает Resource без SDK и активирует
// экспортный вариант после bind; ManualReader остаётся для изолированных тестов.
package otel

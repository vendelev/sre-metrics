// Package otel отображает новые предметные измерения входа на инструменты
// OpenTelemetry SDK. Имена, единицы, View и нулевые ряды инструментов
// принадлежат модулю; общий provider, PeriodicReader и OTLP/HTTP exporter
// владеет pipeline core — модуль регистрирует инструменты в его meter
// через Activate и не управляет SDK напрямую. Локальный вариант
// с ManualReader остаётся для изолированных тестов; сбор не сбрасывает
// cumulative-значения.
package otel

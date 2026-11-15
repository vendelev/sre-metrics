// Package otel содержит общий механизм метрик OTel: Resource из стандартного
// окружения, создание SDK provider с View потребителя, OTLP/HTTP exporter
// с безопасной диагностикой ошибок, PeriodicReader и однократный Shutdown.
// Предметные инструменты, их имена, атрибуты и cardinality принадлежат
// функциональным модулям и регистрируются в meter общего provider.
package otel

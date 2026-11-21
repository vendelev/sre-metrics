# Пакет otlpmetrics

Общий OTLP/HTTP-pipeline создаёт один `MeterProvider` с `PeriodicReader` и
экспортёром. Потребитель передаёт logger и собственные View в
[bootstrap.New](bootstrap/pipeline.go), затем управляет ресурсами через
[api.Pipeline](api/pipeline.go). Конструктор не читает окружение и не запускает ресурсы.

При `Start` OpenTelemetry SDK читает стандартное окружение `OTEL_*`: в том числе
`OTEL_SERVICE_NAME` и `OTEL_RESOURCE_ATTRIBUTES` для Resource,
`OTEL_EXPORTER_OTLP_*` и `OTEL_EXPORTER_OTLP_METRICS_*` для экспортёра,
`OTEL_METRIC_EXPORT_INTERVAL` и `OTEL_METRIC_EXPORT_TIMEOUT` для reader.
Умолчания и приоритеты принадлежат SDK. Примеры — в [.env.example](../../../.env.example).

Владелец вызывает `Start` перед получением `Meter` и `Shutdown` после завершения
работы с инструментами, в том числе при частичном неуспешном запуске.
Последний экспорт входит в однократный `Shutdown` и выполняется best effort в
переданном контексте; отдельного flush или резерва времени нет. Exemplars отключены,
глобальный provider не устанавливается, сообщения экспортёра очищаются от
потенциально чувствительного содержимого. Доставка не гарантируется.

# Модуль core

## Назначение и границы

Общий OTel-pipeline процесса создаёт один `MeterProvider` с одним
`PeriodicReader` и OTLP/HTTP-экспортёром. Предметная логика и управление
жизненным циклом приложения в core не находятся. Общий HTTP-сервер выделен
в [httpserver](../pkg/httpserver/README.md).

## Статус

Работают Resource из стандартного `OTEL_*`-окружения, provider с View
потребителя, отключённые exemplars, экспортёр с безопасной диагностикой и
однократный Shutdown. Сборка в dig выполняется через
[bootstrap.Provide](bootstrap/container.go). Компонент используется
[user-login](../user-login/README.md).

## Входы и выходы

[Metrics](bootstrap/metrics.go) принимает logger и Views инструментов.
Выход — OTLP/HTTP в получатель, настроенный стандартными `OTEL_*`.
Локальный тестовый получатель [oteltest](../pkg/oteltest/receiver.go)
не заменяет SDK и экспортёр подменами.

## Устройство и запуск

Внутренняя реализация provider и экспортёра — в
[otel](internal/infrastructure/otel/pipeline.go). Контейнер только собирает
объекты; последовательность bind HTTP listener → запуск pipeline → запуск
модуля → Serve и общий бюджет остановки задаёт
[bootstrap приложения](../cmd/user-login-httpserver/bootstrap/runtime.go).
Команды проверок — в [инструкции разработки](../../docs/development.md).

## Ограничения и дальнейшее чтение

Финальный экспорт — best effort: ошибка остаётся ошибкой остановки, гарантий
доставки нет. Отдельный резерв бюджета и дополнительный flush не создаются.
Exemplars отключены; имена и атрибуты метрик принадлежат функциональным модулям.
Отдельных точки входа, бинарника и контейнера у core нет.

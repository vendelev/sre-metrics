# Локальный стек наблюдаемости

Основной [docker-compose.yml](../../docker-compose.yml) запускает `user-login-metrics`
(сервис [user-login](../../backend/user-login/README.md)) и основной Collector,
а через `include` подключает [Grafana, Prometheus, Tempo и Loki](../../docker-compose.grafana.yml).
Локальный `docker-compose.override.yml` не требуется.

## Потоки данных

- Метрики сценариев: HTTP-события на `user-login-metrics:8080/metrics/b/user-login/events` → SDK user-login → OTLP/HTTP
  `metrics-otel-collector:4318/v1/metrics` → Prometheus exporter `metrics-otel-collector:8889`
  → scrape Prometheus → recording rules → Grafana.
- Трейсы внешних приложений: основной Collector → Tempo.
  Коннектор `spanmetrics` дополнительно формирует метрики спанов для Prometheus
  с префиксом `traces_span_metrics_`.
- Логи внешних приложений: OTLP → основной Collector → Loki.
  Stdout контейнеров автоматически не собирается; user-login пишет логи в stderr,
  не экспортирует их по OTLP и не создаёт трейсы.
- Метрики связей сервисов: Tempo → Prometheus через remote write `/api/v1/write`.

Collector читает [config.yaml](config.yaml), затем [local.yaml](local.yaml).
Базовый конфиг отдаёт метрики для scrape и преобразует трейсы в метрики спанов.
Локальное дополнение направляет трейсы в Tempo, сохраняя преобразование в метрики,
и добавляет приём логов для Loki.

## Контракт метрик входа

Преобразование `UnderscoreEscapingWithSuffixes` закрепляет ожидаемые имена
`mis_login_scenarios_completed_total`, `mis_login_scenarios_technical_errors_total`
и `mis_login_scenario_duration_seconds` с `_bucket`, `_count`, `_sum`.
Для Resource с `service.name=mis-login-observability` процессор копирует
`host.name` в label `node`, а `deployment.environment.name` — в `environment`.
Исходный Resource сохраняется; заданные приложением окружение и namespace не перезаписываются.
`outcome` и `mode` приходят из SDK. Технический `service.instance.id` различает
процессы после перезапуска и не заменяет стабильное имя ноды.

Prometheus загружает [контрактные recording rules](../../docs/specs/login-observability.prometheus.rules.yml)
через read-only mount, без отдельной копии правил. Scrape и пересчёт правил — раз в 60 секунд.
Смысл SLI, границы Histogram и ограничения baseline определены в
[контракте](../../docs/specs/login-observability.md#8-минимальные-метрики).
Дашборды «Вход МИС: исходы сценариев» и «Вход МИС: SLI и длительности» используют
эти серии с выбором ноды и окружения. SLI вычисляются за скользящий час,
независимо от выбранного диапазона дашборда. Старты и pending не экспортируются.

## Подготовка и запуск

Нужны Docker с Compose, поддерживающим `include`, доступ к registry образов
и существующая доверенная внешняя сеть `sptr_all` с отправителями событий.

Grafana доступна через существующий Traefik в `sptr_all` по адресу
`http://grafana.test/`. Домен задаётся через `GRAFANA_DOMAIN`; добавьте его
в локальное разрешение имён.
Задайте `GRAFANA_ADMIN_PASSWORD` перед первым запуском; умолчание `admin`
допустимо только на изолированном локальном стенде.
`ENTERPRISE_NAME` задаёт домен существующего маршрута приёма внешних трейсов
`http://${ENTERPRISE_NAME}.test/otel/v1/traces`; этот маршрут не публикует API событий.

Из корня проекта:

```sh
export ENTERPRISE_NAME=metrics
test -e .env || cp .env.example .env
# Настройте ноду, окружение и остальные значения в .env.
docker compose config --quiet
docker compose up -d --build
docker compose ps -a
docker compose logs --tail=100 user-login-metrics metrics-otel-collector
```

При обновлении ранее запущенного стенда проверьте `docker compose ps -a`.
Если остались контейнеры удалённых сервисов, добавьте `--remove-orphans` к команде
`docker compose up` для того же Compose-проекта. Флаг удалит контейнеры всех
сервисов этого проекта, которых больше нет в конфигурации, но сохранит именованные тома.
В собственных запросах к метрикам спанов используйте префикс `traces_span_metrics_`.
Имена метрик входа не изменились.

`user-login-metrics` собирается из [docker/metrics/Dockerfile](../metrics/Dockerfile)
с контекстом корня проекта; версия Go должна совпадать с `../../backend/go.mod`.
Настройки сервиса — в его [README](../../backend/user-login/README.md#настройки).
Compose передаёт сервису корневой `.env` через `env_file`; пример всех настроек —
в [.env.example](../../.env.example). В примере заданы OTLP endpoint, интервал экспорта
10 секунд и бюджет остановки 10 секунд; Docker ждёт 15 секунд перед принудительным завершением.
Экспорт использует стандартные `OTEL_*`; интервалы и timeout SDK в `.env` указаны в миллисекундах.
Порты API событий, OTLP и хранилищ на хост не публикуются.
Отправляйте события из доверенной сети на `http://user-login-metrics:8080/metrics/b/user-login/events`.
Внутри сети используйте DNS-имена сервисов Compose (`metrics-otel-collector`, `user-login-metrics`),
не привязывая клиентов к значениям `container_name`.
API не имеет отдельной аутентификации: состав `sptr_all` является границей доверия.

Healthcheck основного Collector проверяет конфигурацию командой `validate`,
а не готовность OTLP или доставку данных. Перед тестовым трафиком дождитесь
успешного экспорта и исходного scrape. Описание отправки синтетического события —
в [инструкции разработки](../../docs/development.md).
Ответ HTTP 202 не подтверждает доставку метрик.

Для остановки приложения с финальным best-effort экспортом сначала выполните
`docker compose stop user-login-metrics`, пока Collector ещё работает.
Затем остановите стек через `docker compose stop`. Эти команды не удаляют тома.
Финальная отправка не гарантирует, что Prometheus успеет сделать scrape.

## Ограничения

Наличие конфигурации и дашбордов не означает приёмку интеграции. Для неё нужно
проверить реальные имена, labels, корзины 5/30/60/+Inf, reset, baseline,
восстановление экспорта и SLI во внешнем pipeline.
SDK и Collector не обеспечивают exactly-once; очереди не сохраняются на диск.
После перезапуска старые ряды процесса могут оставаться в exporter до истечения срока публикации.
Нет данных — не ноль и не 100% SLI; отдельно проверяйте доступность источника.

Tempo и Loki сохраняют данные за 24 часа, Prometheus — до 7 дней с лимитом TSDB 1 GB.
Retention выполняется асинхронно и не задаёт жёсткую квоту диска.
Tempo и Loki ограничены 256 MiB памяти каждый; конфиги рассчитаны на небольшую dev-нагрузку.
В Tempo включены `local-blocks` для TraceQL metrics и `service-graphs`.
Они требуют нового инструментированного трафика; user-login сам его не создаёт.
Метрики спанов создаёт Collector, а не процессор `span-metrics` Tempo.

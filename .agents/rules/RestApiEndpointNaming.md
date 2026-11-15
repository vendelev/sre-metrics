# Именование endpoint'ов REST API

При проектировании HTTP API следуйте принципам [REST resource naming](https://restfulapi.net/resource-naming/), а также правилам ниже.

## Структура URL

Стандартный шаблон для endpoint'ов backend:

```text
/metrics/b/{module-path}/{resource-path}
```

- `/metrics` - имя сервиса;
- `/b` - endpoint серверной части;
- `{module-path}` - путь модуля, в котором существует endpoint;
- `{resource-path}` - ресурс или его вложенный ресурс.

Примеры:

```text
GET  /metrics/b/reference/dictionaries/patient-visit-reasons
PUT  /metrics/b/reference/dictionaries/patient-visit-reasons/{patientVisitReasonId}
GET  /metrics/b/doctor-portfolio/doctors/{doctorId}/settings
```

Все сегменты пути, кроме параметров, именуются в `kebab-case`.
Ресурсы называются существительными, обычно во множественном числе.
Параметры пути именуются в `camelCase` и отражают идентификатор ресурса: `{doctorId}`, `{patientVisitReasonId}`.

Для API, доступного только административной части модуля, используется группировка `/admin` после пути модуля:

```text
/metrics/b/{module-path}/admin/{resource-path}
```

## HTTP-методы

Для стандартных операций используйте HTTP-метод, а не глагол в URL:

```text
GET    /resources                 # список
POST   /resources                 # создание
GET    /resources/{resourceId}    # получение одного ресурса
PUT    /resources/{resourceId}    # полная замена
PATCH  /resources/{resourceId}    # частичное изменение
DELETE /resources/{resourceId}    # удаление
```

## Actions

Если операция не является работой с состоянием ресурса и не выражается стандартным HTTP-методом,
используйте специальную группировку `/actions`.
Глагол обязателен только в сегменте после `/actions` и пишется в `kebab-case`.

```text
POST /metrics/b/doctor-portfolio/doctors/{doctorId}/actions/reset-history
GET  /metrics/b/reference/mes/actions/get-as-pdf
```

Не добавляйте действия напрямую к ресурсу, например `/resources/get-as-pdf` или `/resources/send-mail`.

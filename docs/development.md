# Разработка Metrics

## Окружение

Версия языка указана в [go.mod](../go.mod); текущий baseline — Go 1.26.0.
Для линтера требуется golangci-lint v2, для образа — Docker с доступным daemon.
Проверка с race detector требует поддерживаемой Go платформы и C-компилятора.
Команды выполняются из корня репозитория.

## Команды

Определения поддерживаются в [Makefile](../Makefile):

```bash
make build             # bin/user-auth
make test              # тесты с race detector; тестов поведения пока нет
make vet               # стандартный статический анализ
make lint              # включая архитектурные импорты
make fmt               # изменяет форматирование Go-кода в module/
make image-user-auth   # образ metrics-user-auth:local
```

Сборка без Make:

```bash
go build -o bin/user-auth ./module/user-auth/cmd/user-auth
docker build -f module/user-auth/Dockerfile -t metrics-user-auth:local .
```

Состояние и поведение запуска описаны в [README модуля](../module/user-auth/README.md).
Успешная сборка каркаса не означает реализацию HTTP API или экспорта.

## Зависимости и организация

Внешних Go-зависимостей пока нет, поэтому `go.sum` отсутствует. Путь Go-модуля
`metrics` — локальное имя до выбора адреса Git-репозитория. При переименовании
обновляются `go.mod`, Go-импорты и пути в [.golangci.yml](../.golangci.yml).

Будущий сервис добавляется в `module/{name}/` с собственными README, точкой входа
и Dockerfile. Дополняются каталог модулей в корневом README, команды сборки
и правила изоляции импортов. Отдельный `go.mod` для каждого сервиса не требуется.

Правила [архитектуры](../.agents/rules/Architecture.md) и
[тестирования](../.agents/rules/Testing.md) определяют границы и требования к проверкам.
Секреты и реальные данные пользователей в исходники и fixtures не включаются.

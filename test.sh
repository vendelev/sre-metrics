#!/usr/bin/env bash
# Ручная проверка API и генерация синтетических данных для локального мониторинга.
# Запуск: bash test.sh [--limits]. Требуются Linux/WSL, GNU date и Docker.
if [ -z "${BASH_VERSION:-}" ]; then
  echo 'Ошибка: запустите скрипт командой bash test.sh.' >&2
  exit 2
fi
set -euo pipefail

network=${DOCKER_NETWORK:-sptr_all}
endpoint=${EVENTS_URL:-http://user-login-metrics:8080/events}
curl_image=${CURL_IMAGE:-curlimages/curl:8.12.1}
check_limits=false
passed=0
failed=0

usage() {
  cat <<'HELP'
Запуск: bash test.sh [--limits]

Проверяет исходы, варианты и длительности входа, частичные данные, повторы,
конфликты, немедленный unknown и ошибки HTTP/JSON. Не перезапускает сервисы.
Создаёт синтетические завершения: они влияют на SLI выбранного окружения!

--limits  Дополнительно проверить 413 и лимит событий одной записи (503).
          MAX_BODY_BYTES=4096 и MAX_EVENTS_PER_SCENARIO=32 должны совпадать
          с настройками приложения. Общий лимит сценариев не заполняется.

Окружение: DOCKER_NETWORK, EVENTS_URL, CURL_IMAGE.
Это не полная приёмка контракта: retention, ожидание без начала, гонки,
перезапуск, сбои экспорта и доставка метрик требуют отдельных проверок.
HELP
}

for arg in "$@"; do
  case "$arg" in
    --limits) check_limits=true ;;
    -h|--help) usage; exit 0 ;;
    *) printf 'Неизвестный аргумент: %s\n' "$arg" >&2; usage >&2; exit 2 ;;
  esac
done

for tool in docker date cat; do
  command -v "$tool" >/dev/null || { printf 'Не найдена команда: %s\n' "$tool" >&2; exit 2; }
done
[[ -r /proc/sys/kernel/random/uuid ]] || { echo 'Требуется Linux/WSL с /proc.' >&2; exit 2; }
date -u -d '2 seconds ago' +%s >/dev/null
docker info >/dev/null
docker network inspect "$network" >/dev/null

# Временная часть UUID соответствует моменту генерации, случайность — из ядра Linux.
uuid7() {
  local id ms
  id=$(cat /proc/sys/kernel/random/uuid)
  printf -v ms '%012x' "$(date +%s%3N)"
  printf '%s-%s-7%s-%s\n' "${ms:0:8}" "${ms:8:4}" "${id:15:3}" "${id:19}"
}

timestamp() {
  date -u -d "$1" +%Y-%m-%dT%H:%M:%S.%3NZ
}

event() {
  local scenario=$1 type=$2 source=$3 at=$4 mode=${5:-} id=${6:-}
  [[ -n "$id" ]] || id=$(uuid7)
  printf '{"version":1,"event_id":"%s","scenario_id":"%s","occurred_at":"%s","source":"%s","type":"%s"' \
    "$id" "$scenario" "$at" "$source" "$type"
  [[ -z "$mode" ]] || printf ',"mode":"%s"' "$mode"
  printf '}'
}

request() {
  local label=$1 expected=$2 payload=$3 method=${4:-POST} content_type=${5-application/json}
  local response status body error_code='' error_pattern
  case "$expected" in
    400) error_code=invalid_event ;;
    405) error_code=method_not_allowed ;;
    409) error_code=event_conflict ;;
    410) error_code=scenario_expired ;;
    413) error_code=payload_too_large ;;
    415) error_code=unsupported_media_type ;;
    503) error_code=capacity_exceeded ;;
  esac
  # Без retry: повтор выполняется только в явных тестах дедупликации.
  if ! response=$(docker run --rm --network "$network" "$curl_image" \
    --silent --show-error --max-time 3 --request "$method" \
    --header "Content-Type: $content_type" --data-binary "$payload" \
    --write-out '\n%{http_code}' "$endpoint"); then
    printf 'ОШИБКА: %s — приём не подтверждён, транспортный сбой.\n' "$label" >&2
    exit 1
  fi
  status=${response##*$'\n'}
  body=${response%$'\n'*}
  error_pattern="\"error\"[[:space:]]*:[[:space:]]*\"$error_code\""
  if [[ "$status" == "$expected" ]] && \
    { [[ "$expected" == 204 && -z "$body" ]] || \
      [[ -n "$error_code" && "$body" =~ $error_pattern ]]; }; then
    printf 'OK: %s — HTTP %s\n' "$label" "$status"
    passed=$((passed + 1))
  else
    printf 'ОШИБКА: %s — ожидался HTTP %s (%s), получен %s, тело: %s\n' \
      "$label" "$expected" "${error_code:-без тела}" "$status" "$body" >&2
    failed=$((failed + 1))
  fi
}

accept() {
  local label=$1
  shift
  request "$label" 204 "$(event "$@")"
}

printf 'Синтетические события будут отправлены в %s (сеть %s).\n' "$endpoint" "$network"
printf 'Они изменят метрики и SLI этого приложения.\n\n'

echo '=== Успехи: оба варианта, границы и превышения порогов ==='
for mode in credentials existing_session; do
  for seconds in 0 2 5 6 30 31 60 61; do
    sid=$(uuid7)
    # Факты в прошлом: не нужно ждать реальную длительность входа.
    finished=$(timestamp '2 seconds ago')
    started=$(timestamp "$finished - $seconds seconds")
    accept "$mode / ${seconds}с: начало" "$sid" login_started mis "$started"
    accept "$mode / ${seconds}с: вариант" "$sid" login_mode_determined auth "$started" "$mode"
    accept "$mode / ${seconds}с: успех" "$sid" login_succeeded mis "$finished"
  done
done

echo '=== Технические ошибки и штатные отказы от обоих источников ==='
for mode in credentials existing_session; do
  sid=$(uuid7)
  at=$(timestamp now)
  accept "$mode: начало" "$sid" login_started mis "$at"
  accept "$mode: вариант" "$sid" login_mode_determined auth "$at" "$mode"
  accept "$mode: техническая ошибка" "$sid" login_failed auth "$at"
done
accept 'Техническая ошибка без варианта и начала' "$(uuid7)" login_failed mis "$(timestamp now)"
for source in mis auth; do
  accept "Штатный отказ от $source" "$(uuid7)" login_rejected "$source" "$(timestamp now)"
done

echo '=== Частичные данные и обратный порядок доставки ==='
sid=$(uuid7)
at=$(timestamp now)
accept 'Успех без начала' "$sid" login_succeeded mis "$at"
accept 'Вариант при отсутствующем начале: Histogram не дополняется' "$sid" login_mode_determined auth "$at" credentials
sid=$(uuid7)
at=$(timestamp '2 seconds ago')
accept 'Начало без варианта' "$sid" login_started mis "$at"
accept 'Успех без варианта: только общий Counter' "$sid" login_succeeded mis "$(timestamp now)"
sid=$(uuid7)
at=$(timestamp '2 seconds ago')
accept 'Обратный порядок: сначала успех' "$sid" login_succeeded mis "$(timestamp now)"
accept 'Поздний вариант успеха' "$sid" login_mode_determined auth "$at" existing_session
accept 'Позднее начало: дополнение Histogram' "$sid" login_started mis "$at"
sid=$(uuid7)
at=$(timestamp now)
accept 'Сначала техническая ошибка' "$sid" login_failed mis "$at"
accept 'Поздний вариант ошибки: дополнение Counter по mode' "$sid" login_mode_determined auth "$at" credentials
sid=$(uuid7)
at=$(timestamp now)
accept 'Отрицательная длительность: начало' "$sid" login_started mis "$at"
accept 'Отрицательная длительность: вариант' "$sid" login_mode_determined auth "$at" credentials
accept 'Успех раньше начала: без Histogram, с диагностикой в логе' "$sid" login_succeeded mis "$(timestamp '2 seconds ago')"

echo '=== Повторы и конфликты ==='
sid=$(uuid7)
at=$(timestamp now)
eid=$(uuid7)
original=$(event "$sid" login_started mis "$at" '' "$eid")
request 'Первый приём' 204 "$original"
request 'Точный повтор event_id' 204 "$original"
accept 'То же начало с новым event_id' "$sid" login_started mis "$at"
request 'Другое содержимое сохранённого event_id' 409 "$(event "$sid" login_rejected mis "$at" '' "$eid")"
request 'Сохранённый event_id в другом сценарии' 409 "$(event "$(uuid7)" login_started mis "$at" '' "$eid")"
request 'Другое время начала' 409 "$(event "$sid" login_started mis "$(timestamp '5 seconds ago')")"
accept 'Первый вариант' "$sid" login_mode_determined auth "$at" credentials
accept 'Повтор варианта с новым event_id' "$sid" login_mode_determined auth "$at" credentials
request 'Противоречащий вариант' 409 "$(event "$sid" login_mode_determined auth "$at" existing_session)"
accept 'Первый окончательный исход — отказ' "$sid" login_rejected mis "$at"
accept 'Подтверждение того же исхода другим источником' "$sid" login_rejected auth "$at"
request 'Другой окончательный исход' 409 "$(event "$sid" login_succeeded mis "$at")"

echo '=== Unknown без ожидания 15 минут ==='
sid=$(uuid7)
at=$(timestamp '16 minutes ago')
accept 'Просроченное первое начало: немедленный unknown' "$sid" login_started mis "$at"
accept 'Подтверждение известного начала после unknown' "$sid" login_started mis "$at"
request 'Поздний успех после unknown' 410 "$(event "$sid" login_succeeded mis "$(timestamp now)")"
request 'Новый вариант после unknown' 410 "$(event "$sid" login_mode_determined auth "$(timestamp now)" credentials)"
request 'Противоречие известному началу после unknown' 409 "$(event "$sid" login_started mis "$(timestamp '17 minutes ago')")"

echo '=== Ошибки HTTP и схемы ==='
valid=$(event "$(uuid7)" login_succeeded mis "$(timestamp now)")
request 'Неподдерживаемый метод' 405 "$valid" GET
request 'Неподдерживаемый Content-Type' 415 "$valid" POST text/plain
request 'Отсутствующий Content-Type' 415 "$valid" POST ''
request 'Content-Type проверяется раньше JSON' 415 '{' POST text/plain
request 'Невалидный JSON' 400 '{'
request 'Массив вместо объекта' 400 '[]'
request 'Отсутствуют обязательные поля' 400 '{}'
request 'Неизвестное поле' 400 "${valid%\}},\"extra\":true}"
request 'Повторяющийся ключ' 400 "${valid%\}},\"version\":1}"
request 'Два JSON-значения' 400 "$valid {}"
request 'Неизвестная версия' 400 "${valid/\"version\":1/\"version\":2}"
request 'Невалидный UUID' 400 "${valid/\"event_id\":\"/\"event_id\":\"invalid-}"
request 'Неверный источник успеха' 400 "$(event "$(uuid7)" login_succeeded auth "$(timestamp now)")"
request 'У варианта нет mode' 400 "$(event "$(uuid7)" login_mode_determined auth "$(timestamp now)")"
request 'Неизвестный mode' 400 "$(event "$(uuid7)" login_mode_determined auth "$(timestamp now)" unknown)"
request 'Лишний mode у успеха' 400 "$(event "$(uuid7)" login_succeeded mis "$(timestamp now)" credentials)"
request 'Событие старше допустимого возраста' 400 "$(event "$(uuid7)" login_succeeded mis "$(timestamp '31 minutes ago')")"
request 'Событие слишком далеко в будущем' 400 "$(event "$(uuid7)" login_succeeded mis "$(timestamp '2 minutes')")"
request 'JSON с параметром charset допустим' 204 "$valid" POST 'application/json; charset=utf-8'
# Последний успех выше — ещё один самостоятельный сценарий без начала.

if "$check_limits"; then
  body_limit=${MAX_BODY_BYTES:-4096}
  event_limit=${MAX_EVENTS_PER_SCENARIO:-32}
  for value in "$body_limit" "$event_limit"; do
    [[ "$value" =~ ^[1-9][0-9]{0,4}$ ]] && ((value <= 10000)) || {
      echo 'Лимиты для ручного теста должны быть числами от 1 до 10000.' >&2; exit 2;
    }
  done
  echo '=== Лимиты: должны совпадать с настройками приложения ==='
  printf -v oversized '%*s' "$((body_limit + 1))" ''
  request 'Превышение размера тела' 413 "$oversized"
  sid=$(uuid7)
  at=$(timestamp now)
  first=$(event "$sid" login_rejected mis "$at")
  request 'Первая запись для проверки вместимости' 204 "$first"
  for ((i = 1; i < event_limit; i++)); do
    accept "Заполнение записи: $((i + 1))/$event_limit" "$sid" login_rejected mis "$at"
  done
  request 'Допустимый новый event_id без места' 503 "$(event "$sid" login_rejected mis "$at")"
  request 'Точный повтор в полной записи' 204 "$first"
  request 'Конфликт важнее нехватки места' 409 "$(event "$sid" login_succeeded mis "$at")"
fi

printf '\nПроверок успешно: %s; ошибок: %s.\n' "$passed" "$failed"
cat <<'RESULT'
При всех успешных проверках ожидаемый вклад этого запуска в общий Counter:
  success +21, technical_error +4, rejected +3, unknown +1.
  С --limits дополнительно rejected +1.
Histogram: по 8 успехов каждого mode + 1 existing_session с поздним началом.
Технические ошибки по mode: credentials +2, existing_session +1.
Другой трафик и перезапуск процесса могут изменить наблюдаемую разницу.

HTTP-проверки НЕ подтверждают доставку метрик. Через 1–2 минуты в Grafana →
Explore → Prometheus → Code выполните:
  sum by (node, environment, outcome) (mis_login_scenarios_completed_total)
  sum by (node, environment, mode) (mis_login_scenario_duration_seconds_count)
  sum by (node, environment, mode) (mis_login_scenarios_technical_errors_total)

Для панелей increase/SLI сначала дождитесь baseline, затем повторите запуск.
Первая Histogram появляется только после реального наблюдения; её первый
отсчёт сам по себе не гарантирует прирост. Пересчёт SLI также занимает время.
Не проверяются: длительное ожидание и retention, общий лимит сценариев,
конкурентные гонки, аварии/перезапуск, 500 и сбои внешнего pipeline.
RESULT
((failed == 0))

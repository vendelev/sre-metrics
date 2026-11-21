#!/usr/bin/env bash
# Ручная проверка приёма синтетических данных для локального мониторинга.
# Запуск: bash test.sh [--limits | --inbox | --mixed]. Требуются Linux/WSL, GNU date и Docker.
if [ -z "${BASH_VERSION:-}" ]; then
  echo 'Ошибка: запустите скрипт командой bash test.sh.' >&2
  exit 2
fi
set -euo pipefail

default_network=sptr_all
default_endpoint=http://user-login-metrics:8080/metrics/b/user-login/events
network=${DOCKER_NETWORK:-$default_network}
endpoint=${EVENTS_URL:-$default_endpoint}
curl_image=${CURL_IMAGE:-curlimages/curl:8.12.1}
log_container=${LOG_CONTAINER-}
if [[ ! -v LOG_CONTAINER && "$endpoint" == "$default_endpoint" && "$network" == "$default_network" ]]; then
  log_container=user-login-metrics
fi
test_mode=''
passed=0
failed=0

usage() {
  cat <<'HELP'
Запуск: bash test.sh [--limits | --inbox | --mixed]

Проверяет HTTP-приём (202 без тела) и ошибки входной валидации.
Конфликты и отклонения после закрытия по сроку проверяет в логах воркера.
Отправляет синтетические события для ручной сверки метрик, не перезапускает сервисы.
Запускайте только на тестовом стенде: события влияют на метрики и SLI!

--limits  Дополнительно проверить по логам лимит событий одной записи.
          MAX_EVENTS_PER_SCENARIO (по умолчанию 32) должен совпадать с
          USER_LOGIN_MAX_EVENTS_PER_SCENARIO получателя. Нужен доступ к логам.

--inbox   Вместо HTTP проверить файловый вход (только на тестовом стенде).
          Нужен работающий сервис Compose user-login-metrics с каталогом входа
          /mnt/metrics/user-login. Учитываются COMPOSE_FILE и COMPOSE_PROJECT_NAME.

--mixed   Отправить 17 сценариев: начало и завершение файлами, вариант по HTTP.
          Файлы, HTTP и логи должны относиться к одному получателю Compose.
          Требования к каталогу такие же, как у --inbox.

Окружение HTTP: DOCKER_NETWORK, EVENTS_URL, CURL_IMAGE, LOG_CONTAINER.
LOG_CONTAINER: имя или ID получателя; для штатных адреса и сети по умолчанию
user-login-metrics, иначе проверка логов отключена. Пустое значение отключает её явно.
В режиме --mixed контейнер для чтения логов всегда определяется через Compose.
Переполнение очереди и доставка метрик автоматически не проверяются.
HELP
}

for arg in "$@"; do
  case "$arg" in
    --limits|--inbox|--mixed)
      if [[ -n "$test_mode" && "$test_mode" != "$arg" ]]; then
        printf 'Ошибка: %s и %s нельзя совмещать.\n' "$test_mode" "$arg" >&2
        exit 2
      fi
      test_mode=$arg
      ;;
    -h|--help) usage; exit 0 ;;
    *) printf 'Неизвестный аргумент: %s\n' "$arg" >&2; usage >&2; exit 2 ;;
  esac
done

for tool in docker date cat; do
  command -v "$tool" >/dev/null || { printf 'Не найдена команда: %s\n' "$tool" >&2; exit 2; }
done
[[ -r /proc/sys/kernel/random/uuid ]] || { echo 'Требуется Linux/WSL с /proc.' >&2; exit 2; }
date -u -d '2 seconds ago' +%s >/dev/null

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
  printf '{"event_id":"%s","scenario_id":"%s","occurred_at":"%s","source":"%s","type":"%s"' \
    "$id" "$scenario" "$at" "$source" "$type"
  [[ -z "$mode" ]] || printf ',"mode":"%s"' "$mode"
  printf '}'
}

inbox_publish() {
  local id=$1 payload=$2
  # Данные идут через stdin, а не в команду контейнера или вывод скрипта.
  printf '%s' "$payload" | docker compose --profile file-inbox run --rm --no-deps -T \
    user-login-inbox-writer sh -ec '
      file="/mnt/metrics/user-login/$1.json"
      dir=${file%/*}
      if [ ! -w "$dir" ] || [ ! -x "$dir" ]; then
        echo "Каталог файлового входа недоступен для записи. Проверьте запуск получателя, том и права." >&2
        exit 1
      fi
      tmp=$(mktemp "$dir/.tmp-$1.XXXXXX")
      cleanup() { rm -f -- "$tmp"; }
      trap cleanup EXIT
      cat > "$tmp"
      mv -nT -- "$tmp" "$file"
      if [ -e "$tmp" ]; then
        echo "Имя уже занято, опубликованный файл не заменён." >&2
        exit 1
      fi
      trap - EXIT
      # После публикации писатель файл не меняет и сам не удаляет.
      for i in $(seq 1 30); do
        if [ ! -e "$file" ] && [ ! -L "$file" ]; then
          exit 0
        fi
        sleep 1
      done
      printf "Файл %s.json не исчез за 30 секунд.\n" "$1" >&2
      exit 1
    ' sh "$id"
}

log_has() {
  local logs=$1 key=$2 value=$3 reason=$4 scenario=${5:-} line
  while IFS= read -r line; do
    if [[ "$line" == *"\"$key\":\"$value\""* && ( -z "$reason" || "$line" == *"\"reason\":\"$reason\""* ) &&
      ( -z "$scenario" || "$line" == *"\"scenario_id\":\"$scenario\""* ) ]]; then
      return 0
    fi
  done <<< "$logs"
  return 1
}

compose_receiver() {
  local container
  container=$(docker compose ps --status running -q user-login-metrics)
  if [[ -z "$container" || "$container" == *$'\n'* ]]; then
    echo 'Нужен один работающий сервис user-login-metrics в выбранном Compose-проекте.' >&2
    return 1
  fi
  printf '%s\n' "$container"
}

check_file_inbox() {
  local container since sid valid malformed expired conflict at logs
  container=$(compose_receiver)
  echo 'Файловый вход: синтетический сценарий; запускайте только на тестовом стенде.'
  since=$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)
  sid=$(uuid7)
  valid=$(uuid7)
  malformed=$(uuid7)
  expired=$(uuid7)
  conflict=$(uuid7)
  at=$(timestamp '2 seconds ago')
  inbox_publish "$valid" "$(event "$sid" login_started mis "$at" '' "$valid")"
  inbox_publish "$malformed" '{'
  # Файл создан сейчас, но событие по occurred_at старше 30 минут (BR-37).
  inbox_publish "$expired" "$(event "$(uuid7)" login_started mis "$(timestamp '31 minutes ago')" '' "$expired")"
  # Второе корректно закодированное начало с другим временем: конфликт доказывает,
  # что воркер получил и сохранил первое начало, а не просто что исчез его файл.
  inbox_publish "$conflict" "$(event "$sid" login_started mis "$(timestamp "$at - 1 second")" '' "$conflict")"

  local i
  for ((i = 0; i < 10; i++)); do
    if ! logs=$(docker logs --since "$since" "$container" 2>&1); then
      echo 'Не удалось прочитать логи файлового входа.' >&2
      return 1
    fi
    if log_has "$logs" event_id "$conflict" event_conflict &&
      log_has "$logs" file "$malformed.json" invalid_content &&
      log_has "$logs" file "$expired.json" invalid_content; then
      echo 'OK: все публикации исчезли; воркер обработал валидное начало.'
      echo 'OK: битый JSON и событие старше 30 минут по occurred_at отклонены.'
      return 0
    fi
    sleep 1
  done
  echo 'Файлы исчезли, но нет всех ожидаемых свидетельств обработки и отклонения в логах.' >&2
  return 1
}

if [[ "$test_mode" == --inbox ]]; then
  check_file_inbox
  exit
fi

if [[ "$test_mode" == --limits ]]; then
  event_limit=${MAX_EVENTS_PER_SCENARIO:-32}
  if [[ ! "$event_limit" =~ ^[1-9][0-9]{0,4}$ ]] || ((event_limit > 10000)); then
    echo 'MAX_EVENTS_PER_SCENARIO должен быть целым числом от 1 до 10000.' >&2
    exit 2
  fi
  if [[ -z "$log_container" ]]; then
    echo '--limits требует доступ к логам получателя: задайте LOG_CONTAINER.' >&2
    exit 2
  fi
fi

if [[ "$test_mode" == --mixed ]]; then
  log_container=$(compose_receiver)
fi

docker network inspect -- "$network" >/dev/null
if [[ -n "$log_container" ]]; then
  if ! log_container_id=$(docker container inspect --format '{{if .State.Running}}{{.Id}}{{end}}' -- "$log_container") ||
    [[ -z "$log_container_id" ]]; then
    printf 'Нет работающего контейнера для проверки логов: %s.\n' "$log_container" >&2
    exit 2
  fi
  logs_since=$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)
  worker_checks=()
fi

request() {
  local label=$1 expected=$2 payload=$3 method=${4:-POST} content_type=${5-application/json}
  local response status body response_type allow error_code='' error_pattern
  case "$expected" in
    400) error_code=invalid_event ;;
    405) error_code=method_not_allowed ;;
    415) error_code=unsupported_media_type ;;
  esac
  # Без retry: повтор выполняется только в явных тестах дедупликации.
  if ! response=$(docker run --rm --network "$network" "$curl_image" \
    --silent --show-error --max-time 3 --request "$method" \
    --header "Content-Type: $content_type" --data-binary "$payload" \
    --write-out '\n%{http_code}|%{content_type}|%header{allow}' -- "$endpoint"); then
    printf 'ОШИБКА: %s — приём не подтверждён, транспортный сбой.\n' "$label" >&2
    exit 1
  fi
  IFS='|' read -r status response_type allow <<< "${response##*$'\n'}"
  body=${response%$'\n'*}
  error_pattern="^[[:space:]]*\{[[:space:]]*\"error\"[[:space:]]*:[[:space:]]*\"$error_code\"[[:space:]]*\}[[:space:]]*$"
  if [[ "$status" == "$expected" ]] &&
    { [[ "$expected" == 202 && -z "$body" ]] ||
      [[ -n "$error_code" && "$response_type" == application/json && "$body" =~ $error_pattern ]]; } &&
    [[ "$expected" != 405 || "$allow" == POST ]]; then
    printf 'OK: %s — HTTP %s\n' "$label" "$status"
    passed=$((passed + 1))
  else
    printf 'ОШИБКА: %s — ожидался HTTP %s (%s), получен %s (Content-Type: %s, Allow: %s), тело: %q\n' \
      "$label" "$expected" "${error_code:-без тела}" "$status" "$response_type" "$allow" "$body" >&2
    if [[ "$status" == 503 && "$body" == *'"overloaded"'* ]]; then
      echo 'Очередь приёма переполнена; событие не поставлено, повторной отправки нет.' >&2
    fi
    failed=$((failed + 1))
  fi
}

accept() {
  local label=$1
  shift
  request "$label" 202 "$(event "$@")"
}

expect_rejection() {
  local label=$1 reason=$2 scenario=$3 type=$4 source=$5 at=$6 mode=${7:-} id=${8:-}
  [[ -n "$id" ]] || id=$(uuid7)
  accept "$label" "$scenario" "$type" "$source" "$at" "$mode" "$id"
  if [[ -n "$log_container" ]]; then
    worker_checks+=("$reason $id $scenario $label")
  fi
}

check_worker_logs() {
  local attempt i logs reason id scenario label
  for ((attempt = 0; attempt < 10; attempt++)); do
    if ! logs=$(docker logs --since "$logs_since" -- "$log_container_id" 2>&1); then
      echo 'Не удалось прочитать логи фоновой обработки.' >&2
      return 1
    fi
    for i in "${!worker_checks[@]}"; do
      read -r reason id scenario label <<< "${worker_checks[i]}"
      if log_has "$logs" event_id "$id" "$reason" "$scenario"; then
        printf 'OK: %s — лог воркера: %s\n' "$label" "$reason"
        passed=$((passed + 1))
        unset 'worker_checks[i]'
      fi
    done
    if ((${#worker_checks[@]} == 0)); then
      if [[ "$test_mode" == --limits ]]; then
        # Заключительный конфликт уже обработан: все предыдущие события тоже прошли через воркер.
        if log_has "$logs" event_id "$limit_last_id" '' "$limit_scenario" ||
          log_has "$logs" event_id "$limit_first_id" '' "$limit_scenario"; then
          echo 'ОШИБКА: воркер отклонил последнее событие при заполнении записи или точный повтор первого.' >&2
          failed=$((failed + 1))
        else
          echo 'OK: последнее событие при заполнении записи и точный повтор первого не отклонены.'
          passed=$((passed + 1))
        fi
      fi
      return 0
    fi
    sleep 1
  done
  for i in "${!worker_checks[@]}"; do
    read -r reason id scenario label <<< "${worker_checks[i]}"
    printf 'ОШИБКА: %s — не найден лог воркера: %s (event_id: %s, scenario_id: %s).\n' \
      "$label" "$reason" "$id" "$scenario" >&2
    failed=$((failed + 1))
  done
}

publish_mixed_event() {
  local scenario=$1 type=$2 source=$3 at=$4 id
  id=$(uuid7)
  inbox_publish "$id" "$(event "$scenario" "$type" "$source" "$at" '' "$id")"
}

send_mixed_scenario() {
  local mode=$1 terminal=$2 seconds=$3 verify_sample=${4:-false}
  local sid started finished
  sid=$(uuid7)
  finished=$(timestamp '2 seconds ago')
  started=$(timestamp "$finished - $seconds seconds")
  printf 'Сценарий %s: %s / %s / %s с.\n' "$sid" "$mode" "$terminal" "$seconds"

  publish_mixed_event "$sid" login_started mis "$started"
  accept "Вариант $mode по HTTP" "$sid" login_mode_determined auth "$started" "$mode"
  ((failed == 0)) || return 1
  publish_mixed_event "$sid" "$terminal" mis "$finished"

  if "$verify_sample"; then
    # Исчезновение файла не доказывает обработку. Для первого успеха проверяем
    # по конфликтам, что воркер сохранил все три факта в одном сценарии (BR-58).
    expect_rejection 'Противоречащее начало' event_conflict "$sid" login_started mis "$(timestamp "$started - 1 second")"
    expect_rejection 'Противоречащий вариант' event_conflict "$sid" login_mode_determined auth "$started" existing_session
    expect_rejection 'Противоречащий исход' event_conflict "$sid" login_failed mis "$finished"
    ((failed == 0)) || return 1
    check_worker_logs
    ((failed == 0)) || return 1
  fi
}

check_mixed_inputs() {
  local mode seconds sid verify_sample=true
  echo 'Смешанные сценарии. Запускайте только на тестовом стенде: изменятся метрики и SLI.'
  printf 'HTTP: %s (сеть %s).\n' "$endpoint" "$network"

  for mode in credentials existing_session; do
    for seconds in 2 5 6 30 31 61; do
      send_mixed_scenario "$mode" login_succeeded "$seconds" "$verify_sample"
      verify_sample=false
    done
    send_mixed_scenario "$mode" login_failed 2
    send_mixed_scenario "$mode" login_rejected 2
  done

  # Первое начало ещё допустимо по возрасту события, но срок ожидания исхода уже истёк (BR-52).
  sid=$(uuid7)
  printf 'Сценарий %s: unknown без ожидания 15 минут.\n' "$sid"
  publish_mixed_event "$sid" login_started mis "$(timestamp '16 minutes ago')"
  expect_rejection 'Новый вариант после unknown' scenario_expired "$sid" login_mode_determined auth "$(timestamp now)" credentials
  ((failed == 0)) || return 1
  check_worker_logs
  ((failed == 0)) || return 1

  echo 'Ожидаемый вклад: 33 принятых файла, 12 успехов, 2 технические ошибки, 2 отказа и 1 unknown.'
  echo 'Гистограмма: по 6 наблюдений на вариант, длительности 2, 5, 6, 30, 31 и 61 с.'
  echo 'Скрипт не проверяет доставку метрик. Дождитесь экспорта и следующего сбора Prometheus.'
}

if [[ "$test_mode" == --mixed ]]; then
  check_mixed_inputs
  exit
fi

printf 'Синтетические события будут отправлены в %s (сеть %s).\n' "$endpoint" "$network"
printf 'Запускайте только на тестовом стенде: события могут изменить метрики и SLI.\n'
if [[ -z "$log_container" ]]; then
  echo 'Логи воркера не проверяются; для проверки задайте LOG_CONTAINER.'
fi
echo

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
accept 'Успех раньше начала: без Histogram, с диагностикой в логе' "$sid" login_succeeded mis "$(timestamp "$at - 2 seconds")"

echo '=== Повторы и конфликты ==='
sid=$(uuid7)
at=$(timestamp now)
eid=$(uuid7)
original=$(event "$sid" login_started mis "$at" '' "$eid")
request 'Первый приём' 202 "$original"
request 'Точный повтор event_id' 202 "$original"
accept 'То же начало с новым event_id' "$sid" login_started mis "$at"
expect_rejection 'Другое содержимое сохранённого event_id' event_conflict "$sid" login_rejected mis "$at" '' "$eid"
expect_rejection 'Сохранённый event_id в другом сценарии' event_conflict "$(uuid7)" login_started mis "$at" '' "$eid"
expect_rejection 'Другое время начала' event_conflict "$sid" login_started mis "$(timestamp "$at - 5 seconds")"
accept 'Первый вариант' "$sid" login_mode_determined auth "$at" credentials
accept 'Повтор варианта с новым event_id' "$sid" login_mode_determined auth "$at" credentials
expect_rejection 'Противоречащий вариант' event_conflict "$sid" login_mode_determined auth "$at" existing_session
accept 'Первый окончательный исход — отказ' "$sid" login_rejected mis "$at"
accept 'Подтверждение того же исхода другим источником' "$sid" login_rejected auth "$at"
expect_rejection 'Другой окончательный исход' event_conflict "$sid" login_succeeded mis "$at"

echo '=== Unknown без ожидания 15 минут ==='
sid=$(uuid7)
at=$(timestamp '16 minutes ago')
accept 'Просроченное первое начало: немедленный unknown' "$sid" login_started mis "$at"
accept 'Подтверждение известного начала после unknown' "$sid" login_started mis "$at"
expect_rejection 'Поздний успех после unknown' scenario_expired "$sid" login_succeeded mis "$(timestamp now)"
expect_rejection 'Новый вариант после unknown' scenario_expired "$sid" login_mode_determined auth "$(timestamp now)" credentials
expect_rejection 'Противоречие известному началу после unknown' event_conflict "$sid" login_started mis "$(timestamp "$at - 1 minute")"

echo '=== Ошибки HTTP и схемы ==='
valid=$(event "$(uuid7)" login_succeeded mis "$(timestamp now)")
request 'Метод проверяется раньше Content-Type и JSON' 405 '{' GET text/plain
request 'Неподдерживаемый Content-Type' 415 "$valid" POST text/plain
request 'Отсутствующий Content-Type' 415 "$valid" POST ''
request 'Content-Type проверяется раньше JSON' 415 '{' POST text/plain
request 'Невалидный JSON' 400 '{'
request 'Массив вместо объекта' 400 '[]'
request 'Отсутствуют обязательные поля' 400 '{}'
request 'Два JSON-значения' 400 "$valid {}"
request 'Невалидный UUID' 400 "${valid/\"event_id\":\"/\"event_id\":\"invalid-}"
request 'Неверный источник успеха' 400 "$(event "$(uuid7)" login_succeeded auth "$(timestamp now)")"
request 'У варианта нет mode' 400 "$(event "$(uuid7)" login_mode_determined auth "$(timestamp now)")"
request 'Неизвестный mode' 400 "$(event "$(uuid7)" login_mode_determined auth "$(timestamp now)" unknown)"
request 'Лишний mode у успеха' 400 "$(event "$(uuid7)" login_succeeded mis "$(timestamp now)" credentials)"
request 'Событие старше допустимого возраста' 400 "$(event "$(uuid7)" login_succeeded mis "$(timestamp '31 minutes ago')")"
request 'Событие слишком далеко в будущем' 400 "$(event "$(uuid7)" login_succeeded mis "$(timestamp '2 minutes')")"
request 'JSON с параметром charset допустим' 202 "$valid" POST 'application/json; charset=utf-8'
# Те же известные поля и event_id: эти варианты не создают дополнительные успехи.
request 'Неизвестное поле игнорируется' 202 "${valid%\}},\"extra\":true}"
request 'При повторении строкового ключа используется последнее значение' 202 '{"source":"auth",'"${valid#\{}"
printf -v padding '%8192s' ''
request 'Большое невалидное тело даёт 400' 400 "$padding{"

if [[ "$test_mode" == --limits ]]; then
  echo '=== Лимит событий одной записи ==='
  limit_scenario=$(uuid7)
  at=$(timestamp now)
  limit_first_id=$(uuid7)
  limit_last_id=$limit_first_id
  first=$(event "$limit_scenario" login_rejected mis "$at" '' "$limit_first_id")
  request 'Первая запись для проверки вместимости' 202 "$first"
  for ((i = 1; i < event_limit; i++)); do
    limit_last_id=$(uuid7)
    accept "Заполнение записи: $((i + 1))/$event_limit" "$limit_scenario" login_rejected mis "$at" '' "$limit_last_id"
  done
  expect_rejection 'Допустимый новый event_id без места' capacity_exceeded "$limit_scenario" login_rejected mis "$at"
  request 'HTTP-приём точного повтора в полной записи' 202 "$first"
  expect_rejection 'Конфликт важнее нехватки места' event_conflict "$limit_scenario" login_succeeded mis "$at"
fi

if [[ -n "$log_container" ]]; then
  echo '=== Отклонения в логах воркера ==='
  check_worker_logs
fi

printf '\nПроверок успешно: %s; ошибок: %s.\n' "$passed" "$failed"
cat <<'RESULT'
Если воркер обработал все события как ожидается, вклад этого запуска в общий Counter:
  success +21, technical_error +4, rejected +3, unknown +1.
  С --limits дополнительно rejected +1.
Histogram: по 8 успехов каждого mode + 1 existing_session с поздним началом.
Технические ошибки по mode: credentials +2, existing_session +1.
Другой трафик и перезапуск процесса могут изменить наблюдаемую разницу.

HTTP 202 подтверждает только постановку в очередь. Скрипт не проверяет метрики автоматически.
Для ручной сверки через 1–2 минуты в Grafana →
Explore → Prometheus → Code выполните:
  sum by (node, environment, outcome) (mis_login_scenarios_completed_total)
  sum by (node, environment, mode) (mis_login_scenario_duration_seconds_count)
  sum by (node, environment, mode) (mis_login_scenarios_technical_errors_total)

Для панелей increase/SLI сначала дождитесь baseline, затем повторите запуск.
Первая Histogram появляется только после реального наблюдения; её первый
отсчёт сам по себе не гарантирует прирост. Пересчёт SLI также занимает время.
Не проверяются: длительное ожидание и retention, общий лимит сценариев,
переполнение очереди, конкурентные гонки, аварии/перезапуск, 500 и сбои внешнего pipeline.
RESULT
((failed == 0))

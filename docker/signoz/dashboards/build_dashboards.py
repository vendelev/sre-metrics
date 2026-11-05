#!/usr/bin/env python3
"""Собирает импортируемые дашборды SigNoz v6 без изменения живого стенда."""

import argparse
import json
from pathlib import Path


ROOT = Path(__file__).resolve().parent
FILTER = 'environment="$environment",node="$node"'
COMPLETED = "mis.login.scenarios.completed"
ERRORS = "mis.login.scenarios.technical_errors"
COUNT = "mis.login.scenario.duration.count"
BUCKET = "mis.login.scenario.duration.bucket"
OUTCOMES = {
    "rejected": "Отказы",
    "success": "Успехи",
    "technical_error": "Технические ошибки",
    "unknown": "Неизвестный исход",
}
MODES = {
    "credentials": "С вводом данных",
    "existing_session": "По сессии Auth",
}


def selector(metric, labels=""):
    suffix = f",{labels}" if labels else ""
    return f'{{"{metric}",{FILTER}{suffix}}}'


def increase(metric, labels="", window="1h"):
    return f"sum by (environment, node) (increase({selector(metric, labels)}[{window}]))"


def ratio(numerator, denominator):
    # Нулевой знаменатель удаляет ряд; отсутствующий источник не превращается в 100%.
    return f"({numerator}) / on (environment, node) (({denominator}) > 0)"


def sli_queries():
    result = {
        "success": ratio(
            increase(COMPLETED, 'outcome="success"'),
            increase(COMPLETED, 'outcome=~"success|technical_error"'),
        )
    }
    for mode, boundary in (("credentials", "30"), ("existing_session", "5")):
        labels = f'mode="{mode}"'
        errors = increase(ERRORS, labels)
        measured = increase(COUNT, labels)
        timely = increase(BUCKET, f'{labels},le="{boundary}"')
        # Только ошибки без Histogram дают 0%; глобальной подстановки нуля нет.
        measured = f"(({measured}) or on (environment, node) (0 * ({errors})))"
        timely = f"(({timely}) or on (environment, node) (0 * ({errors})))"
        result[mode] = ratio(timely, f"{measured} + on (environment, node) ({errors})")
    return result


def panel(title, description, queries, number=False, percent=False):
    kind = "NumberPanel" if number else "TimeSeriesPanel"
    spec = {
        "visualization": {"timePreference": "global_time"},
        "formatting": {"unit": "percent" if percent else "short", "decimalPrecision": "2"},
    }
    if number and percent:
        spec["thresholds"] = [
            {"value": 90, "operator": "below", "color": "#F1575F", "format": "text"},
            {"value": 90, "operator": "above_or_equal", "color": "#4CAF50", "format": "text"},
        ]
    if not number:
        spec.update({
            "chartAppearance": {"lineInterpolation": "step_after", "fillMode": "none"},
            "axes": {"softMin": 0},
            "legend": {"position": "bottom", "mode": "list"},
        })
        if percent:
            spec["axes"]["softMax"] = 100
    envelopes = [
        {"type": "promql", "spec": {
            "name": chr(65 + i), "query": f"100 * ({query})" if percent else query,
            "legend": legend, "disabled": False, "step": 60,
        }}
        for i, (legend, query) in enumerate(queries)
    ]
    query = {"kind": "signoz/CompositeQuery", "spec": {"queries": envelopes}}
    if len(envelopes) == 1:
        query = {"kind": "signoz/PromQLQuery", "spec": envelopes[0]["spec"]}
    return {"kind": "Panel", "spec": {
        "display": {"name": title, "description": description},
        "plugin": {"kind": f"signoz/{kind}", "spec": spec},
        "queries": [{"kind": "scalar" if number else "time_series", "spec": {"plugin": query}}],
    }}


def text_panel(content):
    return {"kind": "Panel", "spec": {
        "display": {"name": "Как читать"},
        "plugin": {"kind": "signoz/TextPanel", "spec": {"mode": "markdown", "text": content}},
        "queries": [],
    }}


def dashboard(name, title, panels, positions):
    return {
        "schemaVersion": "v6", "name": name,
        "tags": [{"key": "application", "value": "mis-login"}],
        "spec": {
            "display": {"name": title, "description": "Сценарии входа МИС: исходные OTLP-метрики в SigNoz."},
            "duration": "1h", "refreshInterval": "1m",
            "variables": [
                {"kind": "ListVariable", "spec": {
                    "name": key, "display": {"name": label}, "defaultValue": default,
                    "allowMultiple": False, "allowAllValue": False, "sort": "alphabetical-asc",
                    "plugin": {"kind": "signoz/DynamicVariable", "spec": {"name": key, "signal": "metrics"}},
                }}
                for key, label, default in (
                    ("environment", "Окружение", "development"), ("node", "Нода", "node-1")
                )
            ],
            "panels": panels,
            "layouts": [{"kind": "Grid", "spec": {"items": [
                {"x": x, "y": y, "width": w, "height": h, "content": {"$ref": f"#/spec/panels/{key}"}}
                for key, (x, y, w, h) in positions.items()
            ]}}],
        },
    }


def outcomes_dashboard():
    panels = {"guide": text_panel(
        "Считаются завершённые сценарии, а не HTTP-запросы.\n\n"
        "- **Отказы** — окончательный штатный отказ: нет прав или учётная запись отключена. "
        "Неверный пароль с возможностью повторить ввод не завершает сценарий.\n"
        "- **Успехи** — МИС восстановила сессию, разрешила доступ и сформировала защищённую страницу.\n"
        "- **Технические ошибки** — подтверждённый сбой, прервавший вход.\n"
        "- **Неизвестный исход** — результат не получен до срока ожидания: 15 минут от начала, "
        "а без начала — от первого получения события сервисом. Это не доказательство ошибки входа.\n\n"
        "Отказы и неизвестные исходы исключены из SLI. Приросты — оценки, а не точный журнал; "
        "возможны дробные значения. Нет данных — не ноль. Накопления процесса сбрасываются при перезапуске."
    )}
    positions = {"guide": (0, 0, 12, 8)}
    for i, (outcome, title) in enumerate(OUTCOMES.items()):
        panels[outcome] = panel(
            title, "Оценка числа завершений за выбранный диапазон дашборда.",
            [(title, "")], number=True,
        )
        # В SigNoz нет макроса Grafana $__range. Нативный scalar-запрос
        # суммирует приросты внутри диапазона, выбранного пользователем.
        panels[outcome]["spec"]["queries"][0]["spec"]["plugin"] = {
            "kind": "signoz/BuilderQuery", "spec": {
                "name": "A", "signal": "metrics", "stepInterval": 60,
                "aggregations": [{
                    "metricName": COMPLETED, "temporality": "Cumulative",
                    "timeAggregation": "increase", "spaceAggregation": "sum", "reduceTo": "sum",
                }],
                "filter": {"expression": f"environment = '$environment' AND node = '$node' AND outcome = '{outcome}'"},
                "legend": title, "disabled": False,
            },
        }
        positions[outcome] = (3 * i, 8, 3, 4)
    panels["recent"] = panel(
        "Завершения за скользящие 5 минут", "Каждая точка — оценка прироста за предшествующие 5 минут.",
        [(title, increase(COMPLETED, f'outcome="{outcome}"', "5m")) for outcome, title in OUTCOMES.items()],
    )
    positions["recent"] = (0, 12, 12, 8)
    panels["counters"] = panel(
        "Текущие накопленные счётчики по исходам",
        "Значения процесса сбрасываются при перезапуске. Старые серии экземпляров могут временно сохраняться.",
        [(title, "sum by (environment, node) (" + selector(COMPLETED, 'outcome=' + json.dumps(outcome)) + ")")
         for outcome, title in OUTCOMES.items()],
    )
    positions["counters"] = (0, 20, 12, 8)
    return dashboard("mis-login-outcomes", "Вход МИС: исходы сценариев", panels, positions)


def sli_dashboard():
    queries = sli_queries()
    titles = {
        "success": "SLI-01: общая успешность",
        "credentials": "SLI-02: с вводом данных ≤ 30 с",
        "existing_session": "SLI-03: по сессии Auth ≤ 5 с",
    }
    panels = {"guide": text_panel(
        "**SLI** — измеренная доля качественных входов, **SLO** — цель: каждый показатель **≥ 90%** "
        "для выбранной ноды и окружения.\n\n"
        "- **Общая успешность** — успехи / (успехи + технические ошибки). Медленный успех тоже учитывается.\n"
        "- **С вводом данных** — Auth потребовал ввод учётных данных. SLI: успехи за **≤ 30 с** / "
        "(успехи с измеренной длительностью + технические ошибки этого варианта).\n"
        "- **По сессии Auth** — действующая сессия Auth без ввода данных; та же формула с порогом **≤ 5 с**. "
        "Действующая сессия самой МИС без нового входа не создаёт сценарий.\n"
        "- **Корзины длительностей** — накопленные успешные входы не дольше указанной границы. "
        "Они перекрываются, складывать их нельзя. «Все длительности» включает успехи дольше 60 с. "
        "Граница 60 с — диагностика, не дополнительный SLO. Длительность включает ввод данных, паузы и редиректы.\n\n"
        "Отказы и неизвестные исходы исключены из всех SLI. Успех без варианта или корректной длительности "
        "входит только в общий SLI. Только ошибки дают 0%; нулевой знаменатель — **нет данных**, не 100%.\n\n"
        "Каждая точка SLI использует **скользящий час**, независимо от диапазона дашборда. "
        "Формулы выполняются непосредственно в SigNoz, без recording rules Prometheus. "
        "Первый прирост новой гистограммы может быть не виден без исходного отсчёта. "
        "Отсутствие данных не доказывает исправность источника."
    )}
    positions = {"guide": (0, 0, 12, 10)}
    for i, (key, title) in enumerate(titles.items()):
        panels[key] = panel(title, "За скользящий час. Цель — не менее 90%.", [(title, queries[key])], number=True, percent=True)
        positions[key] = (4 * i, 10, 4, 4)
    panels["history"] = panel(
        "SLI за скользящий час: история", "Каждая точка использует предшествующий час.",
        [(title, queries[key]) for key, title in titles.items()], percent=True,
    )
    positions["history"] = (0, 14, 12, 8)
    buckets = []
    for mode, title in MODES.items():
        for boundary in ("5", "30", "60", "+Inf"):
            label = f"{title}: все длительности" if boundary == "+Inf" else f"{title} ≤ {boundary} с"
            labels = 'mode=' + json.dumps(mode) + ',le=' + json.dumps(boundary)
            buckets.append((label, "sum by (environment, node) (" + selector(BUCKET, labels) + ")"))
    panels["buckets"] = panel(
        "Накопительные корзины успешных длительностей",
        "Перекрывающиеся корзины нельзя складывать. Измерение требует известных начала, варианта и корректной длительности.",
        buckets,
    )
    positions["buckets"] = (0, 22, 6, 10)
    panels["errors"] = panel(
        "Технические ошибки по вариантам: скользящие 5 минут",
        "Оценка прироста ошибок с известным вариантом за предшествующие 5 минут. Дробные значения допустимы.",
        [(title, increase(ERRORS, f'mode="{mode}"', "5m")) for mode, title in MODES.items()],
    )
    positions["errors"] = (6, 22, 6, 10)
    return dashboard("mis-login-sli", "Вход МИС: SLI и длительности", panels, positions)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="проверить актуальность JSON без записи")
    args = parser.parse_args()
    for name, data in (("auth.json", outcomes_dashboard()), ("login-journey.json", sli_dashboard())):
        path = ROOT / name
        content = json.dumps(data, ensure_ascii=False, indent=2) + "\n"
        if args.check:
            if not path.exists() or path.read_text(encoding="utf-8") != content:
                raise SystemExit(f"Требуется пересборка: {path.name}")
        else:
            path.write_text(content, encoding="utf-8")
        print(f"Готово: {path.name}")


if __name__ == "__main__":
    main()

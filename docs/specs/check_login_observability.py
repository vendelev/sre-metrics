"""Validate specification artifacts, not the unimplemented Go service.

Run from any directory using the dependency command in login-observability.md.
Prometheus rule evaluation is a separate promtool test, not simulated here.
"""

from copy import deepcopy
import json
from pathlib import Path
import re

from jsonschema import Draft202012Validator, FormatChecker
from openapi_spec_validator import validate
import promql_parser
from referencing import Registry, Resource
from referencing.jsonschema import DRAFT202012
import yaml


ROOT = Path(__file__).resolve().parent


class UniqueKeyLoader(yaml.SafeLoader):
    pass


def unique_mapping(loader, node):
    result = {}
    for key_node, value_node in node.value:
        key = loader.construct_object(key_node)
        if key in result:
            raise ValueError(f"Duplicate YAML key: {key}")
        result[key] = loader.construct_object(value_node)
    return result


UniqueKeyLoader.add_constructor(
    yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, unique_mapping
)


def load_yaml(name):
    return yaml.load((ROOT / name).read_text(), Loader=UniqueKeyLoader)


def main():
    api = load_yaml("login-observability.openapi.yml")
    validate(api)
    print("PASS OpenAPI 3.1.1 document validation")

    uri = "urn:mis-login-observability:openapi"
    registry = Registry().with_resource(
        uri, Resource.from_contents(api, default_specification=DRAFT202012)
    )

    def validator(pointer):
        return Draft202012Validator(
            {"$ref": uri + pointer},
            registry=registry,
            format_checker=FormatChecker(),
        )

    event_validator = validator("#/components/schemas/LoginEvent")
    operation = api["paths"]["/events"]["post"]
    examples = operation["requestBody"]["content"]["application/json"]["examples"]
    accepted = []
    for example in examples.values():
        event = deepcopy(example["value"])
        event_validator.validate(event)
        accepted.append(event)

    for kind in ("login_failed", "login_rejected"):
        for source in ("mis", "auth"):
            event = deepcopy(next(e for e in accepted if e["type"] == kind))
            event["source"] = source
            event_validator.validate(event)

    uppercase = deepcopy(accepted[0])
    uppercase["event_id"] = "019A0000-AAAA-7AAA-8AAA-AAAAAAAAAAAA"
    event_validator.validate(uppercase)

    rejected = 0

    def must_reject(value):
        nonlocal rejected
        if event_validator.is_valid(value):
            raise AssertionError(f"Schema incorrectly accepts: {value!r}")
        rejected += 1

    for event in accepted:
        for field in ("version", "event_id", "scenario_id", "occurred_at", "source", "type"):
            missing = deepcopy(event)
            del missing[field]
            must_reject(missing)
            null_field = deepcopy(event)
            null_field[field] = None
            must_reject(null_field)
        for field in ("user_id", "state", "reason", "unexpected"):
            extra = deepcopy(event)
            extra[field] = "not part of the contract"
            must_reject(extra)
        if event["type"] != "login_mode_determined":
            extra_mode = deepcopy(event)
            extra_mode["mode"] = "credentials"
            must_reject(extra_mode)

    base = accepted[0]
    for field in ("event_id", "scenario_id"):
        for value in (
            "019a0000-0000-4000-8000-000000000001",
            "019a0000-0000-7000-0000-000000000001",
            "not-a-uuid",
            "",
            1,
        ):
            invalid = deepcopy(base)
            invalid[field] = value
            must_reject(invalid)

    for field, values in {
        "version": (0, 2, "1", True),
        "source": ("auth", "enterprise", "unknown"),
        "type": ("login_expired", "unknown", ""),
        "occurred_at": (
            "2026-02-30T12:00:00.000Z",
            "2026-10-31T25:00:00.000Z",
            "2026-10-31T12:00:00Z",
            "2026-10-31T12:00:00.000+00:00",
            "2026-10-31T12:00:00.0000Z",
        ),
    }.items():
        for value in values:
            invalid = deepcopy(base)
            invalid[field] = value
            must_reject(invalid)

    mode_event = deepcopy(examples["credentials"]["value"])
    for value in ("unknown", "", None):
        invalid = deepcopy(mode_event)
        invalid["mode"] = value
        must_reject(invalid)
    del mode_event["mode"]
    must_reject(mode_event)
    for value in (None, [], [base], "event", 1):
        must_reject(value)

    # Schema is independent of dynamic age checks. Examples use an explicit test clock.
    encoded = json.dumps(base).encode()
    assert len(encoded) < 4096
    assert len(encoded + b" " * (4096 - len(encoded))) == 4096
    event_validator.validate(json.loads(encoded))
    assert operation["x-default-max-body-bytes"] == 4096
    print(f"PASS request examples and {rejected} invalid schema cases")

    error_count = 0
    expected_codes = {
        "400": "invalid_event", "405": "method_not_allowed",
        "409": "event_conflict", "410": "scenario_expired",
        "413": "payload_too_large", "415": "unsupported_media_type",
        "500": "internal_error", "503": "capacity_exceeded",
    }
    assert "content" not in operation["responses"]["204"]
    for status, code in expected_codes.items():
        response_pointer = operation["responses"][status]["$ref"]
        response = api
        for part in response_pointer[2:].split("/"):
            response = response[part]
        schema_pointer = response_pointer + "/content/application~1json/schema"
        check = validator(schema_pointer)
        check.validate(response["content"]["application/json"]["example"])
        check.validate({"error": code})
        assert not check.is_valid({"error": code, "message": "private detail"})
        for other in expected_codes.values():
            if other != code:
                assert not check.is_valid({"error": other}), (status, other)
        error_count += 1
    allow = api["components"]["responses"]["MethodNotAllowed"]["headers"]["Allow"]
    assert allow["required"] and allow["schema"]["const"] == "POST"
    print(f"PASS {error_count} error response schemas, 204 and Allow header")

    rules = load_yaml("login-observability.prometheus.rules.yml")
    seen = set()
    for group in rules["groups"]:
        for rule in group["rules"]:
            name = rule["record"]
            assert name not in seen
            promql_parser.parse(rule["expr"])
            dependencies = set(re.findall(r"mis_login:[a-z_]+:[a-z0-9]+", rule["expr"]))
            assert dependencies <= seen, (name, dependencies - seen)
            seen.add(name)
    rule_tests = load_yaml("login-observability.prometheus.test.yml")
    assert rule_tests["rule_files"] == ["login-observability.prometheus.rules.yml"]
    for test in rule_tests["tests"]:
        for case in test["promql_expr_test"]:
            promql_parser.parse(case["expr"])
    print(f"PASS syntax and dependency order of {len(seen)} PromQL rules")
    print(f"PASS YAML and expression syntax of {len(rule_tests['tests'])} promtool test cases")
    print("NOT RUN: promtool execution, Go HTTP/domain tests, live OTLP/backend integration")

    document = (ROOT / "login-observability.md").read_text()
    for prefix in ("BR", "AC", "OQ"):
        identifiers = re.findall(r"^\| (" + prefix + r"-\d+) \|", document, re.MULTILINE)
        assert len(identifiers) == len(set(identifiers)), prefix
    rules_defined = set(re.findall(r"^\| (BR-\d+) \|", document, re.MULTILINE))
    cases_defined = set(re.findall(r"^\| (AC-\d+) \|", document, re.MULTILINE))
    covered = set()
    for line in document.splitlines():
        if re.match(r"^\| AC-\d+ \|", line):
            references = line.split("|")[2]
            covered.update(re.findall(r"BR-\d+", references))
            for start, end in re.findall(r"BR-(\d+)–BR-(\d+)", references):
                covered.update(f"BR-{number:02}" for number in range(int(start), int(end) + 1))
    assert covered == rules_defined, (rules_defined - covered, covered - rules_defined)
    assert set(re.findall(r"BR-\d+", document)) <= rules_defined
    assert set(re.findall(r"AC-\d+", document)) <= cases_defined
    for target in re.findall(r"\[[^\]]*\]\(([^)]+)\)", document):
        if "://" not in target:
            assert (ROOT / target.split("#")[0]).is_file(), target
    assert document.count("```") % 2 == 0
    print("PASS unique specification IDs, local document links and code fences")
    print(f"PASS traceability: {len(rules_defined)} rules / {len(cases_defined)} acceptance scenarios")


if __name__ == "__main__":
    main()

#!/usr/bin/env python3

import argparse
import json
import os
import pathlib
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request


ROOT = pathlib.Path(__file__).resolve().parent

COMPOSE_FILE = ROOT / "postgres" / "compose.yaml"
COMPOSE_CI_FILE = ROOT / "postgres" / "compose.ci.yaml"
COMPOSE_IMAGE = "sessionio-postgres:18.4-dev"
COMPOSE_URL = "postgresql://sessionio:sessionio-dev@127.0.0.1:5433/sessionio"
CONTAINER_URL = "postgresql://sessionio:sessionio-dev@127.0.0.1:5432/sessionio"
ENDPOINT_ENV = "SESSIONIO_TEST_DATABASE_URL"

POSTGRES_MAJOR = 18
VECTOR_VERSION = "0.8.5"
TEXTSEARCH_VERSION = "1.3.1"

ACCEPTANCE_ROOT = ROOT / "testdata" / "acceptance"
MANIFEST_SCHEMA = "sessionio.acceptance-manifest/v1"

RELEASE_SYSTEMS = ("darwin", "linux", "windows")
RELEASE_ARCHITECTURES = ("amd64", "arm64")

OPENROUTER_CATALOG_URL = "https://openrouter.ai/api/v1/models/{model}/endpoints"
OPENROUTER_EMBEDDINGS_URL = "https://openrouter.ai/api/v1/embeddings"
OPENROUTER_KEY_ENV = "OPENROUTER_API_KEY"
OPENROUTER_INPUT = ["query: session search probe", "passage: session retrieval probe"]
OPENROUTER_CONTEXT = 512
OPENROUTER_DIMENSIONS = 1024
HTTP_TIMEOUT = 30.0

ADVERSARIAL_CASES = ("missing-preload", "health-failure")

# "nastroyka indeksa zavershena": Russian BM25 probe row, escaped to keep this file ASCII.
RUSSIAN_PROBE = (
    "\u043d\u0430\u0441\u0442\u0440\u043e\u0439\u043a\u0430"
    " \u0438\u043d\u0434\u0435\u043a\u0441\u0430"
    " \u0437\u0430\u0432\u0435\u0440\u0448\u0435\u043d\u0430"
)

PROBE_SQL = """
SELECT json_build_object(
    'server_version_num', current_setting('server_version_num')::bigint,
    'shared_preload_libraries',
        (SELECT setting FROM pg_settings WHERE name = 'shared_preload_libraries'),
    'pg_textsearch_library_version',
        (SELECT setting FROM pg_settings WHERE name = 'pg_textsearch.library_version'),
    'installed_extensions', COALESCE(
        (SELECT json_object_agg(extname, extversion) FROM pg_extension
            WHERE extname IN ('vector', 'pg_trgm', 'pg_textsearch')),
        '{}'::json),
    'available_extensions', COALESCE(
        (SELECT json_object_agg(name, default_version) FROM pg_available_extensions
            WHERE name IN ('vector', 'pg_trgm', 'pg_textsearch')),
        '{}'::json)
)
"""

SMOKE_SQL = """
SET client_encoding TO 'UTF8';
BEGIN;
CREATE SCHEMA {schema};
SET LOCAL search_path TO {schema}, public;

CREATE TABLE probe (
    id integer PRIMARY KEY,
    body text NOT NULL,
    embedding vector(4) NOT NULL
);

INSERT INTO probe (id, body, embedding) VALUES
    (1, 'PostgreSQL streaming replication keeps the standby in sync', '[1,0,0,0]'),
    (2, '{russian}', '[0,1,0,0]'),
    (3, 'ECONNRESET: socket hang up (errno=-54)', '[0,0,1,0]'),
    (4, 'Exact literal containment probe', '[0.9,0.1,0,0]');

CREATE INDEX probe_bm25 ON probe USING bm25 (body) WITH (text_config='english');

DO $$
DECLARE
    best integer;
BEGIN
    SELECT id INTO best FROM probe ORDER BY body <@> 'streaming replication' LIMIT 1;
    IF best IS DISTINCT FROM 1 THEN
        RAISE EXCEPTION 'bm25 relevance query returned id %, expected 1', best;
    END IF;
END
$$;

CREATE INDEX probe_trgm ON probe USING gin (body gin_trgm_ops);

DO $$
DECLARE
    sensitive integer;
    insensitive integer;
BEGIN
    SELECT count(*) INTO sensitive FROM probe WHERE body LIKE '%Exact%';
    SELECT count(*) INTO insensitive FROM probe WHERE body LIKE '%exact%';
    IF sensitive <> 1 THEN
        RAISE EXCEPTION 'case-sensitive literal matched % rows, expected 1', sensitive;
    END IF;
    IF insensitive <> 0 THEN
        RAISE EXCEPTION 'case-insensitive literal matched % rows, expected 0', insensitive;
    END IF;
END
$$;

DO $$
DECLARE
    nearest integer;
BEGIN
    SELECT id INTO nearest FROM probe ORDER BY embedding <=> '[1,0,0,0]' LIMIT 1;
    IF nearest IS DISTINCT FROM 1 THEN
        RAISE EXCEPTION 'exact cosine nearest neighbour was id %, expected 1', nearest;
    END IF;
END
$$;

CREATE INDEX probe_hnsw ON probe USING hnsw (embedding vector_cosine_ops);
SET LOCAL enable_seqscan = off;

DO $$
DECLARE
    plan json;
BEGIN
    EXECUTE 'EXPLAIN (FORMAT JSON) SELECT id FROM probe'
        || ' ORDER BY embedding <=> ''[1,0,0,0]'' LIMIT 1'
        INTO plan;
    IF position('probe_hnsw' in plan::text) = 0 THEN
        RAISE EXCEPTION 'hnsw index was not used by the nearest-neighbour plan: %', plan::text;
    END IF;
END
$$;

ROLLBACK;
"""

LEFTOVER_SQL = (
    "SELECT count(*) FROM pg_namespace WHERE nspname LIKE 'sessionio\\_smoke%'"
)


class DevError(Exception):
    pass


def run(command: list[str]) -> int:
    print("+", " ".join(command), flush=True)
    return subprocess.run(command, cwd=ROOT, check=False).returncode


def capture(command: list[str], stdin: str | None = None) -> subprocess.CompletedProcess:
    return subprocess.run(
        command,
        cwd=ROOT,
        check=False,
        text=True,
        encoding="utf-8",
        input=stdin,
        capture_output=True,
    )


def go_files() -> list[str]:
    return sorted(
        str(path.relative_to(ROOT))
        for path in ROOT.rglob("*.go")
        if ".git" not in path.parts and "dist" not in path.parts
    )


def gofmt_check() -> int:
    files = go_files()
    if not files:
        return 0
    print("+ gofmt -l", *files, flush=True)
    result = subprocess.run(
        ["gofmt", "-l", *files],
        cwd=ROOT,
        check=False,
        text=True,
        capture_output=True,
    )
    if result.stderr:
        print(result.stderr, end="")
    if result.returncode != 0:
        return result.returncode
    if result.stdout:
        print("gofmt required:")
        print(result.stdout, end="")
        return 1
    return 0


def tackbox_changed() -> int:
    print("+ git status --porcelain", flush=True)
    result = subprocess.run(
        ["git", "status", "--porcelain"],
        cwd=ROOT,
        check=False,
        text=True,
        capture_output=True,
    )
    if result.stderr:
        print(result.stderr, end="")
    if result.returncode != 0:
        return result.returncode
    if not result.stdout:
        return 0
    return run(["uvx", "tackbox@latest", "lint", "--changed", "."])


def lint() -> int:
    results = [
        gofmt_check(),
        run(["go", "vet", "./..."]),
        run(
            [
                "go",
                "run",
                "github.com/rhysd/actionlint/cmd/actionlint@v1.7.12",
            ]
        ),
        run(["uvx", "tackbox@latest", "lint", "."]),
        tackbox_changed(),
    ]
    return int(any(results))


def test() -> int:
    return run(["go", "test", "./..."])


def e2e() -> int:
    return run(["go", "test", "./...", "-run", "^TestE2E"])


def require_docker() -> None:
    if shutil.which("docker") is None:
        raise DevError("docker is required for the PostgreSQL profile but is not on PATH")
    result = capture(["docker", "version", "--format", "{{.Server.Version}}"])
    if result.returncode != 0:
        raise DevError("docker daemon is not reachable: " + result.stderr.strip())


def compose_argv(*arguments: str) -> list[str]:
    argv = ["docker", "compose", "-f", str(COMPOSE_FILE)]
    if os.environ.get("CI"):
        argv += ["-f", str(COMPOSE_CI_FILE)]
    return argv + list(arguments)


def compose_build() -> None:
    require_docker()
    if run(compose_argv("build")) != 0:
        raise DevError("docker compose build failed")


def compose_up() -> None:
    require_docker()
    argv = compose_argv("up", "-d", "--build", "--wait", "--wait-timeout", "300")
    if run(argv) != 0:
        raise DevError("docker compose up did not reach a healthy PostgreSQL")


def endpoint_url() -> str | None:
    url = os.environ.get(ENDPOINT_ENV)
    if not url:
        return None
    if not url.startswith("postgresql://"):
        raise DevError(f"{ENDPOINT_ENV} must be a postgresql:// URL, got {url!r}")
    return url


def psql_prefix() -> list[str]:
    url = endpoint_url()
    if url is None:
        compose_up()
        # psql runs inside the container, so it uses the container port, not the published one.
        return compose_argv("exec", "-T", "postgres", "psql", CONTAINER_URL)
    # Adversarial cases always need the image, so build it even for an explicit endpoint.
    compose_build()
    psql = shutil.which("psql")
    if psql is None:
        raise DevError(f"{ENDPOINT_ENV} is set but psql was not found on PATH")
    return [psql, url]


def psql_argv(prefix: list[str], *arguments: str) -> list[str]:
    return [*prefix, "-X", "-A", "-t", "-v", "ON_ERROR_STOP=1", *arguments]


def probe_facts(prefix: list[str]) -> dict:
    result = capture(psql_argv(prefix, "-c", PROBE_SQL))
    if result.returncode != 0:
        raise DevError("postgres probe failed: " + (result.stderr.strip() or result.stdout.strip()))
    return json.loads(result.stdout.strip())


def extension_problems(installed: dict, name: str, expected: str) -> list[str]:
    found = installed.get(name)
    if found is None:
        return [f"extension {name} is not installed"]
    if found != expected:
        return [f"extension {name} version {found} is installed, {expected} is required"]
    return []


def textsearch_preloaded(facts: dict) -> bool:
    libraries = facts.get("shared_preload_libraries")
    if libraries is not None:
        return "pg_textsearch" in [item.strip() for item in libraries.split(",")]
    # Non-superusers cannot read shared_preload_libraries; this GUC exists only when
    # pg_textsearch was preloaded, because its _PG_init rejects every other load path.
    return facts.get("pg_textsearch_library_version") is not None


def validate_facts(facts: dict) -> list[str]:
    version = facts.get("server_version_num")
    if not isinstance(version, int):
        return ["probe facts carry no integer server_version_num"]
    major = version // 10000
    if major != POSTGRES_MAJOR:
        return [f"postgres major {major} is unsupported, sessionio requires major {POSTGRES_MAJOR}"]
    if not textsearch_preloaded(facts):
        return [
            "pg_textsearch is absent from shared_preload_libraries;"
            " preload it and restart postgres"
        ]
    installed = facts.get("installed_extensions") or {}
    problems = extension_problems(installed, "vector", VECTOR_VERSION)
    problems += extension_problems(installed, "pg_textsearch", TEXTSEARCH_VERSION)
    if "pg_trgm" not in installed:
        problems.append("extension pg_trgm is not installed")
    return problems


def facts_summary(facts: dict) -> str:
    installed = facts.get("installed_extensions") or {}
    return (
        f"server_version_num={facts.get('server_version_num')}"
        f" vector={installed.get('vector')}"
        f" pg_textsearch={installed.get('pg_textsearch')}"
        f" pg_trgm={installed.get('pg_trgm')}"
    )


def functional_problems(prefix: list[str]) -> list[str]:
    schema = f"sessionio_smoke_tmp_{os.getpid()}"
    script = SMOKE_SQL.format(schema=schema, russian=RUSSIAN_PROBE)
    result = capture(psql_argv(prefix, "-f", "-"), stdin=script)
    if result.returncode != 0:
        return ["functional smoke failed: " + (result.stderr.strip() or result.stdout.strip())]
    leftover = capture(psql_argv(prefix, "-c", LEFTOVER_SQL))
    if leftover.returncode != 0:
        return ["leftover schema check failed: " + leftover.stderr.strip()]
    if leftover.stdout.strip() != "0":
        return [f"smoke schemas survived the rollback: {leftover.stdout.strip()} found"]
    return []


def report_smoke(facts: dict, problems: list[str]) -> int:
    if problems:
        print("pg capability smoke: FAIL")
        for problem in problems:
            print(problem)
        return 1
    print("pg capability smoke: PASS")
    print(facts_summary(facts))
    return 0


def pg_smoke(probe_fixture: str | None) -> int:
    if probe_fixture is not None:
        path = pathlib.Path(probe_fixture)
        if not path.is_absolute():
            path = ROOT / path
        facts = json.loads(path.read_text(encoding="utf-8"))
        return report_smoke(facts, validate_facts(facts))
    prefix = psql_prefix()
    facts = probe_facts(prefix)
    problems = validate_facts(facts)
    if not problems:
        problems = functional_problems(prefix)
    return report_smoke(facts, problems)


def pg_up() -> int:
    compose_up()
    print(f"postgres endpoint: {COMPOSE_URL}")
    return 0


def pg_down() -> int:
    require_docker()
    if run(compose_argv("down")) != 0:
        raise DevError("docker compose down failed")
    return 0


def container_remove(name: str) -> None:
    capture(["docker", "rm", "-f", name])


def container_start(name: str, port: int, command: list[str]) -> None:
    # No --rm: an exited container must keep its logs until the case has read them.
    container_remove(name)
    argv = [
        "docker",
        "run",
        "-d",
        "--name",
        name,
        "-e",
        "POSTGRES_USER=sessionio",
        "-e",
        "POSTGRES_PASSWORD=sessionio-dev",
        "-e",
        "POSTGRES_DB=sessionio",
        "-e",
        "POSTGRES_INITDB_ARGS=--auth-host=scram-sha-256",
        "-p",
        f"127.0.0.1:{port}:5432",
        COMPOSE_IMAGE,
        *command,
    ]
    print("+", " ".join(argv), flush=True)
    result = capture(argv)
    if result.returncode != 0:
        raise DevError(f"docker run for {name} failed: " + result.stderr.strip())


def container_prefix(name: str) -> list[str]:
    return ["docker", "exec", "-i", name, "psql", CONTAINER_URL]


def container_running(name: str) -> bool:
    result = capture(["docker", "inspect", "-f", "{{.State.Status}}", name])
    return result.returncode == 0 and result.stdout.strip() == "running"


def container_logs(name: str) -> str:
    result = capture(["docker", "logs", "--tail", "40", name])
    return (result.stdout + result.stderr).strip()


def wait_for_tcp(name: str, timeout: float) -> bool:
    # The entrypoint runs initdb behind a socket-only server, so only a real TCP query is ready.
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if capture(psql_argv(container_prefix(name), "-c", "SELECT 1")).returncode == 0:
            return True
        if not container_running(name):
            return False
        time.sleep(1.0)
    return False


def report_adversarial(case: str, problems: list[str], evidence: str) -> int:
    if problems:
        print(f"adversarial {case}: FAIL")
        for problem in problems:
            print(problem)
        return 1
    print(f"adversarial {case}: PASS")
    print(evidence)
    return 0


def adversarial_missing_preload() -> int:
    name = "sessionio-adversarial-missing-preload"
    # No initdb mount here: CREATE EXTENSION pg_textsearch refuses to run without the
    # preload, which would fail container init instead of reaching the capability check.
    try:
        container_start(name, 5434, [])
        if not wait_for_tcp(name, 90.0):
            return report_adversarial(
                "missing-preload",
                ["container never accepted TCP connections:\n" + container_logs(name)],
                "",
            )
        problems = validate_facts(probe_facts(container_prefix(name)))
        if not problems:
            return report_adversarial(
                "missing-preload", ["capability validation passed without the preload"], ""
            )
        if "preload" not in problems[0]:
            return report_adversarial(
                "missing-preload", ["first failure is not the preload failure: " + problems[0]], ""
            )
        return report_adversarial("missing-preload", [], problems[0])
    finally:
        container_remove(name)


def adversarial_health_failure() -> int:
    name = "sessionio-adversarial-health-failure"
    command = ["postgres", "-c", "shared_preload_libraries=sessionio_nonexistent"]
    try:
        container_start(name, 5435, command)
        if wait_for_tcp(name, 15.0):
            return report_adversarial(
                "health-failure", ["container became ready with an unloadable preload library"], ""
            )
        logs = container_logs(name)
        if "sessionio_nonexistent" not in logs:
            return report_adversarial(
                "health-failure", ["container logs do not report the failing preload:\n" + logs], ""
            )
        return report_adversarial(
            "health-failure",
            [],
            "readiness wait failed within 15s and the logs report the missing preload library",
        )
    finally:
        container_remove(name)


def pg_adversarial(case: str) -> int:
    require_docker()
    compose_build()
    if case == "missing-preload":
        return adversarial_missing_preload()
    if case == "health-failure":
        return adversarial_health_failure()
    raise DevError(f"unknown adversarial case {case!r}")


def case_report(case: dict, result: subprocess.CompletedProcess, problems: list[str]) -> str:
    lines = [f"case {case['name']}: " + "; ".join(problems)]
    lines.append("argv: " + " ".join(case["argv"]))
    lines.append(f"exit: {result.returncode}")
    lines.append("stdout:\n" + result.stdout.strip())
    lines.append("stderr:\n" + result.stderr.strip())
    return "\n".join(lines)


def run_case(case: dict) -> str | None:
    environment = dict(os.environ)
    environment.update(case.get("env", {}))
    print("+", " ".join(case["argv"]), flush=True)
    result = subprocess.run(
        case["argv"],
        cwd=ROOT,
        check=False,
        text=True,
        encoding="utf-8",
        capture_output=True,
        env=environment,
    )
    expect = case["expect"]
    problems = []
    if result.returncode != expect["exit"]:
        problems.append(f"exit {result.returncode}, expected {expect['exit']}")
    for fragment in expect.get("stdout_contains", []):
        if fragment not in result.stdout:
            problems.append(f"stdout lacks {fragment!r}")
    for fragment in expect.get("stderr_contains", []):
        if fragment not in result.stderr:
            problems.append(f"stderr lacks {fragment!r}")
    if not problems:
        return None
    return case_report(case, result, problems)


def run_manifest(path: pathlib.Path) -> int:
    manifest = json.loads(path.read_text(encoding="utf-8"))
    if manifest.get("schema") != MANIFEST_SCHEMA:
        raise DevError(f"{path}: schema {manifest.get('schema')!r} is not {MANIFEST_SCHEMA}")
    name = manifest["name"]
    for case in manifest["cases"]:
        failure = run_case(case)
        if failure is not None:
            print(f"acceptance {name}: FAIL")
            print(failure)
            return 1
    print(f"acceptance {name}: PASS")
    return 0


def acceptance() -> int:
    manifests = sorted(ACCEPTANCE_ROOT.glob("*/manifest.json"))
    if not manifests:
        raise DevError(f"no acceptance manifests under {ACCEPTANCE_ROOT}")
    # Warm the endpoint once so every case finds a built image and a healthy server.
    psql_prefix()
    results = [run_manifest(path) for path in manifests]
    return int(any(results))


def release_build() -> int:
    failures = 0
    with tempfile.TemporaryDirectory() as directory:
        for system in RELEASE_SYSTEMS:
            for architecture in RELEASE_ARCHITECTURES:
                suffix = ".exe" if system == "windows" else ""
                output = pathlib.Path(directory) / f"sessionio_{system}_{architecture}{suffix}"
                environment = dict(
                    os.environ,
                    CGO_ENABLED="0",
                    GOOS=system,
                    GOARCH=architecture,
                )
                result = subprocess.run(
                    ["go", "build", "-trimpath", "-o", str(output), "./cmd/sessionio"],
                    cwd=ROOT,
                    check=False,
                    text=True,
                    capture_output=True,
                    env=environment,
                )
                if result.returncode == 0:
                    print(f"release build {system}/{architecture}: PASS")
                    continue
                print(f"release build {system}/{architecture}: FAIL")
                print(result.stderr.strip())
                failures += 1
    return int(failures > 0)


def check() -> int:
    # acceptance and release-build run as child commands so one failure still runs the rest.
    results = [
        lint(),
        test(),
        run([sys.executable, "dev.py", "acceptance"]),
        run([sys.executable, "dev.py", "release-build"]),
    ]
    if any(results):
        return 1
    print("sessionio check: PASS")
    return 0


def http_json(
    url: str, payload: dict | None = None, headers: dict | None = None
) -> tuple[dict, dict]:
    data = None if payload is None else json.dumps(payload).encode("utf-8")
    request = urllib.request.Request(url, data=data, headers=headers or {})
    request.add_header("Accept", "application/json")
    if data is not None:
        request.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(request, timeout=HTTP_TIMEOUT) as response:
            body = response.read().decode("utf-8")
            received = dict(response.headers)
    except urllib.error.HTTPError as error:
        excerpt = error.read().decode("utf-8", "replace")[:500]
        raise DevError(f"{url} returned HTTP {error.code}: {excerpt}") from error
    except urllib.error.URLError as error:
        raise DevError(f"{url} request failed: {error.reason}") from error
    return json.loads(body), received


def openrouter_catalog(model: str) -> None:
    payload, _ = http_json(OPENROUTER_CATALOG_URL.format(model=model))
    data = payload.get("data") or {}
    if data.get("id") != model:
        raise DevError(f"openrouter catalog returned id {data.get('id')!r}, expected {model!r}")
    modalities = (data.get("architecture") or {}).get("output_modalities") or []
    if "embeddings" not in modalities:
        raise DevError(f"{model} output modalities are {modalities}, expected embeddings")
    endpoints = data.get("endpoints") or []
    if not endpoints:
        raise DevError(f"{model} has no catalog endpoints")
    contexts = sorted({endpoint.get("context_length") for endpoint in endpoints})
    if contexts != [OPENROUTER_CONTEXT]:
        raise DevError(f"{model} context lengths are {contexts}, expected [{OPENROUTER_CONTEXT}]")
    for endpoint in endpoints:
        pricing = endpoint.get("pricing") or {}
        if not pricing.get("prompt"):
            raise DevError(f"{model} endpoint {endpoint.get('name')!r} has no prompt price")
        print(
            f"catalog endpoint provider={endpoint.get('provider_name')}"
            f" context={endpoint.get('context_length')}"
            f" prompt_price={pricing.get('prompt')}"
            f" completion_price={pricing.get('completion')}"
        )
    print(f"openrouter catalog {model}: PASS")
    # The catalog exposes no dimension field; the live gate is what proves 1024.
    print("(dimensions=not exposed by catalog, context=512, price metadata present)")


def openrouter_live(model: str) -> None:
    key = os.environ.get(OPENROUTER_KEY_ENV)
    if not key:
        raise DevError(f"{OPENROUTER_KEY_ENV} is not set, the live gate cannot run")
    payload = {
        "model": model,
        "input": OPENROUTER_INPUT,
        "provider": {"zdr": True, "data_collection": "deny"},
    }
    response, received = http_json(
        OPENROUTER_EMBEDDINGS_URL,
        payload=payload,
        headers={"Authorization": f"Bearer {key}"},
    )
    vectors = response.get("data") or []
    if len(vectors) != len(OPENROUTER_INPUT):
        raise DevError(f"live embeddings returned {len(vectors)} vectors, expected 2")
    for vector in vectors:
        size = len(vector.get("embedding") or [])
        if size != OPENROUTER_DIMENSIONS:
            raise DevError(f"live embedding has {size} dimensions, expected {OPENROUTER_DIMENSIONS}")
    provider = response.get("provider")
    if not provider:
        raise DevError(
            "live response carries no routing metadata;"
            f" body keys: {sorted(response)}; header names: {sorted(received)}"
        )
    print(f"live routing provider={provider} model={response.get('model')}")
    print(f"live usage={json.dumps(response.get('usage'))}")
    print(f"openrouter profile {model}: PASS")
    print(f"(dimensions={OPENROUTER_DIMENSIONS}, context={OPENROUTER_CONTEXT},"
          " zdr=true, data_collection=deny)")


def openrouter_profile_check(model: str | None, require_live: bool) -> int:
    if not model:
        raise DevError("openrouter-profile-check requires --model")
    openrouter_catalog(model)
    if require_live:
        openrouter_live(model)
    return 0


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "command",
        choices=(
            "lint",
            "test",
            "e2e",
            "check",
            "acceptance",
            "pg-smoke",
            "pg-adversarial",
            "pg-up",
            "pg-down",
            "openrouter-profile-check",
            "release-build",
        ),
    )
    parser.add_argument("case", nargs="?", choices=ADVERSARIAL_CASES)
    parser.add_argument("--probe-fixture")
    parser.add_argument("--model")
    parser.add_argument("--require-live", action="store_true")
    args = parser.parse_args()

    commands = {
        "lint": lint,
        "test": test,
        "e2e": e2e,
        "check": check,
        "acceptance": acceptance,
        "pg-smoke": lambda: pg_smoke(args.probe_fixture),
        "pg-adversarial": lambda: pg_adversarial(args.case),
        "pg-up": pg_up,
        "pg-down": pg_down,
        "openrouter-profile-check": lambda: openrouter_profile_check(
            args.model, args.require_live
        ),
        "release-build": release_build,
    }
    if args.command == "pg-adversarial" and args.case is None:
        parser.error("pg-adversarial requires a case: " + ", ".join(ADVERSARIAL_CASES))
    try:
        return commands[args.command]()
    except DevError as error:
        raise SystemExit(f"error: {error}") from error


if __name__ == "__main__":
    raise SystemExit(main())

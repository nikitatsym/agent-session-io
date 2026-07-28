#!/usr/bin/env python3

import argparse
import contextlib
import difflib
import hashlib
import json
import os
import pathlib
import re
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request


ROOT = pathlib.Path(__file__).resolve().parent

COMPOSE_DIR = ROOT / "postgres"
COMPOSE_FILE = COMPOSE_DIR / "compose.yaml"
COMPOSE_CI_FILE = COMPOSE_DIR / "compose.ci.yaml"
COMPOSE_IMAGE = "sessionio-postgres:18.4-dev"
CONTEXT_HASH_LABEL = "sessionio.context-hash"
CONTEXT_HASH_ENV = "SESSIONIO_PG_CONTEXT_HASH"
COMPOSE_URL = "postgresql://sessionio:sessionio-dev@127.0.0.1:5433/sessionio"
CONTAINER_URL = "postgresql://sessionio:sessionio-dev@127.0.0.1:5432/sessionio"
ENDPOINT_ENV = "SESSIONIO_TEST_DATABASE_URL"

POSTGRES_MAJOR = 18
VECTOR_VERSION = "0.8.5"
TEXTSEARCH_VERSION = "1.3.1"

ACCEPTANCE_ROOT = ROOT / "testdata" / "acceptance"
MANIFEST_SCHEMA = "sessionio.acceptance-manifest/v1"
ACCEPTANCE_ENDPOINT_ENV = "SESSIONIO_ACCEPTANCE_DATABASE_URL"
CACHE_DIR_ENV = "SESSIONIO_CACHE_DIR"
ACCEPTANCE_BINARY = ROOT / "sessionio"

SCHEMA_NAME = re.compile(r"^[a-z_][a-z0-9_]{0,62}$")

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


def run(command: list[str], environment: dict[str, str] | None = None) -> int:
    print("+", " ".join(command), flush=True)
    return subprocess.run(
        command, cwd=ROOT, check=False, env=environment
    ).returncode


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
    with cache_directory() as environment:
        return run(["go", "test", "./..."], environment)


def e2e() -> int:
    with cache_directory() as environment:
        return run(["go", "test", "./...", "-run", "^TestE2E"], environment)


def pg_test() -> int:
    # The compose endpoint is always required: privilege cases need a superuser.
    compose_up()
    with cache_directory() as environment:
        environment.update(
            SESSIONIO_TEST_DATABASE_URL=primary_endpoint(),
            SESSIONIO_TEST_COMPOSE_DATABASE_URL=COMPOSE_URL,
        )
        # One PostgreSQL server serves every package, and whether a vacuum may
        # remove a reclaimed row depends on the oldest snapshot anywhere in it,
        # so the packages run one at a time.
        if run(
            ["go", "test", "-tags", "pgintegration", "-p", "1", "./..."],
            environment,
        ) != 0:
            return 1
    print("pg integration: PASS")
    return 0


@contextlib.contextmanager
def cache_directory():
    """Point the reader listing cache at a per-run directory.

    Nothing under check may read or write the user cache directory.
    """
    with tempfile.TemporaryDirectory(prefix="sessionio-cache-") as directory:
        yield dict(os.environ, **{CACHE_DIR_ENV: directory})


def pg_drop_schema(name: str | None) -> int:
    if not name:
        raise DevError("pg-drop-schema requires a schema name")
    if not name.startswith("sessionio_") or not SCHEMA_NAME.match(name):
        raise DevError(
            f"pg-drop-schema refuses {name!r}: expected a sessionio_ prefixed identifier"
        )
    prefix = psql_prefix()
    result = capture(psql_argv(prefix, "-c", f"DROP SCHEMA IF EXISTS {name} CASCADE"))
    if result.returncode != 0:
        raise DevError(
            f"drop schema {name} failed: "
            + (result.stderr.strip() or result.stdout.strip())
        )
    print(f"dropped schema {name}")
    return 0


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


def context_hash() -> str:
    digest = hashlib.sha256()
    for path in sorted(COMPOSE_DIR.rglob("*")):
        if path.is_file():
            digest.update(path.relative_to(COMPOSE_DIR).as_posix().encode())
            digest.update(b"\x00")
            digest.update(path.read_bytes())
            digest.update(b"\x00")
    return digest.hexdigest()


def image_context_hash() -> str | None:
    template = '{{index .Config.Labels "' + CONTEXT_HASH_LABEL + '"}}'
    result = capture(["docker", "image", "inspect", "--format", template, COMPOSE_IMAGE])
    if result.returncode != 0:
        return None
    return result.stdout.strip() or None


def compose_build() -> None:
    require_docker()
    # Rebuild only when postgres/ changed: repeat runs must stay off the network.
    wanted = context_hash()
    if image_context_hash() == wanted:
        return
    environment = dict(os.environ, **{CONTEXT_HASH_ENV: wanted})
    if run(compose_argv("build"), environment) != 0:
        raise DevError("docker compose build failed")


def compose_up() -> None:
    compose_build()
    argv = compose_argv("up", "-d", "--wait", "--wait-timeout", "300")
    if run(argv) != 0:
        raise DevError("docker compose up did not reach a healthy PostgreSQL")


def primary_endpoint() -> str:
    url = endpoint_url()
    if url is None:
        compose_up()
        return COMPOSE_URL
    return url


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


def run_case(case: dict, endpoint: str) -> str | None:
    environment = dict(os.environ)
    environment[ACCEPTANCE_ENDPOINT_ENV] = endpoint
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


def run_stage(name: str, stage: str, stage_argv: list, endpoint: str) -> str | None:
    # A stage is one argv, or a list of argvs run in order.
    commands = stage_argv if isinstance(stage_argv[0], list) else [stage_argv]
    for index, argv in enumerate(commands):
        failure = run_case(
            {
                "name": f"{name}-{stage}-{index}",
                "argv": argv,
                "expect": {"exit": 0},
            },
            endpoint,
        )
        if failure is not None:
            return failure
    return None


def run_manifest(path: pathlib.Path, endpoint: str) -> int:
    manifest = json.loads(path.read_text(encoding="utf-8"))
    if manifest.get("schema") != MANIFEST_SCHEMA:
        raise DevError(f"{path}: schema {manifest.get('schema')!r} is not {MANIFEST_SCHEMA}")
    name = manifest["name"]
    failure = None
    setup = manifest.get("setup")
    if setup:
        failure = run_stage(name, "setup", setup, endpoint)
    if failure is None:
        for case in manifest["cases"]:
            failure = run_case(case, endpoint)
            if failure is not None:
                break
    teardown = manifest.get("teardown")
    if teardown:
        # Teardown always runs, so a failed case never leaks catalog state.
        teardown_failure = run_stage(name, "teardown", teardown, endpoint)
        failure = failure or teardown_failure
    if failure is not None:
        print(f"acceptance {name}: FAIL")
        print(failure)
        return 1
    print(f"acceptance {name}: PASS")
    return 0


def build_acceptance_binary() -> None:
    # go run collapses every child exit status to 1, so exit-status cases
    # invoke this binary directly.
    if run(["go", "build", "-o", str(ACCEPTANCE_BINARY), "./cmd/sessionio"]) != 0:
        raise DevError("building the acceptance binary failed")


def acceptance() -> int:
    manifests = sorted(ACCEPTANCE_ROOT.glob("*/manifest.json"))
    if not manifests:
        raise DevError(f"no acceptance manifests under {ACCEPTANCE_ROOT}")
    build_acceptance_binary()
    # Warm the endpoint once so every case finds a built image and a healthy server.
    psql_prefix()
    endpoint = primary_endpoint()
    with tempfile.TemporaryDirectory(prefix="sessionio-cache-") as directory:
        os.environ[CACHE_DIR_ENV] = directory
        try:
            results = [run_manifest(path, endpoint) for path in manifests]
        finally:
            del os.environ[CACHE_DIR_ENV]
    return int(any(results))


STATE_TOKEN = re.compile(r'"([A-Za-z0-9+/]{16,}={0,2})"')
TEMP_STREAM = re.compile(r"^sessionio-[a-z0-9.-]+$")


def remove_temp(path: str | None) -> int:
    """Remove one acceptance stream so a refuse-to-overwrite case can rerun."""
    if not path:
        raise DevError("remove-temp requires a path")
    target = pathlib.Path(path)
    if not target.is_absolute() or not TEMP_STREAM.match(target.name):
        raise DevError(f"remove-temp refuses {path!r}: expected an absolute sessionio- path")
    target.unlink(missing_ok=True)
    return 0


def corrupt_state(path: str | None) -> int:
    """Flip one byte inside a state-stream payload, keeping every line valid JSON."""
    if not path:
        raise DevError("corrupt-state requires a stream path")
    target = pathlib.Path(path)
    if not target.is_absolute():
        target = ROOT / target
    lines = target.read_bytes().split(b"\n")
    if len(lines) < 2:
        raise DevError(f"{target} carries no state record to corrupt")
    for index in range(1, len(lines)):
        text = lines[index].decode("utf-8")
        match = STATE_TOKEN.search(text)
        if match is None:
            continue
        token = match.group(1)
        flipped = ("B" if token[0] == "A" else "A") + token[1:]
        lines[index] = text.replace(token, flipped, 1).encode("utf-8")
        target.write_bytes(b"\n".join(lines))
        print(f"corrupted record {index} of {target}")
        return 0
    raise DevError(f"{target} carries no corruptible payload token")


def supersede_builder(name: str | None) -> int:
    """Relabel one schema's derived rows so the next scan sees a builder bump."""
    if not name:
        raise DevError("supersede-builder requires a schema name")
    if not name.startswith("sessionio_") or not SCHEMA_NAME.match(name):
        raise DevError(
            f"supersede-builder refuses {name!r}: expected a sessionio_ prefixed identifier"
        )
    prefix = psql_prefix()
    statement = (
        f"UPDATE {name}.derived_session"
        " SET builder_key = builder_key || ';superseded'"
    )
    result = capture(psql_argv(prefix, "-c", statement))
    if result.returncode != 0:
        raise DevError(
            f"supersede builder in {name} failed: "
            + (result.stderr.strip() or result.stdout.strip())
        )
    print(f"superseded the derived builder of {name}")
    return 0


READER_CACHE_CASES = ("cold-warm", "unreadable", "corrupt", "unwritable")


def sessionio_list(config: str, *extra: str) -> subprocess.CompletedProcess:
    argv = [str(ACCEPTANCE_BINARY), "--config", config, "list", "--format", "ndjson"]
    result = subprocess.run(
        [*argv, *extra],
        cwd=ROOT,
        check=False,
        text=True,
        encoding="utf-8",
        capture_output=True,
    )
    if result.returncode != 0:
        raise DevError(
            f"{' '.join([*argv, *extra])} exited {result.returncode}: "
            + (result.stderr.strip() or result.stdout.strip())
        )
    return result


def cache_files(directory: pathlib.Path) -> list[pathlib.Path]:
    if not directory.is_dir():
        return []
    return sorted(path for path in directory.iterdir() if path.is_file())


def transcript_files(root: pathlib.Path) -> list[pathlib.Path]:
    return sorted(root.rglob("*.jsonl"))


def set_transcript_mode(root: pathlib.Path, mode: int) -> None:
    for path in transcript_files(root):
        path.chmod(mode)


def compare_listing(case: str, first: str, second: str, detail: str) -> int:
    """A listing that a cache changed by one byte fails the case."""
    if first == second:
        print(f"reader cache {case}: byte-identical ({len(first)} bytes{detail})")
        return 0
    print(f"reader cache {case}: DIFFERS ({len(first)} against {len(second)} bytes)")
    for line in difflib.unified_diff(
        first.splitlines(), second.splitlines(), "expected", "actual", lineterm="", n=0
    ):
        print(line)
    return 1


def reader_cache(case: str | None, config: str | None, cache: str | None,
                 root: str | None) -> int:
    if case not in READER_CACHE_CASES:
        raise DevError(f"reader-cache requires a case: {', '.join(READER_CACHE_CASES)}")
    if not config or not cache:
        raise DevError("reader-cache requires --config and --cache")
    directory = ROOT / cache
    shutil.rmtree(directory, ignore_errors=True)
    try:
        if case == "cold-warm":
            return reader_cache_cold_warm(config, directory)
        if case == "unreadable":
            if not root:
                raise DevError("reader-cache --case unreadable requires --root")
            return reader_cache_unreadable(config, ROOT / root)
        if case == "corrupt":
            return reader_cache_corrupt(config, directory)
        return reader_cache_unwritable(config, directory)
    finally:
        directory.chmod(0o700) if directory.is_dir() else None
        shutil.rmtree(directory, ignore_errors=True)


def reader_cache_cold_warm(config: str, directory: pathlib.Path) -> int:
    cold = sessionio_list(config).stdout
    cold_since = sessionio_list(config, "--since", "3650d").stdout
    files = len(cache_files(directory))
    warm = sessionio_list(config).stdout
    warm_since = sessionio_list(config, "--since", "3650d").stdout
    if files == 0:
        print("reader cache cold-warm: the declared cache directory stayed empty")
        return 1
    return max(
        compare_listing("cold-warm", cold, warm, f", {files} cache files"),
        compare_listing("cold-warm --since", cold_since, warm_since, ""),
    )


def reader_cache_unreadable(config: str, root: pathlib.Path) -> int:
    """A warm listing must open no transcript, so every transcript is mode 000."""
    cold = sessionio_list(config).stdout
    cold_since = sessionio_list(config, "--since", "3650d").stdout
    if not transcript_files(root):
        raise DevError(f"no transcripts under {root}")
    set_transcript_mode(root, 0o000)
    try:
        warm = sessionio_list(config).stdout
        warm_since = sessionio_list(config, "--since", "3650d").stdout
    finally:
        set_transcript_mode(root, 0o644)
    return max(
        compare_listing("unreadable", cold, warm, ", every transcript at mode 000"),
        compare_listing("unreadable --since", cold_since, warm_since, ""),
    )


def reader_cache_corrupt(config: str, directory: pathlib.Path) -> int:
    cold = sessionio_list(config).stdout
    files = cache_files(directory)
    if not files:
        raise DevError(f"no cache file under {directory}")
    for path in files:
        lines = path.read_text(encoding="utf-8").splitlines()
        lines[-1] = '{"schema":"sessionio.readercache/v0","kind":"entry"'
        path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    discarded = sessionio_list(config)
    if "reader cache: discarded" not in discarded.stderr:
        print("reader cache corrupt: no discard diagnostic on stderr")
        print(discarded.stderr)
        return 1
    repaired = sessionio_list(config)
    if "reader cache: discarded" in repaired.stderr:
        print("reader cache corrupt: the discarded file was not replaced")
        return 1
    return max(
        compare_listing("corrupt", cold, discarded.stdout, ", discarded and relisted"),
        compare_listing("corrupt-repaired", cold, repaired.stdout, ""),
    )


def reader_cache_unwritable(config: str, directory: pathlib.Path) -> int:
    cold = sessionio_list(config).stdout
    shutil.rmtree(directory, ignore_errors=True)
    directory.mkdir(parents=True)
    directory.chmod(0o500)
    attempt = sessionio_list(config)
    directory.chmod(0o700)
    if "reader cache: could not write" not in attempt.stderr:
        print("reader cache unwritable: no write diagnostic on stderr")
        print(attempt.stderr)
        return 1
    return compare_listing(
        "unwritable", cold, attempt.stdout, ", cache directory not writable"
    )


SEARCH_FRESHNESS_CASES = ("killed-candidate", "concurrent", "unreadable")

# writerLeaseKey in internal/catalog/freshness.go. The concurrent case fails
# loudly if the two ever drift, because nothing would be refused.
WRITER_LEASE_KEY = 0x5E5511

# PLANT_KILLED_CANDIDATE leaves what a scan killed with SIGKILL leaves behind:
# a building generation, its membership, and derived rows no generation
# presents while the shared retrieval indexes still cover them.
PLANT_KILLED_CANDIDATE = """DO $$
DECLARE candidate bigint; source bigint; planted bigint; doc bigint;
BEGIN
    INSERT INTO {schema}.generation (state) VALUES ('building') RETURNING id
        INTO candidate;
    FOR source IN SELECT id FROM {schema}.derived_session LOOP
        planted := nextval('{schema}.derived_session_id');
        INSERT INTO {schema}.derived_session
            SELECT planted, revision_hash, builder_key || ';killed', session_key,
                harness, native_id, title, source_id, occurrence_id,
                discovery_revision, source_revision_kind, source_revision_value,
                locator_kind, locator_root, locator_path, started_at, updated_at
            FROM {schema}.derived_session WHERE id = source;
        FOR doc IN SELECT doc_id FROM {schema}.search_document
            WHERE derived_id = source LOOP
            INSERT INTO {schema}.search_document (doc_id, derived_id, session_ref,
                harness, passage_id, projection_kind, projection_version, body,
                content_hash)
                SELECT nextval('{schema}.search_document_id'), planted,
                    session_ref, harness, NULL, projection_kind,
                    projection_version, body, content_hash
                FROM {schema}.search_document WHERE doc_id = doc;
        END LOOP;
        INSERT INTO {schema}.generation_member (generation_id, derived_id)
            VALUES (candidate, planted);
    END LOOP;
END $$"""


def sessionio_search(config: str, query: str, *extra: str) -> subprocess.CompletedProcess:
    argv = [
        str(ACCEPTANCE_BINARY), "--config", config,
        "search", "--mode", "lexical", "--format", "json", *extra, query,
    ]
    return subprocess.run(
        argv, cwd=ROOT, check=False, text=True, encoding="utf-8", capture_output=True
    )


def quiescent_answer(record: str) -> str:
    """The answer without the two facts a repair is allowed to change."""
    decoded = json.loads(record)
    decoded["catalog_generation"] = 0
    decoded["catalog_refresh"] = {}
    return json.dumps(decoded, sort_keys=True)


def require_schema(schema: str | None) -> str:
    if not schema or not schema.startswith("sessionio_") or not SCHEMA_NAME.match(schema):
        raise DevError(f"search-freshness refuses schema {schema!r}")
    return schema


def search_freshness(case: str | None, config: str | None, schema: str | None,
                     root: str | None, query: str | None) -> int:
    if case not in SEARCH_FRESHNESS_CASES:
        raise DevError(
            "search-freshness requires a case: " + ", ".join(SEARCH_FRESHNESS_CASES)
        )
    if not config:
        raise DevError("search-freshness requires --config")
    if case == "concurrent":
        return search_freshness_concurrent(config, require_schema(schema))
    if not query:
        raise DevError(f"search-freshness {case} requires --query")
    if case == "killed-candidate":
        return search_freshness_killed(config, require_schema(schema), query)
    if not root:
        raise DevError("search-freshness unreadable requires --root")
    return search_freshness_unreadable(config, ROOT / root, query)


def search_freshness_killed(config: str, schema: str, query: str) -> int:
    """The garbage of a killed scan is swept, and the answer is what it was."""
    before = sessionio_search(config, query)
    if before.returncode != 0:
        raise DevError(f"the quiescent search exited {before.returncode}: {before.stderr}")
    prefix = psql_prefix()
    planted = capture(psql_argv(prefix, "-c", PLANT_KILLED_CANDIDATE.format(schema=schema)))
    if planted.returncode != 0:
        raise DevError("planting the killed candidate failed: " + planted.stderr.strip())
    after = sessionio_search(config, query)
    if after.returncode != 0:
        raise DevError(f"the repairing search exited {after.returncode}: {after.stderr}")
    if "unreclaimed" not in after.stderr:
        print("search freshness killed-candidate: no repair message on stderr")
        print(after.stderr)
        return 1
    if quiescent_answer(before.stdout) != quiescent_answer(after.stdout):
        print("search freshness killed-candidate: the answer changed")
        for line in difflib.unified_diff(
            quiescent_answer(before.stdout).splitlines(),
            quiescent_answer(after.stdout).splitlines(),
            "before", "after", lineterm="", n=0,
        ):
            print(line)
        return 1
    scores = [result["bm25_score"] for result in json.loads(after.stdout)["results"]]
    print(f"search freshness killed-candidate: byte-identical ({len(scores)} scored hits)")
    return 0


def search_freshness_concurrent(config: str, schema: str) -> int:
    """One writer at a time: a second scan and every search are refused."""
    prefix = psql_prefix()
    holder = subprocess.Popen(
        psql_argv(prefix),
        cwd=ROOT,
        text=True,
        encoding="utf-8",
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    problems = []
    try:
        holder.stdin.write(
            f"SELECT pg_advisory_lock(hashtext('sessionio:{schema}'),"
            f" {WRITER_LEASE_KEY});\n"
        )
        holder.stdin.flush()
        # psql answers the void function with one empty line: reading it is how
        # this waits until the lease is really held.
        holder.stdout.readline()
        for argv in (
            [str(ACCEPTANCE_BINARY), "--config", config, "scan", "--format", "json"],
            [str(ACCEPTANCE_BINARY), "--config", config, "search", "--format", "json",
             "--mode", "lexical", "probe"],
        ):
            result = subprocess.run(
                argv, cwd=ROOT, check=False, text=True, encoding="utf-8",
                capture_output=True,
            )
            name = argv[3]
            if result.returncode != 3:
                problems.append(f"{name} exited {result.returncode}, want 3")
            if '"kind":"scan_in_progress"' not in result.stdout:
                problems.append(f"{name} did not report scan_in_progress")
    finally:
        holder.stdin.close()
        holder.wait(timeout=30)
    if problems:
        print("search freshness concurrent: " + "; ".join(problems))
        return 1
    print("search freshness concurrent: scan and search both refused")
    return 0


def search_freshness_unreadable(config: str, root: pathlib.Path, query: str) -> int:
    """The gate reads stat identity, never a transcript."""
    warm = sessionio_search(config, query)
    if warm.returncode != 0:
        raise DevError(f"the warming search exited {warm.returncode}: {warm.stderr}")
    if not transcript_files(root):
        raise DevError(f"no transcripts under {root}")
    set_transcript_mode(root, 0o000)
    try:
        gated = sessionio_search(config, query)
    finally:
        set_transcript_mode(root, 0o644)
    if gated.returncode != 0:
        print(f"search freshness unreadable: the gate exited {gated.returncode}")
        print(gated.stderr)
        return 1
    if json.loads(gated.stdout)["catalog_refresh"]["ran"]:
        print("search freshness unreadable: the gate scanned an unchanged catalog")
        return 1
    if warm.stdout != gated.stdout:
        print("search freshness unreadable: the answer changed")
        return 1
    print(
        "search freshness unreadable: byte-identical"
        f" ({len(gated.stdout)} bytes, every transcript at mode 000)"
    )
    return 0


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
        run([sys.executable, "dev.py", "pg-test"]),
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
            "pg-test",
            "pg-smoke",
            "pg-adversarial",
            "pg-drop-schema",
            "corrupt-state",
            "remove-temp",
            "supersede-builder",
            "reader-cache",
            "search-freshness",
            "pg-up",
            "pg-down",
            "openrouter-profile-check",
            "release-build",
        ),
    )
    # Positional payload: an adversarial case, a schema name, or a stream path.
    parser.add_argument("case", nargs="?")
    parser.add_argument("--probe-fixture")
    parser.add_argument("--config")
    parser.add_argument("--cache")
    parser.add_argument("--root")
    parser.add_argument("--schema")
    parser.add_argument("--query")
    parser.add_argument("--model")
    parser.add_argument("--require-live", action="store_true")
    args = parser.parse_args()

    commands = {
        "lint": lint,
        "test": test,
        "e2e": e2e,
        "check": check,
        "acceptance": acceptance,
        "pg-test": pg_test,
        "pg-smoke": lambda: pg_smoke(args.probe_fixture),
        "pg-adversarial": lambda: pg_adversarial(args.case),
        "pg-drop-schema": lambda: pg_drop_schema(args.case),
        "corrupt-state": lambda: corrupt_state(args.case),
        "remove-temp": lambda: remove_temp(args.case),
        "supersede-builder": lambda: supersede_builder(args.case),
        "reader-cache": lambda: reader_cache(
            args.case, args.config, args.cache, args.root
        ),
        "search-freshness": lambda: search_freshness(
            args.case, args.config, args.schema, args.root, args.query
        ),
        "pg-up": pg_up,
        "pg-down": pg_down,
        "openrouter-profile-check": lambda: openrouter_profile_check(
            args.model, args.require_live
        ),
        "release-build": release_build,
    }
    if args.command == "pg-adversarial" and args.case is None:
        parser.error("pg-adversarial requires a case: " + ", ".join(ADVERSARIAL_CASES))
    if args.command == "pg-drop-schema" and args.case is None:
        parser.error("pg-drop-schema requires a schema name")
    if args.command == "corrupt-state" and args.case is None:
        parser.error("corrupt-state requires a state stream path")
    if args.command == "remove-temp" and args.case is None:
        parser.error("remove-temp requires a path")
    if args.command == "supersede-builder" and args.case is None:
        parser.error("supersede-builder requires a schema name")
    if args.command == "reader-cache" and args.case not in READER_CACHE_CASES:
        parser.error("reader-cache requires a case: " + ", ".join(READER_CACHE_CASES))
    if args.command == "search-freshness" and args.case not in SEARCH_FRESHNESS_CASES:
        parser.error(
            "search-freshness requires a case: " + ", ".join(SEARCH_FRESHNESS_CASES)
        )
    try:
        return commands[args.command]()
    except DevError as error:
        raise SystemExit(f"error: {error}") from error


if __name__ == "__main__":
    raise SystemExit(main())

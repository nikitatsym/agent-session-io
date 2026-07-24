#!/usr/bin/env python3

import argparse
import pathlib
import subprocess


ROOT = pathlib.Path(__file__).resolve().parent


def run(command: list[str]) -> int:
    print("+", " ".join(command), flush=True)
    return subprocess.run(command, cwd=ROOT, check=False).returncode


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
    ]
    return int(any(results))


def test() -> int:
    return run(["go", "test", "./..."])


def e2e() -> int:
    return run(["go", "test", "./...", "-run", "^TestE2E"])


def check() -> int:
    results = [lint(), test()]
    return int(any(results))


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("command", choices=("lint", "test", "e2e", "check"))
    args = parser.parse_args()

    commands = {
        "lint": lint,
        "test": test,
        "e2e": e2e,
        "check": check,
    }
    return commands[args.command]()


if __name__ == "__main__":
    raise SystemExit(main())

# agent-session-io
<!-- tackbox: chars=ascii -->

[![ci](https://github.com/nikitatsym/agent-session-io/actions/workflows/ci.yml/badge.svg)](https://github.com/nikitatsym/agent-session-io/actions/workflows/ci.yml)
[![release](https://github.com/nikitatsym/agent-session-io/actions/workflows/release.yml/badge.svg)](https://github.com/nikitatsym/agent-session-io/actions/workflows/release.yml)

Harness-neutral access to local coding-agent sessions.

`agent-session-io` is a Go library and a single `sessionio` CLI for
discovering, reading, exporting, and inspecting current sessions from
coding-agent harnesses. Codex and Claude Code are the first full-fidelity
targets. Catalog-backed scan and search commands are in development against
an optional PostgreSQL 18 service.

The core reader stays usable without PostgreSQL, embeddings, a model
provider, or a background service.

## Install

macOS or Linux:

```sh
curl --proto '=https' --tlsv1.2 -fsSL \
  https://raw.githubusercontent.com/nikitatsym/agent-session-io/main/scripts/install.sh | sh
```

Windows PowerShell:

```powershell
irm https://raw.githubusercontent.com/nikitatsym/agent-session-io/main/scripts/install.ps1 | iex
```

With a Go toolchain:

```sh
go install github.com/nikitatsym/agent-session-io/cmd/sessionio@latest
```

The release installers select the current operating system and
architecture, download the latest GitHub Release archive, verify its
SHA-256 checksum, install into a user-owned directory, and connect shell
completion. Restart the shell after installation. Set `SESSIONIO_VERSION`
to install a specific tag, `SESSIONIO_INSTALL_DIR` to choose another
directory, `SESSIONIO_COMPLETION_SHELL` to override shell detection, or
`SESSIONIO_NO_COMPLETION=1` to skip completion setup.

Release archives are available for macOS, Linux, and Windows on amd64 and
arm64. GitHub build provenance can be verified with:

```sh
gh attestation verify sessionio_darwin_arm64.tar.gz \
  --repo nikitatsym/agent-session-io
```

## CLI

Fang and Cobra provide styled help, shell completion, and manpage
generation:

```sh
sessionio --help
sessionio --version
sessionio version --json
sessionio completion zsh
sessionio completion install
sessionio update
sessionio sources
sessionio list --harness codex --since 7d
sessionio list --current
sessionio list --current=exact --format json
sessionio show SESSION_ID
sessionio export SESSION_ID
```

`sessionio completion install` generates a static completion script and
connects it through a managed block in the detected shell profile. It is
safe to run repeatedly. PowerShell users can override the profile with
`--profile`.

`sessionio update` selects the latest release for the current platform,
verifies it against the published SHA-256 checksum file, and replaces the
current executable with rollback on failure. Public release redirects and
asset URLs are used directly, so checking for an update does not require a
GitHub API token or consume the GitHub REST API rate limit.

`sources` and `list` default to human-readable tables. Both accept
`--format human|json|ndjson`, and `--harness codex|claude` can be repeated.
`list` also accepts inclusive `--since` and `--until` bounds as RFC3339 or
elapsed durations such as `30m`, `7d`, or `2w`. `list --current` reports
sessions tied to live Codex or Claude processes; `--current=exact` excludes
probable evidence before matching and regrouping. Runtime presence cannot be
combined with `--since` or `--until`.

`show` and `export` take the session ID printed by `list`; a unique
prefix of the ID or of its digest part is enough, and an ambiguous
prefix fails with the matching candidates. `show` provides
`--detail normalized|native|provenance`. `export` is the lossless
machine interface: it defaults to streaming, self-describing NDJSON and
accepts `--format json` for a single buffered document. Scripts and
agents should always pass their desired format explicitly.

### Catalog and search

Catalog-backed commands need a configured PostgreSQL 18 endpoint and never
run implicitly. `postgres/compose.yaml` provides the canonical profile.

```sh
sessionio --config config.toml catalog init
sessionio --config config.toml scan
sessionio --config config.toml search --mode lexical 'why did the protocol change'
sessionio --config config.toml search --mode literal 'ECONNRESET: socket hang up'
sessionio --config config.toml doctor --scope postgres
```

Configured source roots decide what a scan reads:

```toml
[sources.codex]
home = "fixtures/codex"

[sources.claude]
config_dir = "fixtures/claude"
```

A declared root wins over the harness environment variable (`CODEX_HOME`,
`CLAUDE_CONFIG_DIR`), which wins over the platform default. A relative root
resolves against the directory holding the configuration file, and every
command that reads sessions - `sources`, `list`, `show`, `export`, and `scan` -
uses the same resolution. Without a `[sources]` section discovery is unchanged.

`scan` reads every discovered session into a new candidate generation,
builds its BM25 and trigram indexes, and publishes it in one transaction. A
scan that fails before publication leaves the previous generation active and
unchanged.

`search` reads exactly one active generation. `--mode lexical` uses the BM25
leg, `--mode literal` uses case-sensitive exact containment, and every result
carries its session, passage, evidence locators, matched leg, catalog
generation, and completeness. Literal results report whether PostgreSQL used
the trigram index or the bounded scan. Projection text is byte-exact to the
native content except for U+0000, which no PostgreSQL text column can store;
a result whose text lost NUL bytes reports a `nul_removed` entry with the
removed-byte count in `projection_limitations`, and the evidence locator still
addresses the original bytes. Exit statuses are `0` for a match, `1`
for a valid search with no match, `2` for an invalid request, `3` for a
missing capability, and `5` for a runtime failure.

Machine output and the Go reader API are current drafts until an explicit
contract-freeze decision. Before that decision, the project updates them in
place without compatibility shims.

The durable reader semantics are documented in the
[reader contract](docs/reader-contract.md).

## Development

The repository requires Go, Python 3, and `uv`.

```sh
python3 dev.py check
uvx pre-commit install
```

`dev.py` is the single development entry point:

- `python3 dev.py lint`
- `python3 dev.py test`
- `python3 dev.py e2e`
- `python3 dev.py check`

Lint includes project-owned Go checks and
`uvx tackbox@latest lint .`. Pre-commit and CI run the same complete
`dev.py check`.

## Release

Pushing a semantic version tag runs the complete check, builds
cross-platform archives with GoReleaser, publishes checksums, and records
GitHub build provenance:

```sh
git tag v0.1.0
git push origin v0.1.0
```

## License

Mozilla Public License 2.0. See [LICENSE](LICENSE).

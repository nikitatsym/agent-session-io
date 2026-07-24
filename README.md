# agent-session-io
<!-- tackbox: chars=ascii -->

[![ci](https://github.com/nikitatsym/agent-session-io/actions/workflows/ci.yml/badge.svg)](https://github.com/nikitatsym/agent-session-io/actions/workflows/ci.yml)
[![release](https://github.com/nikitatsym/agent-session-io/actions/workflows/release.yml/badge.svg)](https://github.com/nikitatsym/agent-session-io/actions/workflows/release.yml)

Harness-neutral access to local coding-agent sessions.

`agent-session-io` is a Go library and a single `sessionio` CLI for
discovering, reading, exporting, indexing, and searching sessions from
coding-agent harnesses. Codex and Claude Code are the first full-fidelity
targets.

The core reader stays usable without SQLite, embeddings, a model provider,
or a background service.

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

Machine output is versioned independently of the Go module. Before 1.0,
breaking Go API changes occur only in tagged minor releases.

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

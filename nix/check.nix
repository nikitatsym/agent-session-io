{ runCommand, python3, package }:

runCommand "sessionio-smoke" { nativeBuildInputs = [ python3 ]; } ''
  export HOME="$TMPDIR/home"
  export XDG_CONFIG_HOME="$HOME/.config"
  export CODEX_HOME="$HOME/codex"
  export CLAUDE_CONFIG_DIR="$HOME/claude"
  export PI_CODING_AGENT_DIR="$HOME/omp"
  export SESSIONIO_CACHE_DIR="$TMPDIR/cache"
  mkdir -p "$CODEX_HOME/sessions/2026/07/25"
  cat > "$CODEX_HOME/sessions/2026/07/25/rollout-2026-07-25T10-00-00-10000000-0000-4000-8000-000000000099.jsonl" <<'EOF'
  {"type":"session_meta","timestamp":"2026-07-25T10:00:00Z","payload":{"id":"10000000-0000-4000-8000-000000000099","timestamp":"2026-07-25T10:00:00Z","cwd":"/work/smoke"}}
  {"type":"response_item","timestamp":"2026-07-25T10:01:00Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"package smoke"}]}}
  EOF

  ${package}/bin/sessionio version --json > version.json
  ${package}/bin/sessionio list --harness codex --format json > list.json
  if ${package}/bin/sessionio update > update.out 2> update.err; then
    echo 'Nix-managed update unexpectedly succeeded' >&2
    exit 1
  fi

  python3 - <<'PY'
  import json
  import subprocess
  from pathlib import Path

  version = json.loads(Path("version.json").read_text())
  assert version["package_manager"] == "nix", version
  listing = json.loads(Path("list.json").read_text())
  sessions = [record["session"] for record in listing["records"] if record["kind"] == "session"]
  assert [session["native_id"] for session in sessions] == ["10000000-0000-4000-8000-000000000099"], listing
  exported = subprocess.check_output(
      ["${package}/bin/sessionio", "export", sessions[0]["id"], "--format", "json"], text=True
  )
  assert "package smoke" in exported, exported
  error = Path("update.err").read_text()
  assert "managed by Nix" in error and "nix profile upgrade" in error, error
  PY

  for completion in \
    ${package}/share/bash-completion/completions/sessionio.bash \
    ${package}/share/zsh/site-functions/_sessionio \
    ${package}/share/fish/vendor_completions.d/sessionio.fish; do
    test -s "$completion"
  done
  touch "$out"
''

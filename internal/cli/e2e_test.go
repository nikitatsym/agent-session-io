package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	sessionio "github.com/nikitatsym/agent-session-io"
)

func TestE2EReaderCLIBuiltBinary(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	binaryName := "sessionio"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binary := filepath.Join(t.TempDir(), binaryName)
	build := exec.Command("go", "build", "-o", binary, "./cmd/sessionio")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build sessionio: %v\n%s", err, output)
	}

	codexHome := filepath.Join(t.TempDir(), "codex")
	claudeHome := filepath.Join(t.TempDir(), "claude")
	writeE2EFixtures(t, codexHome, claudeHome)
	environment := append(
		os.Environ(),
		"CODEX_HOME="+codexHome,
		"CLAUDE_CONFIG_DIR="+claudeHome,
	)

	sourcesOutput := runE2ECommand(
		t,
		binary,
		environment,
		0,
		"sources",
		"--format",
		"json",
	)
	sourceHarnesses := decodeE2ERecordHarnesses(t, sourcesOutput, "source")
	if !sourceHarnesses[sessionio.HarnessCodex] ||
		!sourceHarnesses[sessionio.HarnessClaude] {
		t.Fatalf("source harnesses = %v, want Codex and Claude", sourceHarnesses)
	}

	listOutput := runE2ECommand(
		t,
		binary,
		environment,
		0,
		"list",
		"--format",
		"json",
	)
	sessionIDs := decodeE2ESessionIDs(t, listOutput)
	for _, harness := range []sessionio.Harness{
		sessionio.HarnessCodex,
		sessionio.HarnessClaude,
	} {
		id := sessionIDs[harness]
		if id == "" {
			t.Fatalf("list did not emit a %s session", harness)
		}
		showOutput := runE2ECommand(
			t,
			binary,
			environment,
			0,
			"show",
			string(id),
			"--detail",
			"normalized",
		)
		if !bytes.Contains(showOutput, []byte("event ")) {
			t.Fatalf("%s show output has no event:\n%s", harness, showOutput)
		}
		exportOutput := runE2ECommand(
			t,
			binary,
			environment,
			0,
			"export",
			string(id),
			"--format",
			"ndjson",
		)
		assertE2EExportOrder(t, exportOutput)
	}

	runE2ECommand(
		t,
		binary,
		environment,
		exitInvalid,
		"list",
		"--since",
		"tomorrow",
	)
	runE2ECommand(
		t,
		binary,
		environment,
		exitNotFound,
		"show",
		"missing-session",
	)
}

func writeE2EFixtures(t *testing.T, codexHome, claudeHome string) {
	t.Helper()
	const codexID = "10000000-0000-4000-8000-000000000099"
	codexPath := filepath.Join(
		codexHome,
		"sessions",
		"2026",
		"07",
		"25",
		"rollout-2026-07-25T10-00-00-"+codexID+".jsonl",
	)
	writeE2EFile(
		t,
		codexPath,
		`{"id":"`+codexID+`","session_id":"codex-e2e","timestamp":"2026-07-25T10:00:00Z","type":"session_meta","cwd":"/work/e2e"}`+"\n"+
			`{"timestamp":"2026-07-25T10:01:00Z","type":"message","role":"user","content":"codex e2e"}`+"\n",
	)

	const claudeID = "20000000-0000-4000-8000-000000000099"
	claudePath := filepath.Join(
		claudeHome,
		"projects",
		"-e2e",
		claudeID+".jsonl",
	)
	writeE2EFile(
		t,
		claudePath,
		`{"type":"user","uuid":"e2e-user","sessionId":"`+claudeID+`","timestamp":"2026-07-25T11:00:00Z","message":{"role":"user","content":"claude e2e"}}`+"\n",
	)
}

func writeE2EFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create fixture directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

func runE2ECommand(
	t *testing.T,
	binary string,
	environment []string,
	wantExit int,
	args ...string,
) []byte {
	t.Helper()
	command := exec.Command(binary, args...)
	command.Env = environment
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	gotExit := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatalf("run sessionio %v: %v", args, err)
		}
		gotExit = exitError.ExitCode()
	}
	if gotExit != wantExit {
		t.Fatalf(
			"sessionio %v exit = %d, want %d\nstdout:\n%s\nstderr:\n%s",
			args,
			gotExit,
			wantExit,
			stdout.Bytes(),
			stderr.Bytes(),
		)
	}
	if wantExit != 0 && stderr.Len() == 0 {
		t.Fatalf("sessionio %v failure wrote no stderr", args)
	}
	return stdout.Bytes()
}

func decodeE2ERecordHarnesses(
	t *testing.T,
	output []byte,
	recordKind sessionio.RecordKind,
) map[sessionio.Harness]bool {
	t.Helper()
	var document struct {
		Records []struct {
			Kind   sessionio.RecordKind `json:"kind"`
			Source *sessionio.Source    `json:"source"`
		} `json:"records"`
	}
	if err := json.Unmarshal(output, &document); err != nil {
		t.Fatalf("decode source document: %v\n%s", err, output)
	}
	result := make(map[sessionio.Harness]bool)
	for _, record := range document.Records {
		if record.Kind == recordKind && record.Source != nil {
			result[record.Source.Harness] = true
		}
	}
	return result
}

func decodeE2ESessionIDs(
	t *testing.T,
	output []byte,
) map[sessionio.Harness]sessionio.SessionID {
	t.Helper()
	var document struct {
		Records []struct {
			Kind    sessionio.RecordKind  `json:"kind"`
			Session *sessionio.SessionRef `json:"session"`
		} `json:"records"`
	}
	if err := json.Unmarshal(output, &document); err != nil {
		t.Fatalf("decode session document: %v\n%s", err, output)
	}
	result := make(map[sessionio.Harness]sessionio.SessionID)
	for _, record := range document.Records {
		if record.Kind == sessionio.RecordKindSession &&
			record.Session != nil {
			result[record.Session.Occurrence.Harness] = record.Session.ID
		}
	}
	return result
}

func assertE2EExportOrder(t *testing.T, output []byte) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) < 3 {
		t.Fatalf("export emitted %d lines, want at least 3\n%s", len(lines), output)
	}
	want := []sessionio.RecordKind{
		sessionio.RecordKindSource,
		sessionio.RecordKindSession,
		sessionio.RecordKindReadItem,
	}
	for index, wantKind := range want {
		var envelope struct {
			Record struct {
				Kind sessionio.RecordKind `json:"kind"`
			} `json:"record"`
		}
		if err := json.Unmarshal([]byte(lines[index]), &envelope); err != nil {
			t.Fatalf("decode export line %d: %v", index, err)
		}
		if envelope.Record.Kind != wantKind {
			t.Fatalf(
				"export record %d = %q, want %q",
				index,
				envelope.Record.Kind,
				wantKind,
			)
		}
	}
}

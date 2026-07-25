package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sessionio "github.com/nikitatsym/agent-session-io"
	"github.com/nikitatsym/agent-session-io/internal/buildinfo"
	"github.com/spf13/cobra"
)

func TestSourcesDefaultsToHumanAndDeduplicatesHarnessFilter(t *testing.T) {
	claudeAdapter := &fakeReaderAdapter{
		descriptor: testDescriptor(sessionio.HarnessClaude),
		sources: []sessionio.Source{
			testSource(
				sessionio.HarnessClaude,
				sessionio.SourceKindAuxiliary,
				"source-aux",
			),
			testSource(
				sessionio.HarnessClaude,
				sessionio.SourceKindCanonical,
				"source-canonical",
			),
		},
	}
	codexAdapter := &fakeReaderAdapter{
		descriptor: testDescriptor(sessionio.HarnessCodex),
		sources: []sessionio.Source{
			testSource(
				sessionio.HarnessCodex,
				sessionio.SourceKindCanonical,
				"source-codex",
			),
		},
	}
	root, output, diagnostic := testReaderRoot(
		t,
		time.Now,
		claudeAdapter,
		codexAdapter,
	)
	root.SetArgs([]string{
		"sources",
		"--harness", "claude",
		"--harness", "claude",
	})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute sources: %v", err)
	}
	if claudeAdapter.sourcesCalls != 1 {
		t.Fatalf("Claude Sources calls = %d, want 1", claudeAdapter.sourcesCalls)
	}
	if codexAdapter.sourcesCalls != 0 {
		t.Fatalf("Codex Sources calls = %d, want 0", codexAdapter.sourcesCalls)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("sources lines = %d, want 3\n%s", len(lines), output.String())
	}
	if !strings.HasPrefix(lines[0], "HARNESS\tKIND\tSTATUS") {
		t.Fatalf("heading = %q, want human table", lines[0])
	}
	if !strings.Contains(lines[1], "source-canonical") ||
		!strings.Contains(lines[2], "source-aux") {
		t.Fatalf("source order = %q, want canonical before auxiliary", lines[1:])
	}
	if diagnostic.Len() != 0 {
		t.Fatalf("diagnostic output = %q, want empty", diagnostic.String())
	}
}

func TestListActivityFilterUsesOneNowAndNativeTimestamps(t *testing.T) {
	nowValue := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	nowCalls := 0
	now := func() time.Time {
		nowCalls++
		return nowValue
	}
	atTen := nowValue.Add(-2 * time.Hour)
	atNine := nowValue.Add(-3 * time.Hour)
	first := testSession(sessionio.HarnessCodex, "session-inclusive")
	second := testSession(sessionio.HarnessCodex, "session-too-old")
	missing := testSession(sessionio.HarnessCodex, "session-missing-time")
	adapter := &fakeReaderAdapter{
		descriptor: testDescriptor(sessionio.HarnessCodex),
		sources: []sessionio.Source{
			testSource(
				sessionio.HarnessCodex,
				sessionio.SourceKindCanonical,
				"source-codex",
			),
		},
		sessions: []sessionio.SessionRef{second, missing, first},
		itemsBySession: map[sessionio.SessionID][]sessionio.ReadItem{
			first.ID:  {testReadItem(first, &atTen, []byte("first"))},
			second.ID: {testReadItem(second, &atNine, []byte("second"))},
			missing.ID: {
				testReadItem(missing, nil, []byte("missing")),
			},
		},
	}
	root, output, _ := testReaderRoot(t, now, adapter)
	root.SetArgs([]string{
		"list",
		"--since", "2h",
		"--until", "1h",
	})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute list: %v", err)
	}
	if nowCalls != 1 {
		t.Fatalf("clock calls = %d, want 1", nowCalls)
	}
	if adapter.readCalls != 3 {
		t.Fatalf("Read calls = %d, want 3", adapter.readCalls)
	}
	if !strings.Contains(output.String(), "session-inclusive") {
		t.Fatalf("output missing inclusive boundary session:\n%s", output.String())
	}
	if strings.Contains(output.String(), "session-too-old") {
		t.Fatalf("output contains filtered session:\n%s", output.String())
	}
	if strings.Contains(output.String(), "session-missing-time") {
		t.Fatalf("output contains session without activity time:\n%s", output.String())
	}
}

func TestListWithoutActivityFilterStaysHeaderOnly(t *testing.T) {
	session := testSession(sessionio.HarnessCodex, "session-no-read")
	adapter := &fakeReaderAdapter{
		descriptor: testDescriptor(sessionio.HarnessCodex),
		sessions:   []sessionio.SessionRef{session},
	}
	root, output, _ := testReaderRoot(t, time.Now, adapter)
	root.SetArgs([]string{"list"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute list: %v", err)
	}
	if adapter.readCalls != 0 {
		t.Fatalf("Read calls = %d, want 0", adapter.readCalls)
	}
	if !strings.Contains(output.String(), string(session.ID)) {
		t.Fatalf("output missing session:\n%s", output.String())
	}
}

func TestListUsesStableTimeHarnessAndIDOrdering(t *testing.T) {
	newerTime := time.Date(2026, 7, 25, 11, 0, 0, 0, time.UTC)
	tieTime := newerTime.Add(-time.Hour)
	newer := testSession(sessionio.HarnessCodex, "newer")
	newer.UpdatedAt = &newerTime
	codexA := testSession(sessionio.HarnessCodex, "a")
	codexA.StartedAt = &tieTime
	codexZ := testSession(sessionio.HarnessCodex, "z")
	codexZ.UpdatedAt = &tieTime
	claude := testSession(sessionio.HarnessClaude, "b")
	claude.UpdatedAt = &tieTime
	missing := testSession(sessionio.HarnessClaude, "missing")
	codexAdapter := &fakeReaderAdapter{
		descriptor: testDescriptor(sessionio.HarnessCodex),
		sessions:   []sessionio.SessionRef{codexZ, newer, codexA},
	}
	claudeAdapter := &fakeReaderAdapter{
		descriptor: testDescriptor(sessionio.HarnessClaude),
		sessions:   []sessionio.SessionRef{missing, claude},
	}
	root, output, _ := testReaderRoot(
		t,
		time.Now,
		codexAdapter,
		claudeAdapter,
	)
	root.SetArgs([]string{"list"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute list: %v", err)
	}
	wantOrder := []string{"newer", "b", "a", "z", "missing"}
	previous := -1
	for _, id := range wantOrder {
		index := strings.Index(output.String(), "\t"+id+"\t")
		if index < 0 {
			t.Fatalf("output missing %q:\n%s", id, output.String())
		}
		if index <= previous {
			t.Fatalf("session %q is out of order:\n%s", id, output.String())
		}
		previous = index
	}
}

func TestShowUsesExactCaseSensitiveSessionID(t *testing.T) {
	session := testSession(sessionio.HarnessCodex, "Case-Sensitive")
	adapter := &fakeReaderAdapter{
		descriptor: testDescriptor(sessionio.HarnessCodex),
		sessions:   []sessionio.SessionRef{session},
	}
	root, _, _ := testReaderRoot(t, time.Now, adapter)
	root.SetArgs([]string{"show", "case-sensitive"})

	err := root.Execute()
	if ExitCode(err) != exitNotFound {
		t.Fatalf("ExitCode(%v) = %d, want %d", err, ExitCode(err), exitNotFound)
	}
	if adapter.readCalls != 0 {
		t.Fatalf("Read calls = %d, want 0", adapter.readCalls)
	}
}

func TestShowNativeLabelsArbitraryBytesAsBase64(t *testing.T) {
	session := testSession(sessionio.HarnessCodex, "session-native")
	native := []byte("danger\nnext: raw")
	item := testReadItem(session, nil, native)
	adapter := &fakeReaderAdapter{
		descriptor: testDescriptor(sessionio.HarnessCodex),
		sessions:   []sessionio.SessionRef{session},
		itemsBySession: map[sessionio.SessionID][]sessionio.ReadItem{
			session.ID: {item},
		},
	}
	root, output, _ := testReaderRoot(t, time.Now, adapter)
	root.SetArgs([]string{"show", string(session.ID), "--detail", "native"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute show: %v", err)
	}
	want := "data_base64: " + base64.StdEncoding.EncodeToString(native)
	if !strings.Contains(output.String(), want) {
		t.Fatalf("native output missing %q:\n%s", want, output.String())
	}
	if strings.Contains(output.String(), string(native)) {
		t.Fatalf("native output contains unlabelled raw bytes:\n%s", output.String())
	}
}

func TestShowProvenanceIncludesEvidenceButNotNativePayload(t *testing.T) {
	session := testSession(sessionio.HarnessClaude, "session-provenance")
	native := []byte("private native payload")
	item := testReadItem(session, nil, native)
	evidence := []sessionio.EvidenceRef{{
		Observation: item.Observation.ID,
		Locator:     item.Observation.Locator,
	}}
	item.Observation.Limitations = []sessionio.SourceLimitation{{
		Kind:   sessionio.LimitationKindExternalPayload,
		Detail: "external payload remains outside the transcript",
	}}
	item.Events = []sessionio.Event{{
		ID:       "event-provenance",
		Kind:     sessionio.EventKindUnknown,
		Evidence: evidence,
		Unknown:  &sessionio.UnknownEvent{NativeType: "future"},
	}}
	item.Relations = []sessionio.Relation{{
		ID:       "relation-provenance",
		Kind:     sessionio.RelationKindContains,
		From:     sessionio.NodeRef{Kind: sessionio.NodeKindObservation, ID: string(item.Observation.ID)},
		To:       sessionio.NodeRef{Kind: sessionio.NodeKindEvent, ID: "event-provenance"},
		Origin:   sessionio.RelationOriginDeterministic,
		Evidence: evidence,
	}}
	item.Diagnostics = []sessionio.Diagnostic{{
		Code:     "provenance_notice",
		Severity: sessionio.DiagnosticSeverityInfo,
		Message:  "provenance diagnostic",
	}}
	adapter := &fakeReaderAdapter{
		descriptor: testDescriptor(sessionio.HarnessClaude),
		sessions:   []sessionio.SessionRef{session},
		itemsBySession: map[sessionio.SessionID][]sessionio.ReadItem{
			session.ID: {item},
		},
	}
	root, output, diagnostic := testReaderRoot(t, time.Now, adapter)
	root.SetArgs([]string{
		"show",
		string(session.ID),
		"--detail",
		"provenance",
	})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute provenance show: %v", err)
	}
	for _, expected := range []string{
		"discovery_revision:",
		"revision:",
		"limitation external_payload",
		"evidence observation=",
		"relation relation-provenance",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("provenance output missing %q:\n%s", expected, output.String())
		}
	}
	if strings.Contains(output.String(), string(native)) {
		t.Fatalf("provenance output contains native payload:\n%s", output.String())
	}
	if !strings.Contains(diagnostic.String(), "provenance_notice") {
		t.Fatalf("provenance stderr missing diagnostic: %q", diagnostic.String())
	}
}

func TestExportDefaultsToNDJSONAndKeepsValidPrefixOnLateReadError(t *testing.T) {
	session := testSession(sessionio.HarnessCodex, "session-export")
	adapter := failingExportAdapter(
		session,
		[]byte(`{"safe":true}`),
		errors.New("synthetic late read failure"),
	)
	root, output, _ := testReaderRoot(t, time.Now, adapter)
	root.SetArgs([]string{"export", string(session.ID)})

	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "synthetic late read failure") {
		t.Fatalf("export error = %v, want late read failure", err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("NDJSON lines = %d, want valid 3-record prefix\n%s", len(lines), output.String())
	}
	var kinds []sessionio.RecordKind
	for _, line := range lines {
		var envelope struct {
			Schema string `json:"schema"`
			Record struct {
				Kind sessionio.RecordKind `json:"kind"`
			} `json:"record"`
		}
		if decodeErr := json.Unmarshal([]byte(line), &envelope); decodeErr != nil {
			t.Fatalf("decode NDJSON line: %v\n%s", decodeErr, line)
		}
		if envelope.Schema != sessionio.ReaderSchema {
			t.Fatalf("schema = %q, want %q", envelope.Schema, sessionio.ReaderSchema)
		}
		kinds = append(kinds, envelope.Record.Kind)
	}
	wantKinds := []sessionio.RecordKind{
		sessionio.RecordKindSource,
		sessionio.RecordKindSession,
		sessionio.RecordKindReadItem,
	}
	for index := range wantKinds {
		if kinds[index] != wantKinds[index] {
			t.Fatalf("record kinds = %q, want %q", kinds, wantKinds)
		}
	}
	if adapter.closeCalls != 3 {
		t.Fatalf(
			"stream close calls = %d, want 3 (session, source, read)",
			adapter.closeCalls,
		)
	}
}

func TestJSONExportWritesNothingBeforeCompleteValidatedRead(t *testing.T) {
	session := testSession(sessionio.HarnessCodex, "session-json-failure")
	adapter := failingExportAdapter(
		session,
		[]byte(`{"prefix":true}`),
		errors.New("synthetic JSON read failure"),
	)
	root, output, _ := testReaderRoot(t, time.Now, adapter)
	root.SetArgs([]string{
		"export",
		string(session.ID),
		"--format",
		"json",
	})

	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "synthetic JSON read failure") {
		t.Fatalf("export error = %v, want JSON read failure", err)
	}
	if output.Len() != 0 {
		t.Fatalf("failed JSON export wrote partial output: %q", output.String())
	}
}

func TestReaderInvalidValuesUseExitTwo(t *testing.T) {
	adapter := &fakeReaderAdapter{
		descriptor: testDescriptor(sessionio.HarnessCodex),
	}
	for _, args := range [][]string{
		{"sources", "--format", "xml"},
		{"list", "--since", "0d"},
		{"show"},
		{"export", "id", "--format", "human"},
		{"sources", "--harness", "Codex"},
		{"list", "--unknown"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			root, _, _ := testReaderRoot(t, time.Now, adapter)
			root.SetArgs(args)
			err := root.Execute()
			if ExitCode(err) != exitInvalid {
				t.Fatalf("ExitCode(%v) = %d, want %d", err, ExitCode(err), exitInvalid)
			}
		})
	}
}

func TestDuplicateSessionIDIsIntegrityFailureWithEmptyStdout(t *testing.T) {
	first := testSession(sessionio.HarnessCodex, "duplicate-id")
	second := testSession(sessionio.HarnessClaude, "duplicate-id")
	codexAdapter := &fakeReaderAdapter{
		descriptor: testDescriptor(sessionio.HarnessCodex),
		sessions:   []sessionio.SessionRef{first},
	}
	claudeAdapter := &fakeReaderAdapter{
		descriptor: testDescriptor(sessionio.HarnessClaude),
		sessions:   []sessionio.SessionRef{second},
	}
	root, output, _ := testReaderRoot(
		t,
		time.Now,
		codexAdapter,
		claudeAdapter,
	)
	root.SetArgs([]string{"show", "duplicate-id"})

	err := root.Execute()
	if ExitCode(err) != exitRuntime {
		t.Fatalf("ExitCode(%v) = %d, want %d", err, ExitCode(err), exitRuntime)
	}
	if output.Len() != 0 {
		t.Fatalf("duplicate selector wrote stdout: %q", output.String())
	}
}

func TestDiagnosticsStayNestedForMachineAndUseStderrForHuman(t *testing.T) {
	source := testSource(
		sessionio.HarnessCodex,
		sessionio.SourceKindCanonical,
		"source-diagnostic",
	)
	source.Diagnostics = []sessionio.Diagnostic{{
		Code:     "synthetic_notice",
		Severity: sessionio.DiagnosticSeverityWarning,
		Message:  "synthetic notice",
	}}
	adapter := &fakeReaderAdapter{
		descriptor: testDescriptor(sessionio.HarnessCodex),
		sources:    []sessionio.Source{source},
	}

	humanRoot, humanOutput, humanDiagnostic := testReaderRoot(
		t,
		time.Now,
		adapter,
	)
	humanRoot.SetArgs([]string{"sources"})
	if err := humanRoot.Execute(); err != nil {
		t.Fatalf("execute human sources: %v", err)
	}
	if strings.Contains(humanOutput.String(), "synthetic notice") {
		t.Fatalf("human stdout contains diagnostic: %q", humanOutput.String())
	}
	if !strings.Contains(humanDiagnostic.String(), "synthetic notice") {
		t.Fatalf("human stderr missing diagnostic: %q", humanDiagnostic.String())
	}

	machineRoot, machineOutput, machineDiagnostic := testReaderRoot(
		t,
		time.Now,
		adapter,
	)
	machineRoot.SetArgs([]string{"sources", "--format", "json"})
	if err := machineRoot.Execute(); err != nil {
		t.Fatalf("execute machine sources: %v", err)
	}
	if !strings.Contains(machineOutput.String(), "synthetic_notice") {
		t.Fatalf("machine stdout missing nested diagnostic: %q", machineOutput.String())
	}
	if machineDiagnostic.Len() != 0 {
		t.Fatalf("machine stderr duplicated diagnostic: %q", machineDiagnostic.String())
	}
}

func TestReaderOutputGoldens(t *testing.T) {
	at := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	session := testSession(sessionio.HarnessCodex, "session-golden")
	session.StartedAt = &at
	adapter := &fakeReaderAdapter{
		descriptor: testDescriptor(sessionio.HarnessCodex),
		sources: []sessionio.Source{
			testSource(
				sessionio.HarnessCodex,
				sessionio.SourceKindCanonical,
				"source-codex",
			),
		},
		sessions: []sessionio.SessionRef{session},
	}
	tests := []struct {
		name string
		args []string
		file string
	}{
		{
			name: "human sources",
			args: []string{"sources"},
			file: "sources-human.golden",
		},
		{
			name: "machine list",
			args: []string{"list", "--format", "json"},
			file: "list-json.golden",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, output, diagnostic := testReaderRoot(t, time.Now, adapter)
			root.SetArgs(test.args)
			if err := root.Execute(); err != nil {
				t.Fatalf("execute %v: %v", test.args, err)
			}
			if diagnostic.Len() != 0 {
				t.Fatalf("unexpected stderr: %q", diagnostic.String())
			}
			want, err := os.ReadFile(filepath.Join("testdata", test.file))
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			if !bytes.Equal(output.Bytes(), want) {
				t.Fatalf(
					"output differs from %s\ngot:\n%s\nwant:\n%s",
					test.file,
					output.Bytes(),
					want,
				)
			}
		})
	}
}

func TestReaderEnumFlagCompletions(t *testing.T) {
	adapter := &fakeReaderAdapter{
		descriptor: testDescriptor(sessionio.HarnessCodex),
	}
	root, _, _ := testReaderRoot(t, time.Now, adapter)
	tests := []struct {
		command string
		flag    string
		want    []string
	}{
		{
			command: "sources",
			flag:    "harness",
			want:    []string{"codex", "claude"},
		},
		{
			command: "list",
			flag:    "format",
			want:    []string{"human", "json", "ndjson"},
		},
		{
			command: "show",
			flag:    "detail",
			want:    []string{"normalized", "native", "provenance"},
		},
		{
			command: "export",
			flag:    "format",
			want:    []string{"json", "ndjson"},
		},
	}
	for _, test := range tests {
		command, _, err := root.Find([]string{test.command})
		if err != nil {
			t.Fatalf("find %s: %v", test.command, err)
		}
		completion, found := command.GetFlagCompletionFunc(test.flag)
		if !found {
			t.Fatalf("%s --%s has no completion", test.command, test.flag)
		}
		got, directive := completion(command, nil, "")
		if directive != cobra.ShellCompDirectiveNoFileComp {
			t.Fatalf(
				"%s --%s directive = %d, want no-file",
				test.command,
				test.flag,
				directive,
			)
		}
		if strings.Join(got, ",") != strings.Join(test.want, ",") {
			t.Fatalf(
				"%s --%s completion = %q, want %q",
				test.command,
				test.flag,
				got,
				test.want,
			)
		}
	}
}

func TestConsumeStreamSurfacesCloseError(t *testing.T) {
	closeFailure := errors.New("synthetic close failure")
	stream, err := sessionio.NewStream(
		func(context.Context) (string, error) {
			return "", io.EOF
		},
		func() error {
			return closeFailure
		},
	)
	if err != nil {
		t.Fatalf("create stream: %v", err)
	}
	err = consumeStream(context.Background(), stream, func(string) error {
		return nil
	})
	if !errors.Is(err, closeFailure) {
		t.Fatalf("consumeStream error = %v, want close failure", err)
	}
}

func testReaderRoot(
	t *testing.T,
	now func() time.Time,
	adapters ...sessionio.Adapter,
) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	registry, err := sessionio.NewRegistry(adapters...)
	if err != nil {
		t.Fatalf("create registry: %v", err)
	}
	root := newRoot(
		buildinfo.Info{Version: "0.0.0-test"},
		rootOptions{
			newRegistry: func() (*sessionio.Registry, error) {
				return registry, nil
			},
			now: now,
		},
	)
	var output bytes.Buffer
	var diagnostic bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&diagnostic)
	return root, &output, &diagnostic
}

type fakeReaderAdapter struct {
	descriptor        sessionio.AdapterDescriptor
	sources           []sessionio.Source
	sessions          []sessionio.SessionRef
	itemsBySession    map[sessionio.SessionID][]sessionio.ReadItem
	readTerminalError error
	sourcesCalls      int
	sessionsCalls     int
	readCalls         int
	closeCalls        int
}

func failingExportAdapter(
	session sessionio.SessionRef,
	data []byte,
	terminal error,
) *fakeReaderAdapter {
	return &fakeReaderAdapter{
		descriptor: testDescriptor(sessionio.HarnessCodex),
		sources: []sessionio.Source{
			testSource(
				sessionio.HarnessCodex,
				sessionio.SourceKindCanonical,
				string(session.Occurrence.SourceID),
			),
		},
		sessions: []sessionio.SessionRef{session},
		itemsBySession: map[sessionio.SessionID][]sessionio.ReadItem{
			session.ID: {testReadItem(session, nil, data)},
		},
		readTerminalError: terminal,
	}
}

func (adapter *fakeReaderAdapter) Descriptor() sessionio.AdapterDescriptor {
	return adapter.descriptor
}

func (adapter *fakeReaderAdapter) Sources(
	context.Context,
) (sessionio.Stream[sessionio.Source], error) {
	adapter.sourcesCalls++
	return fakeStream(adapter.sources, nil, func() {
		adapter.closeCalls++
	})
}

func (adapter *fakeReaderAdapter) Sessions(
	context.Context,
	sessionio.SessionRequest,
) (sessionio.Stream[sessionio.SessionRef], error) {
	adapter.sessionsCalls++
	return fakeStream(adapter.sessions, nil, func() {
		adapter.closeCalls++
	})
}

func (adapter *fakeReaderAdapter) Read(
	_ context.Context,
	session sessionio.SessionRef,
) (sessionio.Stream[sessionio.ReadItem], error) {
	adapter.readCalls++
	return fakeStream(
		adapter.itemsBySession[session.ID],
		adapter.readTerminalError,
		func() {
			adapter.closeCalls++
		},
	)
}

func fakeStream[T any](
	values []T,
	terminal error,
	onClose func(),
) (sessionio.Stream[T], error) {
	index := 0
	return sessionio.NewStream(
		func(context.Context) (T, error) {
			var zero T
			if index < len(values) {
				value := values[index]
				index++
				return value, nil
			}
			if terminal != nil {
				return zero, terminal
			}
			return zero, io.EOF
		},
		func() error {
			onClose()
			return nil
		},
	)
}

func testDescriptor(
	harness sessionio.Harness,
) sessionio.AdapterDescriptor {
	return sessionio.AdapterDescriptor{
		Harness: harness,
		Version: "0.0.0-test",
		Capabilities: []sessionio.CapabilityStatus{{
			Capability: sessionio.CapabilityDiscovery,
			Support:    sessionio.SupportFull,
		}},
	}
}

func testSource(
	harness sessionio.Harness,
	kind sessionio.SourceKind,
	id string,
) sessionio.Source {
	return sessionio.Source{
		ID:      sessionio.SourceID(id),
		Harness: harness,
		Kind:    kind,
		Status:  sessionio.SourceStatusAvailable,
		Locator: testLocator("sources/" + id),
	}
}

func testSession(
	harness sessionio.Harness,
	id string,
) sessionio.SessionRef {
	sourceID := sessionio.SourceID("source-" + string(harness))
	return sessionio.SessionRef{
		ID:                sessionio.SessionID(id),
		NativeID:          "native-" + id,
		DiscoveryRevision: sessionio.DiscoveryRevision("discovery-" + id),
		Occurrence: sessionio.SourceOccurrence{
			ID:       sessionio.OccurrenceID("occurrence-" + id),
			SourceID: sourceID,
			Harness:  harness,
			Locator:  testLocator("sessions/" + id),
		},
	}
}

func testReadItem(
	session sessionio.SessionRef,
	timestamp *time.Time,
	data []byte,
) sessionio.ReadItem {
	return sessionio.ReadItem{
		Session: session,
		Observation: sessionio.NativeObservation{
			ID:         sessionio.ObservationID("observation-" + string(session.ID)),
			NativeKind: "test",
			Timestamp:  timestamp,
			Locator:    testLocator("records/" + string(session.ID)),
			Revision: sessionio.Revision{
				Kind:  sessionio.RevisionKindFileSnapshot,
				Value: "revision-" + string(session.ID),
			},
			Representation: sessionio.NativeRepresentation{
				Capture:   sessionio.CaptureKindByteExact,
				MediaType: "application/octet-stream",
				Data:      append([]byte(nil), data...),
				Framing:   []byte("\n"),
			},
		},
	}
}

func testLocator(path string) sessionio.SourceLocator {
	return sessionio.SourceLocator{
		Kind: sessionio.LocatorKindFile,
		File: &sessionio.FileLocator{
			Root: "test-root",
			Path: path,
		},
	}
}

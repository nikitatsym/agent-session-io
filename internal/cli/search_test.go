package cli

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sessionio "github.com/nikitatsym/agent-session-io"
	"github.com/nikitatsym/agent-session-io/internal/catalog"
)

// closedPortListener accepts and immediately closes, so pgx fails fast while
// the test still counts every connection attempt that reached the socket.
type closedPortListener struct {
	address  string
	accepted *atomic.Int64
}

func newClosedPortListener(t *testing.T) closedPortListener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("open the probe listener: %v", err)
	}
	accepted := &atomic.Int64{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			if err := connection.Close(); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Errorf("close the probe listener: %v", err)
		}
		<-done
	})
	return closedPortListener{
		address:  listener.Addr().String(),
		accepted: accepted,
	}
}

func (listener closedPortListener) config(t *testing.T) string {
	t.Helper()
	return writeFixtureFile(t, "config.toml", fmt.Sprintf(
		`schema = "sessionio.config/v1"

[search]
backend = "postgres"
dsn = "postgresql://sessionio@%s/sessionio?sslmode=disable"
schema_name = "sessionio"
`,
		listener.address,
	))
}

// Reader commands must never open the configured PostgreSQL endpoint. The
// catalog case is the counter-proof that the listener does observe a real
// connection attempt when one is made.
func TestReaderCommandsOpenNoPostgresConnection(t *testing.T) {
	listener := newClosedPortListener(t)
	path := listener.config(t)
	adapter := &fakeReaderAdapter{
		descriptor: testDescriptor(sessionio.HarnessCodex),
		sources: []sessionio.Source{testSource(
			sessionio.HarnessCodex,
			sessionio.SourceKindCanonical,
			"source-one",
		)},
		sessions: []sessionio.SessionRef{
			testSession(sessionio.HarnessCodex, "session-one"),
		},
	}
	for _, args := range [][]string{
		{"sources", "--format", "json"},
		{"list", "--format", "json"},
	} {
		root, _, _ := testReaderRoot(t, time.Now, adapter)
		root.SetArgs(append([]string{"--config", path}, args...))
		if err := root.Execute(); err != nil {
			t.Fatalf("execute %v: %v", args, err)
		}
	}
	if attempts := listener.accepted.Load(); attempts != 0 {
		t.Fatalf("reader commands opened %d PostgreSQL connections", attempts)
	}

	root, _, _ := testReaderRoot(t, time.Now, adapter)
	root.SetArgs([]string{"--config", path, "catalog", "init"})
	if err := root.Execute(); err == nil {
		t.Fatal("catalog init succeeded against a closed endpoint")
	}
	if attempts := listener.accepted.Load(); attempts == 0 {
		t.Fatal("the listener observed no connection from a catalog command")
	}
}

func TestSearchRejectsUnknownMode(t *testing.T) {
	root, output, _ := testReaderRoot(t, time.Now)
	root.SetArgs([]string{
		"--config", "testdata/catalog/missing.toml",
		"search", "--mode", "fuzzy", "protocol",
	})
	err := root.Execute()
	if ExitCode(err) != exitInvalid {
		t.Fatalf("exit code = %d, want %d (%v)", ExitCode(err), exitInvalid, err)
	}
	if output.Len() != 0 {
		t.Fatalf("rejected mode wrote stdout: %q", output.String())
	}
}

func TestEmptySearchResultUsesTheNoMatchStatus(t *testing.T) {
	record := searchRecordFrom(
		"nothing",
		catalog.SearchModeLexical,
		20,
		"sessionio",
		catalog.SearchResult{
			Generation:      7,
			GenerationState: catalog.StateComplete,
			Complete:        true,
		},
	)
	root, output, _ := testReaderRoot(t, time.Now)
	err := writeSearchRecord(root, formatJSON, record)
	if ExitCode(err) != exitNoMatch {
		t.Fatalf("exit code = %d, want %d (%v)", ExitCode(err), exitNoMatch, err)
	}
	if !ErrorReported(err) {
		t.Fatal("an empty result was not marked as reported")
	}
	var decoded searchRecord
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatalf("decode search record: %v\n%s", err, output.String())
	}
	if decoded.Schema != searchSchema {
		t.Fatalf("schema = %q, want %q", decoded.Schema, searchSchema)
	}
	if decoded.Matched != 0 || len(decoded.Results) != 0 {
		t.Fatalf("record = %+v, want an empty result set", decoded)
	}
	if !decoded.Complete || decoded.Generation != 7 {
		t.Fatalf("record = %+v, want the active generation and completeness",
			decoded)
	}
}

func TestSearchResultCarriesProvenanceAndMatchedLeg(t *testing.T) {
	record := int64(5)
	score := -1.25
	converted := searchResultFrom(catalog.SearchHit{
		Rank:              1,
		MatchedLeg:        catalog.LegBM25,
		BM25Score:         &score,
		SessionKey:        "session-one",
		Harness:           "codex",
		NativeID:          "native-one",
		SourceID:          "source-one",
		OccurrenceID:      "occurrence-one",
		DiscoveryRevision: "discovery-one",
		SessionLocator: catalog.Locator{
			Kind: "file",
			Root: "/root",
			Path: "a.jsonl",
		},
		PassageID:             3,
		PassageKind:           "tool_result",
		PassageBuilderVersion: "sessionio.passage/v1",
		ProjectionKind:        catalog.ProjectionKindLexical,
		ProjectionVersion:     "sessionio.projection/v1",
		Body:                  "ECONNRESET: socket hang up",
		EventKeys:             []string{"event-one"},
		Evidence: []catalog.HitEvidence{{
			Observation: "observation-one",
			Locator: catalog.Locator{
				Kind:   "file",
				Root:   "/root",
				Path:   "a.jsonl",
				Record: &record,
			},
		}},
	})
	if converted.MatchedLeg != catalog.LegBM25 || converted.BM25Score == nil {
		t.Fatalf("result = %+v, want the BM25 leg and its score", converted)
	}
	if converted.Session.Locator != `file:root="/root" path="a.jsonl"` {
		t.Fatalf("session locator = %q", converted.Session.Locator)
	}
	if len(converted.Evidence) != 1 {
		t.Fatalf("evidence = %d, want 1", len(converted.Evidence))
	}
	want := `file:root="/root" path="a.jsonl" record=5`
	if converted.Evidence[0].Locator != want {
		t.Fatalf("evidence locator = %q, want %q",
			converted.Evidence[0].Locator, want)
	}
	if converted.Passage.ProjectionVersion != "sessionio.projection/v1" {
		t.Fatalf("passage = %+v, want the projection version", converted.Passage)
	}
	if len(converted.Limitations) != 0 {
		t.Fatalf("limitations = %+v, want the empty byte-exact list",
			converted.Limitations)
	}
}

// A consumer decides from the result alone whether a body is byte-exact, so the
// list is always encoded, empty or not.
func TestSearchResultAlwaysEncodesProjectionLimitations(t *testing.T) {
	limited := searchResultFrom(catalog.SearchHit{
		Rank: 1,
		Limitations: []catalog.ProjectionLimitation{{
			Kind:         "nul_removed",
			RemovedBytes: 2,
		}},
	})
	encoded, err := json.Marshal([]searchResult{
		limited,
		searchResultFrom(catalog.SearchHit{Rank: 2}),
	})
	if err != nil {
		t.Fatalf("encode search results: %v", err)
	}
	document := string(encoded)
	want := `"projection_limitations":[{"kind":"nul_removed","removed_bytes":2}]`
	if !strings.Contains(document, want) {
		t.Fatalf("encoded results = %s, want %s", document, want)
	}
	if !strings.Contains(document, `"projection_limitations":[]`) {
		t.Fatalf("encoded results = %s, want an empty list on the second hit",
			document)
	}
}

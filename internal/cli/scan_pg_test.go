//go:build pgintegration

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nikitatsym/agent-session-io/internal/buildinfo"
	"github.com/nikitatsym/agent-session-io/internal/catalog"
)

const cliEndpointEnv = "SESSIONIO_TEST_DATABASE_URL"

var cliSchemaSequence atomic.Int64

// scanFixture owns one temporary Codex corpus and one temporary catalog schema.
type scanFixture struct {
	t          *testing.T
	home       string
	configPath string
	schema     string
	dsn        string
}

func newScanFixture(t *testing.T) *scanFixture {
	t.Helper()
	dsn := os.Getenv(cliEndpointEnv)
	if dsn == "" {
		t.Fatalf("%s must be set for pgintegration tests", cliEndpointEnv)
	}
	root := t.TempDir()
	fixture := &scanFixture{
		t:    t,
		home: filepath.Join(root, "codex"),
		schema: fmt.Sprintf(
			"sessionio_cli_%d_%d",
			os.Getpid(),
			cliSchemaSequence.Add(1),
		),
		dsn: dsn,
	}
	fixture.configPath = filepath.Join(root, "config.toml")
	if err := os.MkdirAll(fixture.home, 0o755); err != nil {
		t.Fatal(err)
	}
	// Both roots are declared: an undeclared Claude root would fall back to the
	// developer's real corpus.
	claude := filepath.Join(root, "claude")
	if err := os.MkdirAll(claude, 0o755); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(
		"schema = \"sessionio.config/v1\"\n\n"+
			"[sources.codex]\nhome = %q\n\n"+
			"[sources.claude]\nconfig_dir = %q\n\n"+
			"[search]\nbackend = \"postgres\"\ndsn = %q\nschema_name = %q\n",
		fixture.home,
		claude,
		dsn,
		fixture.schema,
	)
	if err := os.WriteFile(fixture.configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fixture.dropSchema)
	return fixture
}

func (fixture *scanFixture) dropSchema() {
	ctx := context.Background()
	connection, err := pgx.Connect(ctx, fixture.dsn)
	if err != nil {
		fixture.t.Fatalf("connect to drop schema: %v", err)
	}
	defer func() {
		if err := connection.Close(ctx); err != nil {
			fixture.t.Errorf("close drop connection: %v", err)
		}
	}()
	if _, err := connection.Exec(
		ctx,
		"DROP SCHEMA IF EXISTS "+pgx.Identifier{fixture.schema}.Sanitize()+" CASCADE",
	); err != nil {
		fixture.t.Fatalf("drop schema %s: %v", fixture.schema, err)
	}
}

func (fixture *scanFixture) rollout(name string, records ...string) string {
	fixture.t.Helper()
	path := filepath.Join(
		fixture.home,
		"sessions", "2026", "07", "27",
		"rollout-2026-07-27T10-00-00-"+name+".jsonl",
	)
	fixture.write(path, records...)
	return path
}

func (fixture *scanFixture) write(path string, records ...string) {
	fixture.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fixture.t.Fatal(err)
	}
	body := strings.Join(records, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		fixture.t.Fatal(err)
	}
	fixture.touch(path)
}

// touch advances the modification time so a same-size rewrite still changes the
// adapter's discovery token.
func (fixture *scanFixture) touch(path string) {
	fixture.t.Helper()
	moment := time.Now().Add(time.Duration(cliSchemaSequence.Add(1)) * time.Second)
	if err := os.Chtimes(path, moment, moment); err != nil {
		fixture.t.Fatal(err)
	}
}

func sessionMeta(id string) string {
	return `{"timestamp":"2026-07-27T10:00:00Z","type":"session_meta","payload":` +
		`{"id":"` + id + `","cwd":"/workspace/search04",` +
		`"model_provider":"openai"}}`
}

func userRecord(second int, text string) string {
	return fmt.Sprintf(
		`{"timestamp":"2026-07-27T10:00:%02dZ","type":"response_item",`+
			`"payload":{"type":"message","role":"user",`+
			`"content":[{"type":"input_text","text":%q}]}}`,
		second,
		text,
	)
}

// nulToolOutput carries the JSON escape for U+0000 literally, so the scanned
// projection loses bytes and must report a nul_removed limitation.
func nulToolOutput(second int, call string) []string {
	return []string{
		fmt.Sprintf(
			`{"timestamp":"2026-07-27T10:00:%02dZ","type":"response_item",`+
				`"payload":{"type":"function_call","name":"shell",`+
				`"arguments":"{\"command\":\"tail build.log\"}","call_id":%q}}`,
			second,
			call,
		),
		fmt.Sprintf(
			`{"timestamp":"2026-07-27T10:00:%02dZ","type":"response_item",`+
				`"payload":{"type":"function_call_output","call_id":%q,`+
				`"output":"reading build.log\u0000: checksum mismatch\u0000"}}`,
			second+1,
			call,
		),
	}
}

func (fixture *scanFixture) run(arguments ...string) (string, string, error) {
	fixture.t.Helper()
	root := newRoot(buildinfo.Info{Version: "0.0.0-test"}, rootOptions{
		newRegistry:          newDefaultRegistry,
		newPresenceProviders: newDefaultPresenceProviders,
	})
	var output bytes.Buffer
	var diagnostic bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&diagnostic)
	root.SetArgs(append([]string{"--config", fixture.configPath}, arguments...))
	err := root.ExecuteContext(context.Background())
	return output.String(), diagnostic.String(), err
}

func (fixture *scanFixture) mustRun(arguments ...string) string {
	fixture.t.Helper()
	output, diagnostic, err := fixture.run(arguments...)
	if err != nil {
		fixture.t.Fatalf("%v: %v\n%s\n%s", arguments, err, output, diagnostic)
	}
	return output
}

func (fixture *scanFixture) initialize() {
	fixture.t.Helper()
	fixture.mustRun("catalog", "init", "--format", "json")
}

func (fixture *scanFixture) scan(arguments ...string) scanRecord {
	fixture.t.Helper()
	output := fixture.mustRun(append([]string{"scan", "--format", "json"}, arguments...)...)
	return decodeScanRecord(fixture.t, output)
}

func decodeScanRecord(t *testing.T, output string) scanRecord {
	t.Helper()
	var record scanRecord
	if err := json.Unmarshal([]byte(output), &record); err != nil {
		t.Fatalf("decode scan record: %v\n%s", err, output)
	}
	return record
}

func (fixture *scanFixture) queryInt(query string, arguments ...any) int64 {
	fixture.t.Helper()
	ctx := context.Background()
	connection, err := pgx.Connect(ctx, fixture.dsn)
	if err != nil {
		fixture.t.Fatalf("connect: %v", err)
	}
	defer func() {
		if err := connection.Close(ctx); err != nil {
			fixture.t.Errorf("close: %v", err)
		}
	}()
	var value int64
	statement := strings.ReplaceAll(
		query,
		"{schema}",
		pgx.Identifier{fixture.schema}.Sanitize(),
	)
	if err := connection.QueryRow(ctx, statement, arguments...).Scan(&value); err != nil {
		fixture.t.Fatalf("query %q: %v", statement, err)
	}
	return value
}

func requireChange(t *testing.T, record scanRecord, kind string, want int64) {
	t.Helper()
	if record.Checkpoints[kind] != want {
		t.Fatalf(
			"checkpoint %s = %d, want %d (%+v)",
			kind,
			record.Checkpoints[kind],
			want,
			record.Checkpoints,
		)
	}
}

func TestFirstScanRetainsEvidenceAndCheckpoints(t *testing.T) {
	fixture := newScanFixture(t)
	fixture.rollout(
		"a0000000-0000-4000-8000-000000000001",
		sessionMeta("a0000000-0000-4000-8000-000000000001"),
		userRecord(1, "the retained evidence probe"),
	)
	fixture.initialize()
	record := fixture.scan()
	if record.Counts.Sessions != 1 || record.Retention.SessionsRead != 1 ||
		record.Retention.SessionsReused != 0 {
		t.Fatalf("scan record = %+v", record)
	}
	requireChange(t, record, catalog.ChangeInitial, 1)
	if record.Retention.SnapshotsStored != 1 || record.Retention.RevisionsStored != 1 {
		t.Fatalf("retention = %+v", record.Retention)
	}
	retained := fixture.queryInt("SELECT count(*) FROM {schema}.source")
	if retained != int64(len(record.Sources)) || retained == 0 {
		t.Fatalf("retained sources = %d, reported %d", retained, len(record.Sources))
	}
	if got := fixture.queryInt(
		"SELECT count(*) FROM {schema}.scan_checkpoint WHERE change_kind = $1",
		catalog.ChangeInitial,
	); got != 1 {
		t.Fatalf("initial checkpoints = %d, want 1", got)
	}
	if got := fixture.queryInt(
		"SELECT count(*) FROM {schema}.generation_member",
	); got != 1 {
		t.Fatalf("generation members = %d, want 1", got)
	}
	if got := fixture.queryInt(
		"SELECT uncompressed_size FROM {schema}.snapshot_blob",
	); got == 0 {
		t.Fatal("the retained snapshot is empty")
	}
}

// The reuse path is the whole point of an incremental catalog: an unchanged
// occurrence must be republished without reopening its transcript.
func TestUnchangedSessionsAreReusedWithoutRereading(t *testing.T) {
	fixture := newScanFixture(t)
	fixture.rollout(
		"a0000000-0000-4000-8000-000000000002",
		sessionMeta("a0000000-0000-4000-8000-000000000002"),
		userRecord(1, "reuse probe one"),
	)
	fixture.rollout(
		"a0000000-0000-4000-8000-000000000003",
		sessionMeta("a0000000-0000-4000-8000-000000000003"),
		userRecord(1, "reuse probe two"),
	)
	fixture.initialize()
	first := fixture.scan()
	if first.Counts.Sessions != 2 || first.Retention.SessionsRead != 2 {
		t.Fatalf("first scan = %+v", first)
	}
	firstSearch := fixture.mustRun(
		"search", "--mode", "literal", "--format", "json", "reuse probe two",
	)
	second := fixture.scan()
	if second.Retention.SessionsReused != 2 || second.Retention.SessionsRead != 0 {
		t.Fatalf("second scan = %+v", second)
	}
	requireChange(t, second, catalog.ChangeUnchanged, 2)
	if second.Counts.Sessions != first.Counts.Sessions ||
		second.Counts.Events != first.Counts.Events ||
		second.Counts.Passages != first.Counts.Passages ||
		second.Counts.Projections != first.Counts.Projections ||
		second.Counts.Evidence != first.Counts.Evidence {
		t.Fatalf("reused counts = %+v, want %+v", second.Counts, first.Counts)
	}
	secondSearch := fixture.mustRun(
		"search", "--mode", "literal", "--format", "json", "reuse probe two",
	)
	if normalizeGeneration(firstSearch) != normalizeGeneration(secondSearch) {
		t.Fatalf("reuse changed the result:\n%s\n%s", firstSearch, secondSearch)
	}
}

// normalizeGeneration removes the only field a rescan is expected to change.
func normalizeGeneration(record string) string {
	for _, generation := range []string{"1", "2", "3"} {
		record = strings.ReplaceAll(
			record,
			`"catalog_generation":`+generation+`,`,
			`"catalog_generation":N,`,
		)
	}
	return record
}

func TestCheckpointsClassifyEveryContainerChange(t *testing.T) {
	fixture := newScanFixture(t)
	id := "a0000000-0000-4000-8000-000000000004"
	path := fixture.rollout(id, sessionMeta(id), userRecord(1, "append base"))
	fixture.initialize()
	fixture.scan()

	fixture.write(path, sessionMeta(id), userRecord(1, "append base"),
		userRecord(2, "appended record"))
	grown := fixture.scan()
	requireChange(t, grown, catalog.ChangeGrown, 1)
	if grown.Retention.SessionsRead != 1 {
		t.Fatalf("an appended transcript was not reread: %+v", grown.Retention)
	}

	fixture.write(path, sessionMeta(id), userRecord(1, "append base"))
	truncated := fixture.scan()
	requireChange(t, truncated, catalog.ChangeTruncated, 1)

	fixture.write(path, sessionMeta(id), userRecord(1, "append BASE"))
	rewritten := fixture.scan()
	requireChange(t, rewritten, catalog.ChangeRewritten, 1)

	replacement := filepath.Join(fixture.home, "replacement.jsonl")
	fixture.write(replacement, sessionMeta(id), userRecord(1, "replaced body"))
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	fixture.touch(path)
	replaced := fixture.scan()
	requireChange(t, replaced, catalog.ChangeReplaced, 1)
}

func TestDisappearedOccurrenceIsTombstoned(t *testing.T) {
	fixture := newScanFixture(t)
	first := "a0000000-0000-4000-8000-000000000005"
	second := "a0000000-0000-4000-8000-000000000006"
	fixture.rollout(first, sessionMeta(first), userRecord(1, "kept"))
	removed := fixture.rollout(second, sessionMeta(second), userRecord(1, "gone"))
	fixture.initialize()
	fixture.scan()
	if err := os.Remove(removed); err != nil {
		t.Fatal(err)
	}
	record := fixture.scan()
	if record.Tombstones.Occurrences != 1 || record.Tombstones.Sources != 0 {
		t.Fatalf("tombstones = %+v", record.Tombstones)
	}
	if record.Counts.Sessions != 1 {
		t.Fatalf("sessions after disappearance = %d, want 1", record.Counts.Sessions)
	}
	if got := fixture.queryInt(
		"SELECT count(*) FROM {schema}.source_occurrence" +
			" WHERE disappeared_at IS NOT NULL",
	); got != 1 {
		t.Fatalf("tombstoned occurrences = %d, want 1", got)
	}
	// The retained evidence of a disappeared occurrence survives its tombstone.
	if got := fixture.queryInt(
		"SELECT count(*) FROM {schema}.session_revision",
	); got != 2 {
		t.Fatalf("retained revisions = %d, want 2", got)
	}
}

// Equal bytes share one compressed blob without merging two observations.
func TestCopiedSourcesShareOneBlobAndStayDistinct(t *testing.T) {
	fixture := newScanFixture(t)
	id := "a0000000-0000-4000-8000-000000000007"
	records := []string{sessionMeta(id), userRecord(1, "copied corpus probe")}
	fixture.rollout(id, records...)
	fixture.write(
		filepath.Join(
			fixture.home, "sessions", "2026", "07", "28",
			"rollout-2026-07-28T10-00-00-"+id+".jsonl",
		),
		records...,
	)
	fixture.initialize()
	record := fixture.scan()
	if record.Counts.Sessions != 2 {
		t.Fatalf("copied sources merged: %+v", record.Counts)
	}
	if record.Retention.SnapshotsStored != 1 || record.Retention.SnapshotsReused != 1 {
		t.Fatalf("blob reuse = %+v", record.Retention)
	}
	if got := fixture.queryInt("SELECT count(*) FROM {schema}.snapshot_blob"); got != 1 {
		t.Fatalf("snapshot blobs = %d, want 1", got)
	}
	if got := fixture.queryInt(
		"SELECT count(DISTINCT revision_hash) FROM {schema}.session_revision",
	); got != 2 {
		t.Fatalf("session revisions = %d, want 2", got)
	}
	if got := fixture.queryInt(
		"SELECT count(DISTINCT occurrence_id) FROM {schema}.source_occurrence",
	); got != 2 {
		t.Fatalf("source occurrences = %d, want 2", got)
	}
}

func TestPartialScanPublishesAndReportsFailedSources(t *testing.T) {
	fixture := newScanFixture(t)
	id := "a0000000-0000-4000-8000-000000000008"
	fixture.rollout(id, `{"type":"response_item","payload":{}}`)
	fixture.initialize()
	if _, _, err := fixture.run("scan", "--format", "json"); err == nil {
		t.Fatal("a broken source did not fail the default scan")
	}
	output, _, err := fixture.run("scan", "--format", "json", "--partial")
	if ExitCode(err) != exitPartial {
		t.Fatalf("partial scan exit = %d, want %d (%v)", ExitCode(err), exitPartial, err)
	}
	record := decodeScanRecord(t, output)
	if record.State != catalog.StatePartial || len(record.FailedSources) != 1 {
		t.Fatalf("partial record = %+v", record)
	}
	if got := fixture.queryInt(
		"SELECT count(*) FROM {schema}.generation WHERE state = 'partial'",
	); got != 1 {
		t.Fatalf("partial generations = %d, want 1", got)
	}
}

func TestCatalogStateRoundTripsThroughAnEmptyTarget(t *testing.T) {
	source := newScanFixture(t)
	id := "a0000000-0000-4000-8000-000000000009"
	source.rollout(id, sessionMeta(id), userRecord(1, "state stream probe"))
	source.initialize()
	source.scan()
	stream := filepath.Join(t.TempDir(), "state.ndjson")
	exported := source.mustRun(
		"catalog", "state", "export", "--output", stream, "--format", "json",
	)
	var exportRecord stateRecord
	if err := json.Unmarshal([]byte(exported), &exportRecord); err != nil {
		t.Fatalf("decode export record: %v", err)
	}
	target := newScanFixture(t)
	target.initialize()
	imported := target.mustRun(
		"catalog", "state", "import", "--input", stream, "--format", "json",
	)
	var importRecord stateRecord
	if err := json.Unmarshal([]byte(imported), &importRecord); err != nil {
		t.Fatalf("decode import record: %v", err)
	}
	if importRecord.Counts != exportRecord.Counts ||
		importRecord.Checksum != exportRecord.Checksum {
		t.Fatalf("import %+v does not match export %+v", importRecord, exportRecord)
	}
	if got := target.queryInt(
		"SELECT count(*) FROM {schema}.session_revision",
	); got != exportRecord.Counts.SessionRevisions {
		t.Fatalf("imported revisions = %d, want %d",
			got, exportRecord.Counts.SessionRevisions)
	}
	// A second import must refuse the now non-empty target and change nothing.
	_, _, err := target.run(
		"catalog", "state", "import", "--input", stream, "--format", "json",
	)
	if ExitCode(err) != exitInvalid {
		t.Fatalf("second import exit = %d, want %d (%v)", ExitCode(err), exitInvalid, err)
	}
	if got := target.queryInt(
		"SELECT count(*) FROM {schema}.session_revision",
	); got != exportRecord.Counts.SessionRevisions {
		t.Fatalf("refused import changed the target: %d rows", got)
	}
	if _, _, err := source.run(
		"catalog", "state", "export", "--output", stream, "--format", "json",
	); ExitCode(err) != exitInvalid {
		t.Fatalf("export overwrote an existing stream: %v", err)
	}
}

// Reuse copies the limitation rows of an unchanged session, so the rescan must
// report the same limitation count as the scan that built them.
func TestReusedSessionsKeepTheirLimitationCount(t *testing.T) {
	fixture := newScanFixture(t)
	id := "a0000000-0000-4000-8000-000000000012"
	fixture.rollout(id, append(
		[]string{sessionMeta(id), userRecord(1, "limitation reuse probe")},
		nulToolOutput(2, "call-nul")...,
	)...)
	fixture.initialize()
	first := fixture.scan()
	if first.Counts.Limitations == 0 {
		t.Fatalf("the NUL fixture reported no limitation: %+v", first.Counts)
	}
	second := fixture.scan()
	if second.Retention.SessionsReused != 1 || second.Retention.SessionsRead != 0 {
		t.Fatalf("second scan did not reuse the session: %+v", second.Retention)
	}
	if second.Counts.Limitations != first.Counts.Limitations {
		t.Fatalf(
			"reused limitations = %d, want the %d the first scan reported",
			second.Counts.Limitations,
			first.Counts.Limitations,
		)
	}
	rows := fixture.queryInt(fmt.Sprintf(
		"SELECT count(*) FROM {schema}.projection_limitation_g%d",
		second.Generation,
	))
	if rows != second.Counts.Limitations {
		t.Fatalf(
			"the reused generation holds %d limitation rows but reports %d",
			rows,
			second.Counts.Limitations,
		)
	}
	removed := fixture.queryInt(fmt.Sprintf(
		"SELECT sum(removed_bytes) FROM {schema}.projection_limitation_g%d",
		second.Generation,
	))
	if removed != 2 {
		t.Fatalf("reused removed bytes = %d, want the 2 NUL bytes", removed)
	}
}

// A moved transcript is a new occurrence of the same source: the old occurrence
// disappeared, the bytes are shared, and neither observation absorbs the other.
func TestMovedSourceTombstonesTheOldOccurrenceAndSharesTheBlob(t *testing.T) {
	fixture := newScanFixture(t)
	id := "a0000000-0000-4000-8000-000000000013"
	from := fixture.rollout(
		id,
		sessionMeta(id),
		userRecord(1, "moved source probe"),
	)
	fixture.initialize()
	first := fixture.scan()
	if first.Counts.Sessions != 1 || first.Retention.SnapshotsStored != 1 {
		t.Fatalf("first scan = %+v", first)
	}
	to := filepath.Join(
		fixture.home,
		"sessions", "2026", "07", "28",
		"rollout-2026-07-28T10-00-00-"+id+".jsonl",
	)
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	second := fixture.scan()
	if second.Counts.Sessions != 1 || second.Retention.SessionsRead != 1 {
		t.Fatalf("the moved transcript was not rescanned: %+v", second)
	}
	if second.Tombstones.Occurrences != 1 || second.Tombstones.Sources != 0 {
		t.Fatalf("tombstones = %+v, want the old occurrence only", second.Tombstones)
	}
	if second.Retention.SnapshotsStored != 0 || second.Retention.SnapshotsReused != 1 {
		t.Fatalf("the moved bytes were stored again: %+v", second.Retention)
	}
	if got := fixture.queryInt(
		"SELECT count(*) FROM {schema}.snapshot_blob",
	); got != 1 {
		t.Fatalf("snapshot blobs = %d, want the one shared blob", got)
	}
	if got := fixture.queryInt(
		"SELECT count(*) FROM {schema}.source_occurrence" +
			" WHERE disappeared_at IS NOT NULL",
	); got != 1 {
		t.Fatalf("tombstoned occurrences = %d, want 1", got)
	}
	live := fixture.queryInt(
		"SELECT count(*) FROM {schema}.source_occurrence"+
			" WHERE disappeared_at IS NULL AND locator_path = $1",
		filepath.ToSlash(strings.TrimPrefix(to, fixture.home+string(filepath.Separator))),
	)
	if live != 1 {
		t.Fatalf("live occurrences at the new path = %d, want 1", live)
	}
	// The move must not merge the two observations into one identity.
	if got := fixture.queryInt(
		"SELECT count(*) FROM {schema}.source_occurrence",
	); got != 2 {
		t.Fatalf("retained occurrences = %d, want 2", got)
	}
	if got := fixture.queryInt(
		"SELECT count(DISTINCT session_key) FROM {schema}.session_revision",
	); got != 2 {
		t.Fatalf("retained session keys = %d, want 2", got)
	}
	if got := fixture.queryInt(
		"SELECT count(*) FROM {schema}.source",
	); got != int64(len(second.Sources)) {
		t.Fatalf("retained sources = %d, reported %d", got, len(second.Sources))
	}
}

// A symlinked transcript is never a second observation: the adapter skips it,
// so the catalog retains one occurrence for the one real container.
func TestSymlinkedTranscriptIsNeverRetained(t *testing.T) {
	fixture := newScanFixture(t)
	id := "a0000000-0000-4000-8000-000000000010"
	real := fixture.rollout(id, sessionMeta(id), userRecord(1, "symlink probe"))
	link := filepath.Join(
		filepath.Dir(real),
		"rollout-2026-07-27T11-00-00-a0000000-0000-4000-8000-000000000011.jsonl",
	)
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("create fixture symlink: %v", err)
	}
	fixture.initialize()
	record := fixture.scan()
	if record.Counts.Sessions != 1 || record.Retention.ObservedOccurred != 1 {
		t.Fatalf("symlink became an observation: %+v", record)
	}
	if got := fixture.queryInt(
		"SELECT count(*) FROM {schema}.source_occurrence",
	); got != 1 {
		t.Fatalf("retained occurrences = %d, want 1", got)
	}
}

//go:build pgintegration

package cli

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/nikitatsym/agent-session-io/internal/catalog"
)

func decodeSearchRecord(t *testing.T, output string) searchRecord {
	t.Helper()
	var record searchRecord
	if err := json.Unmarshal([]byte(output), &record); err != nil {
		t.Fatalf("decode search record: %v\n%s", err, output)
	}
	return record
}

func (fixture *scanFixture) search(arguments ...string) (searchRecord, string) {
	fixture.t.Helper()
	output, diagnostic, err := fixture.run(
		append([]string{"search", "--format", "json"}, arguments...)...,
	)
	if err != nil {
		fixture.t.Fatalf("search %v: %v\n%s\n%s", arguments, err, output, diagnostic)
	}
	return decodeSearchRecord(fixture.t, output), diagnostic
}

// plantKilledCandidate leaves behind exactly what a scan killed with SIGKILL
// leaves: a building generation, its membership, and derived rows that no live
// generation presents but that the shared retrieval indexes still cover.
const plantKilledCandidate = `DO $$
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
				SELECT nextval('{schema}.search_document_id'), planted, session_ref,
					harness, NULL, projection_kind, projection_version, body,
					content_hash
				FROM {schema}.search_document WHERE doc_id = doc;
		END LOOP;
		INSERT INTO {schema}.generation_member (generation_id, derived_id)
			VALUES (candidate, planted);
	END LOOP;
END $$`

func (fixture *scanFixture) unreclaimed() int64 {
	fixture.t.Helper()
	return fixture.queryInt(
		"SELECT count(*) FROM {schema}.generation" +
			" WHERE reclaimed_at IS NULL AND id NOT IN" +
			" (SELECT generation_id FROM {schema}.active_generation)",
	)
}

func (fixture *scanFixture) activeGeneration() int64 {
	fixture.t.Helper()
	return fixture.queryInt(
		"SELECT generation_id FROM {schema}.active_generation",
	)
}

// scanPartial publishes a partial generation. The command exits 4 by contract,
// so the record is decoded from the run that reported that status.
func (fixture *scanFixture) scanPartial() scanRecord {
	fixture.t.Helper()
	output, diagnostic, err := fixture.run("scan", "--partial", "--format", "json")
	if ExitCode(err) != exitPartial {
		fixture.t.Fatalf("scan --partial exit = %d, want %d (%v)\n%s\n%s",
			ExitCode(err), exitPartial, err, output, diagnostic)
	}
	record := decodeScanRecord(fixture.t, output)
	if record.State != catalog.StatePartial {
		fixture.t.Fatalf("scan --partial published %q, want a partial generation",
			record.State)
	}
	return record
}

func (fixture *scanFixture) failedSources(generation int64) []catalog.SourceFailure {
	fixture.t.Helper()
	opened, err := catalog.New(catalog.Settings{
		SchemaName: fixture.schema,
		DSN:        fixture.dsn,
	})
	if err != nil {
		fixture.t.Fatalf("open the catalog: %v", err)
	}
	defer opened.Close()
	failures, err := opened.FailedSources(
		context.Background(),
		catalog.GenerationID(generation),
	)
	if err != nil {
		fixture.t.Fatalf("read the failed sources of generation %d: %v",
			generation, err)
	}
	return failures
}

// failedScope reduces a declared failed set to what the catch-up tolerance is
// about: which source, in which harness. The reason text is a diagnostic.
func failedScope(failures []catalog.SourceFailure) string {
	scope := make([]string, 0, len(failures))
	for _, failure := range failures {
		scope = append(scope, failure.Harness+"/"+failure.SourceID)
	}
	sort.Strings(scope)
	return strings.Join(scope, " ")
}

// A permanently broken source is authorized once, by an explicit scan
// --partial. An unrelated healthy session drifting afterwards must not take the
// catalog away: the catch-up the gate runs continues that authorization at the
// same scope and publishes the same failed set instead of failing the search on
// a source the active generation already declares broken.
func TestSearchCatchesUpWithinTheDeclaredPartialScope(t *testing.T) {
	fixture := newScanFixture(t)
	fixture.rollout(
		"c0000000-0000-4000-8000-000000000410",
		userRecord(1, "a codex transcript that starts without session metadata"),
	)
	first := "c0000000-0000-4000-8000-000000000411"
	fixture.claudeTranscript(
		first,
		claudeRecord(first, "partial-scope-first", "the first healthy probe"),
	)
	fixture.initialize()
	partial := fixture.scanPartial()

	later := "c0000000-0000-4000-8000-000000000412"
	fixture.claudeTranscript(
		later,
		claudeRecord(later, "partial-scope-later", "the later healthy probe"),
	)

	record, diagnostic := fixture.search("--mode", "literal", "healthy probe")
	if !record.Refresh.Ran || record.Refresh.Reason != refreshReasonStale {
		t.Fatalf("refresh = %+v, want a stale catch-up (%s)",
			record.Refresh, diagnostic)
	}
	if record.Generation <= partial.Generation {
		t.Fatalf("generation = %d, want a newer one than the partial %d",
			record.Generation, partial.Generation)
	}
	if record.State != catalog.StatePartial || record.Complete {
		t.Fatalf("answer came from %q (complete %v), want the partial generation"+
			" the catch-up published", record.State, record.Complete)
	}
	if record.Matched != 2 {
		t.Fatalf("matched = %d, want both healthy sessions", record.Matched)
	}
	if got, want := failedScope(fixture.failedSources(record.Generation)),
		failedScope(partial.FailedSources); got != want {
		t.Fatalf("the catch-up declared %q failed, want the authorized %q",
			got, want)
	}
}

// Tolerating a declared failure still attempts the source: once it can be read
// the catch-up includes it, the generation heals to complete, and the answers
// after it need no catch-up at all.
func TestACatchUpHealsAPartialGenerationWhenTheSourceRecovers(t *testing.T) {
	fixture := newScanFixture(t)
	broken := "c0000000-0000-4000-8000-000000000413"
	fixture.rollout(
		broken,
		userRecord(1, "a codex transcript that starts without session metadata"),
	)
	claude := "c0000000-0000-4000-8000-000000000414"
	fixture.claudeTranscript(
		claude,
		claudeRecord(claude, "heal-probe", "the healthy probe"),
	)
	fixture.initialize()
	partial := fixture.scanPartial()

	fixture.rollout(broken, sessionMeta(broken), userRecord(1, "the healed probe"))

	record, diagnostic := fixture.search("--mode", "literal", "the healed probe")
	if !record.Refresh.Ran {
		t.Fatalf("the gate did not catch up with the healed source (%s)", diagnostic)
	}
	if record.State != catalog.StateComplete || !record.Complete {
		t.Fatalf("answer came from %q (complete %v), want a complete generation",
			record.State, record.Complete)
	}
	if record.Generation <= partial.Generation || record.Matched != 1 {
		t.Fatalf("generation %d matched %d, want the healed session in a newer"+
			" generation than %d", record.Generation, record.Matched,
			partial.Generation)
	}
	if failures := fixture.failedSources(record.Generation); len(failures) != 0 {
		t.Fatalf("the healed generation still declares %+v failed", failures)
	}
	quiescent, _ := fixture.search("--mode", "literal", "the healed probe")
	if quiescent.Refresh.Ran {
		t.Fatalf("the healed catalog scanned again: %+v", quiescent.Refresh)
	}
}

// Tolerance never reaches a source the active generation does not declare: a
// healthy source that breaks while a catch-up runs fails the search loudly and
// publishes nothing, so an automatic scan can never widen a partial generation.
func TestANewlyBrokenSourceFailsTheCatchUp(t *testing.T) {
	fixture := newScanFixture(t)
	fixture.rollout(
		"c0000000-0000-4000-8000-000000000415",
		userRecord(1, "a codex transcript that starts without session metadata"),
	)
	claude := "c0000000-0000-4000-8000-000000000416"
	transcript := fixture.claudeTranscript(
		claude,
		claudeRecord(claude, "newly-broken-probe", "the healthy probe"),
	)
	fixture.initialize()
	partial := fixture.scanPartial()

	// The transcript changes while it is still readable and the listing cache
	// takes its new stat identity, so the gate lists it and the catch-up scan is
	// what discovers that this session can no longer be read.
	fixture.claudeTranscript(
		claude,
		claudeRecord(claude, "newly-broken-probe", "the healthy probe"),
		claudeRecord(claude, "newly-broken-later", "the later healthy probe"),
	)
	fixture.mustRun("list", "--harness", "claude")
	unreadable(t, transcript)

	output, diagnostic, err := fixture.run(
		"search", "--format", "json", "--mode", "literal", "healthy probe",
	)
	if ExitCode(err) != exitIntegrity {
		t.Fatalf("search exit = %d, want %d (%v)\n%s\n%s",
			ExitCode(err), exitIntegrity, err, output, diagnostic)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("failure = %v, want the unreadable transcript", err)
	}
	if active := fixture.activeGeneration(); active != partial.Generation {
		t.Fatalf("active generation = %d, want the partial %d it started from",
			active, partial.Generation)
	}
	if left := fixture.unreclaimed(); left != 0 {
		t.Fatalf("the failed catch-up left %d unreclaimed generations", left)
	}
}

// A quiescent catalog answers from the generation it already has: the gate
// costs one listing and publishes nothing.
func TestSearchAnswersAQuiescentCatalogWithoutScanning(t *testing.T) {
	fixture := newScanFixture(t)
	id := "c0000000-0000-4000-8000-000000000401"
	fixture.rollout(id, sessionMeta(id), userRecord(1, "quiescent gate probe"))
	fixture.initialize()
	first := fixture.scan()
	record, diagnostic := fixture.search(
		"--mode", "literal", "quiescent gate probe",
	)
	if record.Refresh.Ran {
		t.Fatalf("a quiescent catalog scanned: %+v", record.Refresh)
	}
	if record.Generation != first.Generation {
		t.Fatalf("answered generation %d, want %d",
			record.Generation, first.Generation)
	}
	if diagnostic != "" {
		t.Fatalf("a quiescent search wrote stderr: %q", diagnostic)
	}
	if got := fixture.queryInt("SELECT count(*) FROM {schema}.generation"); got != 1 {
		t.Fatalf("generations = %d, want the one the scan published", got)
	}
}

// A session that appeared after the last scan makes the catalog stale, and the
// gate must catch up before it answers instead of answering from the old set.
func TestSearchCatchesUpWithAStaleCatalog(t *testing.T) {
	fixture := newScanFixture(t)
	first := "c0000000-0000-4000-8000-000000000402"
	fixture.rollout(first, sessionMeta(first), userRecord(1, "the first probe"))
	fixture.initialize()
	scanned := fixture.scan()
	second := "c0000000-0000-4000-8000-000000000403"
	fixture.rollout(second, sessionMeta(second), userRecord(1, "the later probe"))

	record, diagnostic := fixture.search("--mode", "literal", "the later probe")
	if !record.Refresh.Ran || record.Refresh.SessionsBehind != 1 {
		t.Fatalf("refresh = %+v, want one session behind", record.Refresh)
	}
	if record.Refresh.Reason != refreshReasonStale {
		t.Fatalf("refresh reason = %q, want %q",
			record.Refresh.Reason, refreshReasonStale)
	}
	if !strings.Contains(diagnostic, "1 session behind") {
		t.Fatalf("stderr = %q, want the catching-up message", diagnostic)
	}
	if record.Generation <= scanned.Generation {
		t.Fatalf("generation = %d, want a newer one than %d",
			record.Generation, scanned.Generation)
	}
	if record.Matched != 1 {
		t.Fatalf("matched = %d, want the session added after the scan", record.Matched)
	}
	if !strings.Contains(record.Results[0].Passage.Text, "the later probe") {
		t.Fatalf("hit = %+v, want the new session", record.Results[0].Passage)
	}
}

// The garbage of a killed scan is swept by the scan the next search triggers,
// and the answer from the repaired catalog is the answer from before the kill,
// BM25 scores included.
func TestSearchSweepsAKilledCandidateAndAnswersByteIdentically(t *testing.T) {
	fixture := newScanFixture(t)
	for index, text := range []string{
		"shared derived storage keeps one row per builder version",
		"why did the derived storage become shared between generations",
	} {
		id := "c000000" + string(rune('0'+index)) +
			"-0000-4000-8000-000000000404"
		fixture.rollout(id, sessionMeta(id), userRecord(1, text))
	}
	fixture.initialize()
	fixture.scan()
	before, _ := fixture.search("--mode", "lexical", "shared derived storage")
	if before.Matched == 0 || before.Results[0].BM25Score == nil {
		t.Fatalf("the lexical probe matched nothing: %+v", before)
	}
	baseline := fixture.derivedRows()

	fixture.exec(plantKilledCandidate)
	if fixture.unreclaimed() != 1 {
		t.Fatalf("planting left %d unreclaimed generations, want 1",
			fixture.unreclaimed())
	}

	after, diagnostic := fixture.search("--mode", "lexical", "shared derived storage")
	if !after.Refresh.Ran || after.Refresh.Reason != refreshReasonUnreclaimed {
		t.Fatalf("refresh = %+v, want the unreclaimed reason", after.Refresh)
	}
	if !strings.Contains(diagnostic, "unreclaimed") {
		t.Fatalf("stderr = %q, want the repair message", diagnostic)
	}
	if fixture.unreclaimed() != 0 {
		t.Fatalf("the catch-up scan left %d unreclaimed generations",
			fixture.unreclaimed())
	}
	rows := fixture.derivedRows()
	for table, count := range baseline {
		if rows[table] != count {
			t.Fatalf("%s holds %d rows after the sweep, want the %d before the kill",
				table, rows[table], count)
		}
	}
	if len(after.Results) != len(before.Results) {
		t.Fatalf("results = %d, want the %d from before the kill",
			len(after.Results), len(before.Results))
	}
	for index := range before.Results {
		want := before.Results[index]
		got := after.Results[index]
		if *got.BM25Score != *want.BM25Score {
			t.Fatalf("rank %d scored %v after the sweep, want %v",
				got.Rank, *got.BM25Score, *want.BM25Score)
		}
	}
	if quiescentAnswer(t, before) != quiescentAnswer(t, after) {
		t.Fatalf("the sweep changed the answer:\n%+v\n%+v", before, after)
	}
}

// quiescentAnswer is the answer without the two facts a repair is allowed to
// change: which generation answered, and whether the gate had to scan.
func quiescentAnswer(t *testing.T, record searchRecord) string {
	t.Helper()
	record.Generation = 0
	record.Refresh = searchRefresh{}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("encode search record: %v", err)
	}
	return string(encoded)
}

// The catalog is single-writer: while one scan holds the lease, a second scan
// and every catalog-backed search are refused with the same typed failure.
func TestASecondWriterAndASearchAreRefusedWhileAScanRuns(t *testing.T) {
	fixture := newScanFixture(t)
	id := "c0000000-0000-4000-8000-000000000405"
	fixture.rollout(id, sessionMeta(id), userRecord(1, "single writer probe"))
	fixture.initialize()
	fixture.scan()

	holder, err := catalog.New(catalog.Settings{
		SchemaName: fixture.schema,
		DSN:        fixture.dsn,
	})
	if err != nil {
		t.Fatalf("open the holding catalog: %v", err)
	}
	defer holder.Close()
	lease, err := holder.AcquireScanLease(context.Background())
	if err != nil {
		t.Fatalf("acquire the scan lease: %v", err)
	}
	for _, arguments := range [][]string{
		{"scan", "--format", "json"},
		{"search", "--format", "json", "--mode", "literal", "single writer probe"},
	} {
		output, _, err := fixture.run(arguments...)
		if ExitCode(err) != exitCapability {
			t.Fatalf("%v exit = %d, want %d (%v)",
				arguments, ExitCode(err), exitCapability, err)
		}
		if !strings.Contains(output, `"kind":"scan_in_progress"`) {
			t.Fatalf("%v reported %q, want the scan_in_progress failure",
				arguments, output)
		}
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatalf("release the scan lease: %v", err)
	}
	fixture.scan()
	if _, _, err := fixture.run(
		"search", "--format", "json", "--mode", "literal", "single writer probe",
	); err != nil {
		t.Fatalf("search after the lease was released: %v", err)
	}
}

// refuseReclaim makes the next reclaim fail the way a broken catalog would,
// after the generation it belongs to is already published and active.
const refuseReclaim = `CREATE FUNCTION {schema}.refuse_reclaim()
	RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	RAISE EXCEPTION 'reclaim refused by the fixture';
END $$;
CREATE TRIGGER refuse_reclaim BEFORE DELETE ON {schema}.derived_session
	FOR EACH ROW EXECUTE FUNCTION {schema}.refuse_reclaim()`

// A reclaim that fails after publication must report the truth: the generation
// is published and active, and only the reclaim failed.
func TestReclaimFailureAfterPublicationKeepsTheScanRecord(t *testing.T) {
	fixture := newScanFixture(t)
	id := "c0000000-0000-4000-8000-000000000408"
	fixture.rollout(id, sessionMeta(id), userRecord(1, "reclaim failure probe"))
	fixture.initialize()
	first := fixture.scan()
	fixture.supersedeBuilder()
	fixture.exec(refuseReclaim)

	output, diagnostic, err := fixture.run("scan", "--format", "json")
	if ExitCode(err) != exitIntegrity {
		t.Fatalf("scan exit = %d, want %d (%v)", ExitCode(err), exitIntegrity, err)
	}
	record := decodeScanRecord(t, output)
	if record.State != catalog.StateComplete || record.Generation <= first.Generation {
		t.Fatalf("scan record = %+v, want the published generation", record)
	}
	if !strings.Contains(diagnostic, "reclaim failed") {
		t.Fatalf("stderr = %q, want the reclaim failure", diagnostic)
	}
	if active := fixture.queryInt(
		"SELECT generation_id FROM {schema}.active_generation",
	); active != record.Generation {
		t.Fatalf("active generation = %d, want the published %d",
			active, record.Generation)
	}
	fixture.exec("DROP TRIGGER refuse_reclaim ON {schema}.derived_session")
	repaired := fixture.scan()
	if repaired.Reclaimed == 0 {
		t.Fatalf("the next scan reclaimed nothing: %+v", repaired)
	}
	if left := fixture.unreclaimed(); left != 0 {
		t.Fatalf("%d generations are still unreclaimed", left)
	}
}

// The freshness gate reads stat identity and the catalog, never a transcript:
// with every transcript unreadable and a warm listing cache the search still
// answers. The cold-cache leg is the counter-proof that the mode really bites.
func TestTheFreshnessGateOpensNoTranscript(t *testing.T) {
	fixture := newScanFixture(t)
	id := "c0000000-0000-4000-8000-000000000409"
	path := fixture.rollout(id, sessionMeta(id), userRecord(1, "gate probe"))
	fixture.initialize()
	fixture.scan()
	unreadable(t, path)

	record, diagnostic := fixture.search("--mode", "literal", "gate probe")
	if record.Refresh.Ran {
		t.Fatalf("the gate scanned an unchanged catalog: %+v", record.Refresh)
	}
	if record.Matched != 1 {
		t.Fatalf("matched = %d, want the retained passage (%s)",
			record.Matched, diagnostic)
	}
	fixture.dropCache()
	_, _, err := fixture.run(
		"search", "--format", "json", "--mode", "literal", "gate probe",
	)
	if err == nil {
		t.Fatal("a cold gate listed unreadable transcripts, so the case is vacuous")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("cold gate failure = %v, want the unreadable transcript", err)
	}
}

// A scan that fails after writing rows reclaims them before it exits, so the
// next search finds a quiescent catalog and answers without scanning.
func TestAFailedScanReclaimsItsOwnRows(t *testing.T) {
	fixture := newScanFixture(t)
	id := "c0000000-0000-4000-8000-000000000406"
	fixture.rollout(id, sessionMeta(id), userRecord(1, "self reclaim probe"))
	claude := "c0000000-0000-4000-8000-000000000407"
	transcript := fixture.claudeTranscript(
		claude,
		claudeRecord(claude, "self-reclaim-record", "the claude probe"),
	)
	fixture.initialize()
	fixture.scan()
	before, _ := fixture.search("--mode", "literal", "self reclaim probe")
	baseline := fixture.derivedRows()

	// A builder bump rebuilds both sessions; the unreadable one fails the scan
	// after the readable one has already written its rows.
	fixture.supersedeBuilder()
	unreadable(t, transcript)
	if _, _, err := fixture.run("scan", "--format", "json"); err == nil {
		t.Fatal("a scan of an unreadable source reported success")
	}
	if left := fixture.unreclaimed(); left != 0 {
		t.Fatalf("the failed scan left %d unreclaimed generations", left)
	}
	rows := fixture.derivedRows()
	for table, count := range baseline {
		if rows[table] != count {
			t.Fatalf("%s holds %d rows after the failed scan, want %d",
				table, rows[table], count)
		}
	}
	after, diagnostic := fixture.search("--mode", "literal", "self reclaim probe")
	if after.Refresh.Ran {
		t.Fatalf("the catalog was not quiescent after the failure: %q", diagnostic)
	}
	if quiescentAnswer(t, before) != quiescentAnswer(t, after) {
		t.Fatalf("the failed scan changed the answer:\n%+v\n%+v", before, after)
	}
}

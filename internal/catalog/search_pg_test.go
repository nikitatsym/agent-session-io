//go:build pgintegration

package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	fixtureBuilderVersion    = "fixture.passage/v1"
	fixtureProjectionVersion = "fixture.projection/v1"
	fixtureBuilderKey        = "passage=" + fixtureBuilderVersion +
		";projection=" + fixtureProjectionVersion
)

func scanPassageFixture(index int, body string, facet FacetFilter) ScanPassage {
	return ScanPassage{
		Kind:              "user",
		BuilderVersion:    fixtureBuilderVersion,
		ProjectionKind:    ProjectionKindLexical,
		ProjectionVersion: fixtureProjectionVersion,
		Events:            []int{index},
		Body:              body,
		ContentHash:       []byte(body),
		Parts:             1,
		Facets:            []FacetFilter{facet},
	}
}

func scanSessionFixture(key string, bodies ...string) ScanSession {
	root := "/fixtures"
	path := key + ".jsonl"
	session := ScanSession{
		Key:               key,
		Harness:           "codex",
		NativeID:          "native-" + key,
		Title:             "fixture " + key,
		SourceID:          "source-" + key,
		OccurrenceID:      "occurrence-" + key,
		DiscoveryRevision: "discovery-" + key,
		Locator:           Locator{Kind: "file", Root: root, Path: path},
	}
	session.SourceRevisionKind = "file_snapshot"
	session.SourceRevisionValue = "sha256:" + key
	for index, body := range bodies {
		record := int64(index + 1)
		session.Events = append(session.Events, ScanEvent{
			Key:         fmt.Sprintf("%s-event-%d", key, index),
			Kind:        "message",
			Role:        "user",
			Observation: fmt.Sprintf("%s-observation-%d", key, index),
			Evidence: []ScanEvidence{{
				Observation: fmt.Sprintf("%s-observation-%d", key, index),
				Locator: Locator{
					Kind:   "file",
					Root:   root,
					Path:   path,
					Record: &record,
				},
			}},
		})
		session.Passages = append(
			session.Passages,
			scanPassageFixture(index, body, wantedFilter()),
		)
	}
	return session
}

// retainFixture stores the evidence chain a derived session references, so a
// fixture goes through the same foreign keys as a real scan.
func retainFixture(t *testing.T, catalog *Catalog, session ScanSession) []byte {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := catalog.ObserveSource(ctx, RetainedSource{
		SourceID: session.SourceID,
		Harness:  session.Harness,
		Locator:  session.Locator,
	}, now); err != nil {
		t.Fatalf("retain fixture source: %v", err)
	}
	if err := catalog.ObserveOccurrence(ctx, RetainedOccurrence{
		OccurrenceID: session.OccurrenceID,
		SourceID:     session.SourceID,
		Harness:      session.Harness,
		Locator:      session.Locator,
	}, now); err != nil {
		t.Fatalf("retain fixture occurrence: %v", err)
	}
	blob, err := CompressSnapshot([]byte(session.Key))
	if err != nil {
		t.Fatalf("compress fixture snapshot: %v", err)
	}
	if _, err := catalog.PutSnapshot(ctx, blob, now); err != nil {
		t.Fatalf("retain fixture snapshot: %v", err)
	}
	revision := SessionRevision{
		SessionKey:          session.Key,
		OccurrenceID:        session.OccurrenceID,
		Harness:             session.Harness,
		NativeID:            session.NativeID,
		Title:               session.Title,
		DiscoveryRevision:   session.DiscoveryRevision,
		SourceRevisionKind:  session.SourceRevisionKind,
		SourceRevisionValue: session.SourceRevisionValue,
		SnapshotHash:        blob.ContentHash,
		Locator:             session.Locator,
	}
	revision.RevisionHash = RevisionHash(revision)
	if _, err := catalog.PutSessionRevision(ctx, revision, now); err != nil {
		t.Fatalf("retain fixture revision: %v", err)
	}
	return revision.RevisionHash
}

// presentSession follows the production path: derived rows are written only
// when this revision and builder-version set have none.
func presentSession(
	t *testing.T,
	catalog *Catalog,
	writer *DerivedWriter,
	generation GenerationID,
	session ScanSession,
) int64 {
	t.Helper()
	ctx := context.Background()
	session.RevisionHash = retainFixture(t, catalog, session)
	derived, found, err := catalog.FindDerivedSession(
		ctx,
		session.RevisionHash,
		writer.builderKey,
	)
	if err != nil {
		t.Fatalf("locate derived session %s: %v", session.Key, err)
	}
	if !found {
		derived, err = writer.WriteSession(ctx, session)
		if err != nil {
			t.Fatalf("write scan session %s: %v", session.Key, err)
		}
	}
	if err := catalog.AddGenerationMember(ctx, generation, derived); err != nil {
		t.Fatalf("present scan session %s: %v", session.Key, err)
	}
	return derived
}

func publishedScan(
	t *testing.T,
	catalog *Catalog,
	parent *GenerationID,
	sessions ...ScanSession,
) GenerationID {
	t.Helper()
	generation, err := catalog.BeginCandidate(context.Background(), parent)
	if err != nil {
		t.Fatalf("begin candidate: %v", err)
	}
	writer := catalog.NewDerivedWriter(fixtureBuilderKey)
	for _, session := range sessions {
		presentSession(t, catalog, writer, generation, session)
	}
	publishGeneration(t, catalog, generation)
	return generation
}

func mustSearch(
	t *testing.T,
	catalog *Catalog,
	request SearchRequest,
) SearchResult {
	t.Helper()
	result, err := catalog.Search(context.Background(), request)
	if err != nil {
		t.Fatalf("search %+v: %v", request, err)
	}
	return result
}

// sequentialScanRows runs one statement with every index plan disabled.
func sequentialScanRows(
	t *testing.T,
	catalog *Catalog,
	statement string,
	arguments []any,
) int {
	t.Helper()
	count := 0
	queryWithPlanSettings(
		t,
		catalog,
		[]string{
			"SET LOCAL enable_indexscan = off",
			"SET LOCAL enable_bitmapscan = off",
			"SET LOCAL enable_indexonlyscan = off",
		},
		statement,
		arguments,
		func(rows pgx.Rows) {
			for rows.Next() {
				count++
			}
		},
	)
	return count
}

func searchBytes(t *testing.T, result SearchResult) []byte {
	t.Helper()
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("encode search result: %v", err)
	}
	return encoded
}

// A candidate generation that dies before publication must not change one byte
// of what a reader sees.
func TestKilledCandidateLeavesTheActiveResultUnchanged(t *testing.T) {
	catalog := newTestCatalog(t, testEndpoint(t, primaryEndpointEnv))
	mustInit(t, catalog)
	ctx := context.Background()
	active := publishedScan(t, catalog, nil, scanSessionFixture(
		"first",
		"ECONNRESET: socket hang up (errno=-54)",
		"protokol rukopozhatiya izmenilsya",
	))
	request := SearchRequest{
		Query: "ECONNRESET: socket hang up",
		Mode:  SearchModeLiteral,
		Limit: 5,
	}
	before := searchBytes(t, mustSearch(t, catalog, request))

	candidate, err := catalog.BeginCandidate(ctx, &active)
	if err != nil {
		t.Fatalf("begin the killed candidate: %v", err)
	}
	writer := catalog.NewDerivedWriter(fixtureBuilderKey)
	presentSession(t, catalog, writer, candidate, scanSessionFixture(
		"second",
		"ECONNRESET: socket hang up (errno=-54) in the candidate",
	))
	if err := catalog.MarkFailed(ctx, candidate); err != nil {
		t.Fatalf("mark the killed candidate failed: %v", err)
	}

	after := searchBytes(t, mustSearch(t, catalog, request))
	if !bytes.Equal(before, after) {
		t.Fatalf("public result changed after a killed scan:\n%s\n%s",
			before, after)
	}
	dropped, err := catalog.Reclaim(ctx)
	if err != nil {
		t.Fatalf("reclaim the killed candidate: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("reclaim dropped %d generations, want 1", dropped)
	}
	if reclaimed := searchBytes(t, mustSearch(t, catalog, request)); !bytes.Equal(
		before,
		reclaimed,
	) {
		t.Fatalf("public result changed after reclaim:\n%s\n%s",
			before, reclaimed)
	}
}

// The shared tables keep the superseded revisions until reclaim, and every one
// of them outscores the single passage the active generation presents. A limit
// applied before the membership predicate would return nothing but stale rows.
func TestGenerationMembershipRunsBeforeTheCandidateLimit(t *testing.T) {
	catalog := newTestCatalog(t, testEndpoint(t, primaryEndpointEnv))
	mustInit(t, catalog)
	stale := make([]string, 0, 600)
	for index := 0; index < 600; index++ {
		stale = append(stale, fmt.Sprintf(
			"%s potok %s potok %s nomer %d",
			adversarialTerm,
			adversarialTerm,
			adversarialTerm,
			index,
		))
	}
	first := publishedScan(t, catalog, nil, scanSessionFixture("stale", stale...))
	active := publishedScan(
		t,
		catalog,
		&first,
		scanSessionFixture("live", adversarialTerm+" odna zapis"),
	)
	if members := queryInt(t, catalog, fmt.Sprintf(
		"SELECT count(*) FROM %s.generation_member WHERE generation_id = $1",
		catalog.schema,
	), int64(first)); members != 1 {
		t.Fatalf("the superseded generation was already reclaimed: %d", members)
	}
	if kept := queryInt(t, catalog, fmt.Sprintf(
		"SELECT count(*) FROM %s",
		catalog.table(tableSearchDocument),
	)); kept != 601 {
		t.Fatalf("shared documents = %d, want the stale rows to still be there",
			kept)
	}
	request := SearchRequest{
		Query: adversarialTerm,
		Mode:  SearchModeLexical,
		Limit: 3,
	}
	result := mustSearch(t, catalog, request)
	if len(result.Hits) != 1 || result.Hits[0].SessionKey != "live" {
		t.Fatalf("hits = %+v, want only the passage the active generation"+
			" presents", result.Hits)
	}
	if result.Generation != active {
		t.Fatalf("answering generation = %d, want %d", result.Generation, active)
	}
	// The same answer must survive the index-backed plan, where the limit is
	// closest to the ranking operator.
	statement, arguments := catalog.searchStatement(active, request)
	forced := 0
	queryWithoutSequentialScans(
		t,
		catalog,
		statement,
		arguments,
		func(rows pgx.Rows) {
			for rows.Next() {
				forced++
			}
		},
	)
	if forced != 1 {
		t.Fatalf("forced index plan returned %d rows, want 1", forced)
	}
}

func TestSearchWithoutAnActiveGenerationIsTyped(t *testing.T) {
	catalog := newTestCatalog(t, testEndpoint(t, primaryEndpointEnv))
	mustInit(t, catalog)
	_, err := catalog.Search(context.Background(), SearchRequest{
		Query: "anything",
		Mode:  SearchModeLexical,
	})
	requireKind(t, err, KindCatalogGenerationIncomplete)
}

// A sequential scan scores every row, so BM25 is only a match test when the
// statement filters on the score.
func TestBM25RejectsNonMatchingDocumentsUnderASequentialScan(t *testing.T) {
	catalog := newTestCatalog(t, testEndpoint(t, primaryEndpointEnv))
	mustInit(t, catalog)
	generation := publishedScan(t, catalog, nil, scanSessionFixture(
		"tiny",
		"protokol rukopozhatiya izmenilsya",
		"sovsem drugaya tema pro sadovodstvo",
		"eshche odna nesvyazannaya zapis",
	))
	request := SearchRequest{Query: "protokol", Mode: SearchModeLexical, Limit: 10}
	statement, arguments := catalog.searchStatement(generation, request)
	// The same statement without the score filter is what a naive ORDER BY
	// implementation would run; it returns every row once the index is gone.
	naive := strings.Replace(
		statement,
		" AND (document.body <@> to_bm25query($2, $3)) < 0",
		"",
		1,
	)
	if naive == statement {
		t.Fatalf("the lexical statement carries no score filter:\n%s", statement)
	}
	if scored := sequentialScanRows(t, catalog, naive, arguments); scored != 3 {
		t.Fatalf("unfiltered sequential scan returned %d rows, want every row",
			scored)
	}
	if filtered := sequentialScanRows(
		t,
		catalog,
		statement,
		arguments,
	); filtered != 1 {
		t.Fatalf("filtered sequential scan returned %d rows, want 1", filtered)
	}
	result := mustSearch(t, catalog, request)
	if len(result.Hits) != 1 {
		t.Fatalf("hits = %d, want only the matching passage: %+v",
			len(result.Hits), result.Hits)
	}
	if result.Hits[0].MatchedLeg != LegBM25 {
		t.Fatalf("matched leg = %q, want %q", result.Hits[0].MatchedLeg, LegBM25)
	}
	if result.Hits[0].BM25Score == nil || *result.Hits[0].BM25Score >= 0 {
		t.Fatalf("bm25 score = %v, want a negative match score",
			result.Hits[0].BM25Score)
	}
	if !result.Complete {
		t.Fatal("a published generation reported incomplete")
	}
}

func TestLiteralLegKeepsExactContainment(t *testing.T) {
	catalog := newTestCatalog(t, testEndpoint(t, primaryEndpointEnv))
	mustInit(t, catalog)
	publishedScan(t, catalog, nil, scanSessionFixture(
		"literal",
		"ECONNRESET: socket hang up (errno=-54)",
		"econnreset: socket hang up in lower case",
		"ECONNRESET plus socket in another order",
	))
	cases := map[string]int{
		"ECONNRESET: socket hang up": 1,
		"econnreset: socket hang up": 1,
		"ECONNRESET%socket":          0,
		"ECONNRESET_ socket":         0,
	}
	for query, want := range cases {
		result := mustSearch(t, catalog, SearchRequest{
			Query: query,
			Mode:  SearchModeLiteral,
			Limit: 10,
		})
		if len(result.Hits) != want {
			t.Fatalf("literal %q matched %d passages, want %d",
				query, len(result.Hits), want)
		}
		for _, hit := range result.Hits {
			if !strings.Contains(hit.Body, query) {
				t.Fatalf("literal %q returned a passage without it: %q",
					query, hit.Body)
			}
			if hit.MatchedLeg != LegLiteral {
				t.Fatalf("matched leg = %q, want %q", hit.MatchedLeg, LegLiteral)
			}
		}
	}
}

// The trigram plan is reported only when PostgreSQL actually chooses it.
func TestLiteralPathReportsTheChosenPlan(t *testing.T) {
	catalog := newTestCatalog(t, testEndpoint(t, primaryEndpointEnv))
	mustInit(t, catalog)
	bodies := make([]string, 0, 4000)
	for index := 0; index < 4000; index++ {
		bodies = append(bodies, fmt.Sprintf(
			"zapis nomer %d bez osobogo soderzhaniya", index,
		))
	}
	bodies = append(bodies, "ECONNRESET: socket hang up (errno=-54)")
	publishedScan(t, catalog, nil, scanSessionFixture("large", bodies...))
	mustExec(t, catalog, "ANALYZE "+catalog.table(tableSearchDocument))
	accelerated := mustSearch(t, catalog, SearchRequest{
		Query: "ECONNRESET: socket hang up",
		Mode:  SearchModeLiteral,
		Limit: 5,
	})
	if accelerated.LiteralPath != LiteralPathTrigram {
		t.Fatalf("literal path = %q, want %q at %d rows",
			accelerated.LiteralPath, LiteralPathTrigram, len(bodies))
	}
	if len(accelerated.Hits) != 1 {
		t.Fatalf("accelerated hits = %d, want 1", len(accelerated.Hits))
	}
	short := mustSearch(t, catalog, SearchRequest{
		Query: "up",
		Mode:  SearchModeLiteral,
		Limit: 5,
	})
	if short.LiteralPath != LiteralPathScan {
		t.Fatalf("short literal path = %q, want %q",
			short.LiteralPath, LiteralPathScan)
	}
	if len(short.Hits) != 1 {
		t.Fatalf("short literal hits = %d, want the bounded scan result",
			len(short.Hits))
	}
}

// Every globally cheapest literal candidate sits outside the filter, so a limit
// applied before the filter would drop the only eligible passage.
func TestHardFilterRunsBeforeTheLiteralCandidateLimit(t *testing.T) {
	catalog, generation := newCandidateGeneration(t)
	writer := catalog.NewDerivedWriter(fixtureBuilderKey)
	excluded := scanSessionFixture("excluded")
	for index := 0; index < 500; index++ {
		body := fmt.Sprintf("ECONNRESET: socket hang up (errno=-54) %d", index)
		excluded.Events = append(excluded.Events, ScanEvent{
			Key:  fmt.Sprintf("excluded-event-%d", index),
			Kind: "message",
			Role: "user",
		})
		excluded.Passages = append(
			excluded.Passages,
			scanPassageFixture(index, body, excludedFilter()),
		)
	}
	presentSession(t, catalog, writer, generation, excluded)
	presentSession(t, catalog, writer, generation, scanSessionFixture(
		"eligible",
		"ECONNRESET: socket hang up (errno=-54) the only eligible passage",
	))
	publishGeneration(t, catalog, generation)

	filter := wantedFilter()
	result := mustSearch(t, catalog, SearchRequest{
		Query:  "ECONNRESET: socket hang up",
		Mode:   SearchModeLiteral,
		Filter: &filter,
		Limit:  3,
	})
	if len(result.Hits) != 1 {
		t.Fatalf("hits = %d, want only the eligible passage", len(result.Hits))
	}
	if result.Hits[0].SessionKey != "eligible" {
		t.Fatalf("session = %q, want the eligible session",
			result.Hits[0].SessionKey)
	}
}

func TestSearchHydratesProvenance(t *testing.T) {
	catalog := newTestCatalog(t, testEndpoint(t, primaryEndpointEnv))
	mustInit(t, catalog)
	publishedScan(t, catalog, nil, scanSessionFixture(
		"provenance",
		"ECONNRESET: socket hang up (errno=-54)",
	))
	result := mustSearch(t, catalog, SearchRequest{
		Query: "ECONNRESET",
		Mode:  SearchModeLiteral,
		Limit: 5,
	})
	if len(result.Hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(result.Hits))
	}
	hit := result.Hits[0]
	if hit.NativeID != "native-provenance" ||
		hit.SourceID != "source-provenance" ||
		hit.OccurrenceID != "occurrence-provenance" ||
		hit.DiscoveryRevision != "discovery-provenance" {
		t.Fatalf("hit provenance = %+v, want the retained identifiers", hit)
	}
	if hit.PassageBuilderVersion != fixtureBuilderVersion ||
		hit.ProjectionVersion != fixtureProjectionVersion ||
		hit.ProjectionKind != ProjectionKindLexical {
		t.Fatalf("hit versions = %+v, want the retained builder identities", hit)
	}
	if len(hit.EventKeys) != 1 || hit.EventKeys[0] != "provenance-event-0" {
		t.Fatalf("event keys = %v, want the retained event", hit.EventKeys)
	}
	if len(hit.Evidence) != 1 {
		t.Fatalf("evidence = %d, want 1", len(hit.Evidence))
	}
	if hit.Evidence[0].Locator.Path != "provenance.jsonl" ||
		hit.Evidence[0].Locator.Record == nil ||
		*hit.Evidence[0].Locator.Record != 1 {
		t.Fatalf("evidence locator = %+v, want the retained record",
			hit.Evidence[0].Locator)
	}
}

// A projection that lost NUL bytes must be identifiable from the hit alone.
func TestSearchReportsProjectionLimitations(t *testing.T) {
	catalog := newTestCatalog(t, testEndpoint(t, primaryEndpointEnv))
	mustInit(t, catalog)
	session := scanSessionFixture(
		"limited",
		"checksum mismatch in chunk 7\x07 retry scheduled",
		"byte exact tool output",
	)
	session.Passages[0].Limitations = []ProjectionLimitation{{
		Kind:         "nul_removed",
		RemovedBytes: 2,
	}}
	publishedScan(t, catalog, nil, session)
	result := mustSearch(t, catalog, SearchRequest{
		Query: "chunk 7\x07 retry",
		Mode:  SearchModeLiteral,
		Limit: 5,
	})
	if len(result.Hits) != 1 {
		t.Fatalf("hits = %d, want the limited passage", len(result.Hits))
	}
	want := []ProjectionLimitation{{Kind: "nul_removed", RemovedBytes: 2}}
	if !reflect.DeepEqual(result.Hits[0].Limitations, want) {
		t.Fatalf("limitations = %+v, want %+v", result.Hits[0].Limitations, want)
	}
	exact := mustSearch(t, catalog, SearchRequest{
		Query: "byte exact tool output",
		Mode:  SearchModeLiteral,
		Limit: 5,
	})
	if len(exact.Hits) != 1 {
		t.Fatalf("hits = %d, want the byte-exact passage", len(exact.Hits))
	}
	if len(exact.Hits[0].Limitations) != 0 {
		t.Fatalf("limitations = %+v, want none", exact.Hits[0].Limitations)
	}
}

func TestSearchRejectsAnEmptyQueryAndAnUnknownMode(t *testing.T) {
	catalog := newTestCatalog(t, testEndpoint(t, primaryEndpointEnv))
	mustInit(t, catalog)
	publishedScan(t, catalog, nil, scanSessionFixture("modes", "some body"))
	ctx := context.Background()
	_, err := catalog.Search(ctx, SearchRequest{
		Query: "   ",
		Mode:  SearchModeLexical,
	})
	requireKind(t, err, KindSearchRequestInvalid)
	_, err = catalog.Search(ctx, SearchRequest{
		Query: "body",
		Mode:  SearchMode("fuzzy"),
	})
	requireKind(t, err, KindSearchRequestInvalid)
}

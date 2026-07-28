//go:build pgintegration

package catalog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// contentWord keeps fixtures away from stopwords the russian text
// configuration legitimately drops.
const (
	adversarialTerm = "replikaciya"
	eligibleValue   = "wanted"
	excludedValue   = "excluded"
)

func wantedFilter() FacetFilter {
	return FacetFilter{Namespace: "user", Key: "project", Value: eligibleValue}
}

func excludedFilter() FacetFilter {
	return FacetFilter{Namespace: "user", Key: "project", Value: excludedValue}
}

func addFacetedDocument(
	t *testing.T,
	catalog *Catalog,
	derived int64,
	body string,
	facet FacetFilter,
) int64 {
	t.Helper()
	ctx := context.Background()
	docID, err := catalog.AddDocument(ctx, derived, Document{
		SessionRef:  fmt.Sprintf("session-%s-%d", facet.Value, derived),
		Harness:     "claude",
		Body:        body,
		ContentHash: []byte(body),
	})
	if err != nil {
		t.Fatalf("add document: %v", err)
	}
	if err := catalog.AddFacet(ctx, derived, docID, facet); err != nil {
		t.Fatalf("add facet: %v", err)
	}
	return docID
}

func publishGeneration(
	t *testing.T,
	catalog *Catalog,
	generation GenerationID,
) {
	t.Helper()
	ctx := context.Background()
	if err := catalog.MaintainIndexes(ctx, generation, true); err != nil {
		t.Fatalf("build indexes: %v", err)
	}
	if err := catalog.Publish(ctx, generation); err != nil {
		t.Fatalf("publish generation: %v", err)
	}
}

func candidateWithDocument(
	t *testing.T,
	catalog *Catalog,
	parent *GenerationID,
	body string,
) (GenerationID, int64) {
	t.Helper()
	generation, err := catalog.BeginCandidate(context.Background(), parent)
	if err != nil {
		t.Fatalf("begin candidate generation: %v", err)
	}
	derived := newSubstrateSession(t, catalog, generation)
	docID := addFacetedDocument(t, catalog, derived, body, wantedFilter())
	return generation, docID
}

func publishedGeneration(
	t *testing.T,
	catalog *Catalog,
	parent *GenerationID,
	body string,
) (GenerationID, int64) {
	t.Helper()
	generation, docID := candidateWithDocument(t, catalog, parent, body)
	publishGeneration(t, catalog, generation)
	return generation, docID
}

// The globally best BM25 match sits outside the hard filter, so a candidate
// limit applied before the filter would lose the eligible document.
func TestBM25FilterRunsBeforeTheCandidateLimit(t *testing.T) {
	catalog, generation := newCandidateGeneration(t)
	ctx := context.Background()
	derived := newSubstrateSession(t, catalog, generation)
	const excludedDocuments = 600
	best := int64(0)
	for index := 0; index < excludedDocuments; index++ {
		body := fmt.Sprintf(
			"%s potok %s potok %s nomer %d",
			adversarialTerm,
			adversarialTerm,
			adversarialTerm,
			index,
		)
		docID := addFacetedDocument(
			t,
			catalog,
			derived,
			body,
			excludedFilter(),
		)
		if index == 0 {
			best = docID
		}
	}
	eligible := addFacetedDocument(
		t,
		catalog,
		derived,
		adversarialTerm+" nastroyka odna zapis",
		wantedFilter(),
	)
	for index := 0; index < 100; index++ {
		addFacetedDocument(
			t,
			catalog,
			derived,
			fmt.Sprintf("shum bez temy nomer %d", index),
			wantedFilter(),
		)
	}
	if err := catalog.MaintainIndexes(ctx, generation, true); err != nil {
		t.Fatalf("build indexes: %v", err)
	}
	mustExec(t, catalog, "ANALYZE "+catalog.table(tableSearchDocument))
	query := RankQuery{Mode: RankBM25, Text: adversarialTerm, Limit: 3}
	result, err := catalog.EligibleThenRank(
		ctx,
		generation,
		wantedFilter(),
		query,
	)
	if err != nil {
		t.Fatalf("rank documents: %v", err)
	}
	assertEligibleTop(t, result, eligible, best)
	statement, arguments, err := catalog.rankStatement(
		generation,
		wantedFilter(),
		query,
	)
	if err != nil {
		t.Fatalf("build rank statement: %v", err)
	}
	plan := explain(t, catalog, statement, arguments...)
	if !strings.Contains(plan, bm25IndexName) {
		t.Fatalf("forced plan did not reach the bm25 index:\n%s", plan)
	}
	forced := rankWithoutSequentialScans(t, catalog, statement, arguments)
	assertEligibleTop(t, forced, eligible, best)
}

func assertEligibleTop(
	t *testing.T,
	result RankResult,
	eligible int64,
	excluded int64,
) {
	t.Helper()
	if len(result.Documents) == 0 {
		t.Fatalf("ranking returned no document, want the eligible one")
	}
	if result.Documents[0].DocID != eligible {
		t.Fatalf(
			"top document = %d, want the in-filter document %d",
			result.Documents[0].DocID,
			eligible,
		)
	}
	for _, document := range result.Documents {
		if document.DocID == excluded {
			t.Fatalf("ranking returned the filtered-out document %d", excluded)
		}
	}
}

func rankWithoutSequentialScans(
	t *testing.T,
	catalog *Catalog,
	statement string,
	arguments []any,
) RankResult {
	t.Helper()
	var result RankResult
	queryWithoutSequentialScans(
		t,
		catalog,
		statement,
		arguments,
		func(rows pgx.Rows) {
			for rows.Next() {
				var document RankedDocument
				if err := rows.Scan(
					&document.DocID,
					&document.SessionRef,
					&document.Score,
				); err != nil {
					t.Fatalf("read forced ranking row: %v", err)
				}
				result.Documents = append(result.Documents, document)
			}
		},
	)
	return result
}

func TestVectorFilterRunsBeforeTheCandidateLimitAndCandidatesStayInvisible(
	t *testing.T,
) {
	catalog := newTestCatalog(t, testEndpoint(t, primaryEndpointEnv))
	mustInit(t, catalog)
	ctx := context.Background()
	space, err := catalog.EnsureEmbeddingSpace(ctx, EmbeddingSpace{
		Name:       "fixture_small",
		Provider:   "fixture",
		Model:      "fixture-4",
		Dimensions: 4,
		Distance:   "cosine",
	})
	if err != nil {
		t.Fatalf("ensure embedding space: %v", err)
	}
	active, activeDoc := publishedGeneration(
		t,
		catalog,
		nil,
		adversarialTerm+" aktivnoe pokolenie",
	)
	candidate, err := catalog.BeginCandidate(ctx, &active)
	if err != nil {
		t.Fatalf("begin candidate generation: %v", err)
	}
	derived := newSubstrateSession(t, catalog, candidate)
	nearest := addFacetedDocument(
		t,
		catalog,
		derived,
		"blizhayshiy vektor vne filtra",
		excludedFilter(),
	)
	attach(t, catalog, nearest, space, []float32{1, 0, 0, 0})
	eligible := addFacetedDocument(
		t,
		catalog,
		derived,
		"dopustimaya zapis vnutri filtra",
		wantedFilter(),
	)
	attach(t, catalog, eligible, space, []float32{0.8, 0.2, 0, 0})
	distant := addFacetedDocument(
		t,
		catalog,
		derived,
		"dalekaya zapis vnutri filtra",
		wantedFilter(),
	)
	attach(t, catalog, distant, space, []float32{0, 0, 1, 0})

	result, err := catalog.EligibleThenRank(ctx, candidate, wantedFilter(), RankQuery{
		Mode:       RankVector,
		SpaceID:    space,
		Dimensions: 4,
		Vector:     []float32{1, 0, 0, 0},
		Limit:      2,
	})
	if err != nil {
		t.Fatalf("rank candidate vectors: %v", err)
	}
	assertEligibleTop(t, result, eligible, nearest)

	// A reader of the active generation never sees candidate rows: they share
	// one table, so only the membership predicate separates them.
	current, present, err := catalog.ActiveGeneration(ctx)
	if err != nil {
		t.Fatalf("read the active generation: %v", err)
	}
	if !present || current != active {
		t.Fatalf("active generation = %d (present=%t), want %d",
			current, present, active)
	}
	reader, err := catalog.EligibleThenRank(ctx, current, wantedFilter(), RankQuery{
		Mode:  RankBM25,
		Text:  adversarialTerm,
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("read the active generation: %v", err)
	}
	if !strings.Contains(reader.SQL, "generation_member") {
		t.Fatalf("reader SQL carries no membership predicate:\n%s", reader.SQL)
	}
	if len(reader.Documents) != 1 || reader.Documents[0].DocID != activeDoc {
		t.Fatalf("reader documents = %+v, want only %d",
			reader.Documents, activeDoc)
	}
	if visible := presentedDocuments(t, catalog, candidate); visible != 3 {
		t.Fatalf("candidate documents = %d, want the 3 it wrote", visible)
	}
}

func attach(
	t *testing.T,
	catalog *Catalog,
	docID int64,
	space int64,
	vector []float32,
) AttachResult {
	t.Helper()
	result, err := catalog.AttachEmbedding(
		context.Background(),
		docID,
		space,
		[]byte(fmt.Sprintf("hash-%d-%v", space, vector)),
		vector,
	)
	if err != nil {
		t.Fatalf("attach embedding: %v", err)
	}
	return result
}

func TestPerSpacePartialHNSWIndexesBuildAndServeTheirSpace(t *testing.T) {
	catalog := newTestCatalog(t, testEndpoint(t, primaryEndpointEnv))
	mustInit(t, catalog)
	ctx := context.Background()
	small, err := catalog.EnsureEmbeddingSpace(ctx, EmbeddingSpace{
		Name:       "fixture_four",
		Provider:   "fixture",
		Model:      "fixture-4",
		Dimensions: 4,
		Distance:   "cosine",
	})
	if err != nil {
		t.Fatalf("ensure four-dimensional space: %v", err)
	}
	large, err := catalog.EnsureEmbeddingSpace(ctx, EmbeddingSpace{
		Name:       "fixture_eight",
		Provider:   "fixture",
		Model:      "fixture-8",
		Dimensions: 8,
		Distance:   "cosine",
	})
	if err != nil {
		t.Fatalf("ensure eight-dimensional space: %v", err)
	}
	generation, err := catalog.BeginCandidate(ctx, nil)
	if err != nil {
		t.Fatalf("begin candidate: %v", err)
	}
	derived := newSubstrateSession(t, catalog, generation)
	for index := 0; index < 40; index++ {
		docID := addFacetedDocument(
			t,
			catalog,
			derived,
			fmt.Sprintf("zapis chetyre nomer %d", index),
			wantedFilter(),
		)
		attach(t, catalog, docID, small, []float32{
			float32(index%7) / 7, float32(index%5) / 5, 0.1, 0.2,
		})
	}
	for index := 0; index < 40; index++ {
		docID := addFacetedDocument(
			t,
			catalog,
			derived,
			fmt.Sprintf("zapis vosem nomer %d", index),
			wantedFilter(),
		)
		attach(t, catalog, docID, large, []float32{
			float32(index%3) / 3, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8,
		})
	}
	if err := catalog.MaintainIndexes(ctx, generation, true); err != nil {
		t.Fatalf("build indexes: %v", err)
	}
	for _, space := range []int64{small, large} {
		if queryInt(t, catalog,
			"SELECT count(*) FROM pg_indexes WHERE schemaname = $1"+
				" AND indexname = $2",
			catalog.settings.SchemaName,
			vectorIndexName(space),
		) != 1 {
			t.Fatalf("space %d has no partial HNSW index", space)
		}
	}
	plan := explain(t, catalog, fmt.Sprintf(
		"SELECT doc_id FROM %s WHERE embedding_space_id = %d"+
			" ORDER BY (embedding::vector(4)) <=> $1::vector(4) LIMIT 1",
		catalog.table(tableSearchDocument),
		small,
	), vectorLiteral([]float32{1, 0, 0, 0}))
	if !strings.Contains(plan, vectorIndexName(small)) {
		t.Fatalf("plan did not use the four-dimensional index:\n%s", plan)
	}
	if strings.Contains(plan, vectorIndexName(large)) {
		t.Fatalf("plan used the eight-dimensional index:\n%s", plan)
	}
}

func TestInterruptedPublicationKeepsTheActiveGeneration(t *testing.T) {
	catalog := newTestCatalog(t, testEndpoint(t, primaryEndpointEnv))
	mustInit(t, catalog)
	background := context.Background()
	first, _ := publishedGeneration(t, catalog, nil, adversarialTerm+" pervoe")
	second, _ := candidateWithDocument(
		t,
		catalog,
		&first,
		adversarialTerm+" vtoroe",
	)
	if err := catalog.MaintainIndexes(background, second, true); err != nil {
		t.Fatalf("build indexes: %v", err)
	}
	ctx, cancel := context.WithCancel(background)
	defer cancel()
	catalog.publishHook = func(context.Context) error {
		cancel()
		return ctx.Err()
	}
	err := catalog.Publish(ctx, second)
	catalog.publishHook = nil
	if err == nil {
		t.Fatal("interrupted publication reported success")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("publication error = %v, want a cancellation", err)
	}
	active, present, err := catalog.ActiveGeneration(background)
	if err != nil {
		t.Fatalf("read the active generation: %v", err)
	}
	if !present || active != first {
		t.Fatalf("active generation = %d (present=%t), want %d",
			active, present, first)
	}
	state, err := catalog.GenerationState(background, second)
	if err != nil {
		t.Fatalf("read the interrupted generation state: %v", err)
	}
	if state != StateBuilding {
		t.Fatalf("interrupted generation state = %q, want %q",
			state, StateBuilding)
	}
	firstState, err := catalog.GenerationState(background, first)
	if err != nil {
		t.Fatalf("read the published generation state: %v", err)
	}
	if firstState != StateComplete {
		t.Fatalf("published generation state = %q, want %q",
			firstState, StateComplete)
	}
}

func TestFailedIndexBuildCannotPublish(t *testing.T) {
	catalog, first, second, secondDoc := twoGenerations(t)
	ctx := context.Background()
	space, err := catalog.EnsureEmbeddingSpace(ctx, EmbeddingSpace{
		Name:       "fixture_failed",
		Provider:   "fixture",
		Model:      "fixture-4",
		Dimensions: 4,
		Distance:   "cosine",
	})
	if err != nil {
		t.Fatalf("ensure embedding space: %v", err)
	}
	attach(t, catalog, secondDoc, space, []float32{1, 0, 0, 0})
	// A row whose stored vector does not fit the space's declared dimensions
	// fails the cast the partial index builds on, so the build this generation
	// still owes cannot finish.
	mustExec(t, catalog, fmt.Sprintf(
		"UPDATE %s SET embedding = '[1,2,3]'::vector WHERE doc_id = $1",
		catalog.table(tableSearchDocument),
	), secondDoc)
	if err := catalog.MaintainIndexes(ctx, second, true); err == nil {
		t.Fatal("index build succeeded with a conflicting index name")
	}
	state, err := catalog.GenerationState(ctx, second)
	if err != nil {
		t.Fatalf("read the failed generation state: %v", err)
	}
	if state != StateFailed {
		t.Fatalf("generation state = %q, want %q", state, StateFailed)
	}
	if err := catalog.Publish(ctx, second); err == nil {
		t.Fatal("a failed generation was published")
	}
	requireActive(t, catalog, first)
}

// The shared retrieval indexes are what every generation is published against,
// so a missing one must block publication instead of demoting search silently.
func TestPublicationRequiresTheSharedRetrievalIndexes(t *testing.T) {
	catalog, first, second, _ := twoGenerations(t)
	ctx := context.Background()
	mustExec(t, catalog, fmt.Sprintf(
		"DROP INDEX %s.%s",
		catalog.schema,
		quoteIdentifier(bm25IndexName),
	))
	if err := catalog.Publish(ctx, second); err == nil {
		t.Fatal("a generation published without the bm25 index")
	}
	requireActive(t, catalog, first)
}

// twoGenerations publishes one generation and leaves a second one building.
func twoGenerations(t *testing.T) (*Catalog, GenerationID, GenerationID, int64) {
	t.Helper()
	catalog := newTestCatalog(t, testEndpoint(t, primaryEndpointEnv))
	mustInit(t, catalog)
	first, _ := publishedGeneration(t, catalog, nil, adversarialTerm+" pervoe")
	second, docID := candidateWithDocument(
		t,
		catalog,
		&first,
		adversarialTerm+" vtoroe",
	)
	return catalog, first, second, docID
}

func requireActive(t *testing.T, catalog *Catalog, want GenerationID) {
	t.Helper()
	active, present, err := catalog.ActiveGeneration(context.Background())
	if err != nil {
		t.Fatalf("read the active generation: %v", err)
	}
	if !present || active != want {
		t.Fatalf("active generation = %d (present=%t), want %d",
			active, present, want)
	}
}

func TestEmbeddingCacheIsReusedAcrossGenerations(t *testing.T) {
	catalog := newTestCatalog(t, testEndpoint(t, primaryEndpointEnv))
	mustInit(t, catalog)
	ctx := context.Background()
	space, err := catalog.EnsureEmbeddingSpace(ctx, EmbeddingSpace{
		Name:       "fixture_reuse",
		Provider:   "fixture",
		Model:      "fixture-4",
		Dimensions: 4,
		Distance:   "cosine",
	})
	if err != nil {
		t.Fatalf("ensure embedding space: %v", err)
	}
	hash := []byte("shared-content-hash")
	vector := []float32{0.1, 0.2, 0.3, 0.4}
	first, firstDoc := candidateWithDocument(
		t,
		catalog,
		nil,
		adversarialTerm+" povtor",
	)
	initial, err := catalog.AttachEmbedding(ctx, firstDoc, space, hash, vector)
	if err != nil {
		t.Fatalf("attach the first embedding: %v", err)
	}
	if initial.Reused {
		t.Fatal("the first attach reported reuse")
	}
	publishGeneration(t, catalog, first)

	second, secondDoc := candidateWithDocument(
		t,
		catalog,
		&first,
		adversarialTerm+" povtor",
	)
	reused, err := catalog.AttachEmbedding(ctx, secondDoc, space, hash, nil)
	if err != nil {
		t.Fatalf("attach the reused embedding: %v", err)
	}
	if !reused.Reused {
		t.Fatal("the second attach did not report reuse")
	}
	if count := queryInt(t, catalog, fmt.Sprintf(
		"SELECT count(*) FROM %s.embedding_cache"+
			" WHERE space_id = $1 AND content_hash = $2",
		catalog.schema,
	), space, hash); count != 1 {
		t.Fatalf("embedding cache rows = %d, want 1", count)
	}
	if stored := queryInt(t, catalog, fmt.Sprintf(
		"SELECT count(*) FROM %s document"+
			" JOIN %s.generation_member member"+
			" ON member.derived_id = document.derived_id"+
			" WHERE member.generation_id = $1 AND document.embedding IS NOT NULL",
		catalog.table(tableSearchDocument),
		catalog.schema,
	), int64(second)); stored != 1 {
		t.Fatalf("documents with an embedding = %d, want 1", stored)
	}
}

// Cleanup deletes rows instead of dropping tables, so a live reader keeps the
// MVCC snapshot it started with and no row a live generation presents is lost.
func TestCleanupRefusesTheActiveGenerationAndKeepsWhatAReaderHolds(t *testing.T) {
	dsn := testEndpoint(t, primaryEndpointEnv)
	catalog := newTestCatalog(t, dsn)
	mustInit(t, catalog)
	ctx := context.Background()
	first, firstDoc := publishedGeneration(
		t,
		catalog,
		nil,
		adversarialTerm+" pervoe",
	)
	second, _ := publishedGeneration(
		t,
		catalog,
		&first,
		adversarialTerm+" vtoroe",
	)
	if err := catalog.Cleanup(ctx, second); err == nil {
		t.Fatal("cleanup removed the active generation")
	}
	state, err := catalog.GenerationState(ctx, first)
	if err != nil {
		t.Fatalf("read the superseded generation state: %v", err)
	}
	if state != StateSuperseded {
		t.Fatalf("previous generation state = %q, want %q",
			state, StateSuperseded)
	}

	reader, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect the reader: %v", err)
	}
	defer closeConnection(t, reader)
	transaction, err := reader.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		t.Fatalf("begin the reader transaction: %v", err)
	}
	count := func() int64 {
		var visible int64
		if err := transaction.QueryRow(ctx, fmt.Sprintf(
			"SELECT count(*) FROM %s WHERE doc_id = $1",
			catalog.table(tableSearchDocument),
		), firstDoc).Scan(&visible); err != nil {
			t.Fatalf("read the superseded generation: %v", err)
		}
		return visible
	}
	if before := count(); before != 1 {
		t.Fatalf("reader saw %d rows before cleanup, want 1", before)
	}
	if err := catalog.Cleanup(ctx, first); err != nil {
		t.Fatalf("cleanup while a reader holds the generation: %v", err)
	}
	if after := count(); after != 1 {
		t.Fatalf("cleanup removed a row the reader holds: %d rows left", after)
	}
	if err := transaction.Commit(ctx); err != nil {
		t.Fatalf("commit the reader transaction: %v", err)
	}
	if remaining := documentRows(t, catalog, firstDoc); remaining != 0 {
		t.Fatalf("cleanup left %d reclaimed documents", remaining)
	}
	if members := queryInt(t, catalog, fmt.Sprintf(
		"SELECT count(*) FROM %s.generation_member WHERE generation_id = $1",
		catalog.schema,
	), int64(first)); members != 0 {
		t.Fatalf("cleanup left %d memberships", members)
	}
	// The generation still active keeps every row it presents.
	if kept := presentedDocuments(t, catalog, second); kept != 1 {
		t.Fatalf("the active generation lost rows: %d left", kept)
	}
}

// documentRows counts one projection row regardless of any membership.
func documentRows(t *testing.T, catalog *Catalog, docID int64) int64 {
	t.Helper()
	return queryInt(t, catalog, fmt.Sprintf(
		"SELECT count(*) FROM %s WHERE doc_id = $1",
		catalog.table(tableSearchDocument),
	), docID)
}

// presentedDocuments counts the projections one generation presents.
func presentedDocuments(
	t *testing.T,
	catalog *Catalog,
	generation GenerationID,
) int64 {
	t.Helper()
	return queryInt(t, catalog, fmt.Sprintf(
		"SELECT count(*) FROM %s document"+
			" JOIN %s.generation_member member"+
			" ON member.derived_id = document.derived_id"+
			" WHERE member.generation_id = $1",
		catalog.table(tableSearchDocument),
		catalog.schema,
	), int64(generation))
}

// Two generations that present the same derived session share its rows, so
// reclaiming one must not remove what the other still references.
func TestCleanupKeepsRowsASecondGenerationStillPresents(t *testing.T) {
	catalog := newTestCatalog(t, testEndpoint(t, primaryEndpointEnv))
	mustInit(t, catalog)
	ctx := context.Background()
	first, err := catalog.BeginCandidate(ctx, nil)
	if err != nil {
		t.Fatalf("begin the first generation: %v", err)
	}
	derived := newSubstrateSession(t, catalog, first)
	docID := addFacetedDocument(
		t,
		catalog,
		derived,
		adversarialTerm+" obshchaya zapis",
		wantedFilter(),
	)
	publishGeneration(t, catalog, first)
	second, err := catalog.BeginCandidate(ctx, &first)
	if err != nil {
		t.Fatalf("begin the second generation: %v", err)
	}
	if err := catalog.AddGenerationMember(ctx, second, derived); err != nil {
		t.Fatalf("present the shared session again: %v", err)
	}
	publishGeneration(t, catalog, second)
	dropped, err := catalog.Reclaim(ctx)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("reclaimed %d generations, want 1", dropped)
	}
	if kept := documentRows(t, catalog, docID); kept != 1 {
		t.Fatalf("reclaim removed a row the active generation presents")
	}
	if members := queryInt(t, catalog, fmt.Sprintf(
		"SELECT count(*) FROM %s.generation_member WHERE derived_id = $1",
		catalog.schema,
	), derived); members != 1 {
		t.Fatalf("memberships of the shared session = %d, want 1", members)
	}
}

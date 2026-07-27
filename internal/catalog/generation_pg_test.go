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
	generation GenerationID,
	body string,
	facet FacetFilter,
) int64 {
	t.Helper()
	ctx := context.Background()
	docID, err := catalog.AddDocument(ctx, generation, Document{
		SessionRef:  fmt.Sprintf("session-%s-%d", facet.Value, generation),
		Harness:     "claude",
		Body:        body,
		ContentHash: []byte(body),
	})
	if err != nil {
		t.Fatalf("add document: %v", err)
	}
	if err := catalog.AddFacet(ctx, generation, docID, facet); err != nil {
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
	if err := catalog.BuildIndexes(ctx, generation); err != nil {
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
	docID := addFacetedDocument(t, catalog, generation, body, wantedFilter())
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
			generation,
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
		generation,
		adversarialTerm+" nastroyka odna zapis",
		wantedFilter(),
	)
	for index := 0; index < 100; index++ {
		addFacetedDocument(
			t,
			catalog,
			generation,
			fmt.Sprintf("shum bez temy nomer %d", index),
			wantedFilter(),
		)
	}
	if err := catalog.BuildIndexes(ctx, generation); err != nil {
		t.Fatalf("build indexes: %v", err)
	}
	mustExec(t, catalog, fmt.Sprintf(
		"ANALYZE %s.%s",
		catalog.schema,
		quoteIdentifier(documentTable(generation)),
	))
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
	if !strings.Contains(plan, bm25IndexName(generation)) {
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
	nearest := addFacetedDocument(
		t,
		catalog,
		candidate,
		"blizhayshiy vektor vne filtra",
		excludedFilter(),
	)
	attach(t, catalog, candidate, nearest, space, []float32{1, 0, 0, 0})
	eligible := addFacetedDocument(
		t,
		catalog,
		candidate,
		"dopustimaya zapis vnutri filtra",
		wantedFilter(),
	)
	attach(t, catalog, candidate, eligible, space, []float32{0.8, 0.2, 0, 0})
	distant := addFacetedDocument(
		t,
		catalog,
		candidate,
		"dalekaya zapis vnutri filtra",
		wantedFilter(),
	)
	attach(t, catalog, candidate, distant, space, []float32{0, 0, 1, 0})

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

	// A reader of the active generation never sees candidate rows.
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
	if !strings.Contains(reader.SQL, documentTable(active)) {
		t.Fatalf("reader SQL does not name the active generation:\n%s", reader.SQL)
	}
	if strings.Contains(reader.SQL, documentTable(candidate)) {
		t.Fatalf("reader SQL names the candidate generation:\n%s", reader.SQL)
	}
	if len(reader.Documents) != 1 || reader.Documents[0].DocID != activeDoc {
		t.Fatalf("reader documents = %+v, want only %d",
			reader.Documents, activeDoc)
	}
}

func attach(
	t *testing.T,
	catalog *Catalog,
	generation GenerationID,
	docID int64,
	space int64,
	vector []float32,
) AttachResult {
	t.Helper()
	result, err := catalog.AttachEmbedding(
		context.Background(),
		generation,
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
	for index := 0; index < 40; index++ {
		docID := addFacetedDocument(
			t,
			catalog,
			generation,
			fmt.Sprintf("zapis chetyre nomer %d", index),
			wantedFilter(),
		)
		attach(t, catalog, generation, docID, small, []float32{
			float32(index%7) / 7, float32(index%5) / 5, 0.1, 0.2,
		})
	}
	for index := 0; index < 40; index++ {
		docID := addFacetedDocument(
			t,
			catalog,
			generation,
			fmt.Sprintf("zapis vosem nomer %d", index),
			wantedFilter(),
		)
		attach(t, catalog, generation, docID, large, []float32{
			float32(index%3) / 3, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8,
		})
	}
	if err := catalog.BuildIndexes(ctx, generation); err != nil {
		t.Fatalf("build indexes: %v", err)
	}
	for _, space := range []int64{small, large} {
		if queryInt(t, catalog,
			"SELECT count(*) FROM pg_indexes WHERE schemaname = $1"+
				" AND indexname = $2",
			catalog.settings.SchemaName,
			vectorIndexName(generation, space),
		) != 1 {
			t.Fatalf("space %d has no partial HNSW index", space)
		}
	}
	plan := explain(t, catalog, fmt.Sprintf(
		"SELECT doc_id FROM %s.%s WHERE embedding_space_id = %d"+
			" ORDER BY (embedding::vector(4)) <=> $1::vector(4) LIMIT 1",
		catalog.schema,
		quoteIdentifier(documentTable(generation)),
		small,
	), vectorLiteral([]float32{1, 0, 0, 0}))
	if !strings.Contains(plan, vectorIndexName(generation, small)) {
		t.Fatalf("plan did not use the four-dimensional index:\n%s", plan)
	}
	if strings.Contains(plan, vectorIndexName(generation, large)) {
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
	if err := catalog.BuildIndexes(background, second); err != nil {
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
	catalog := newTestCatalog(t, testEndpoint(t, primaryEndpointEnv))
	mustInit(t, catalog)
	ctx := context.Background()
	first, _ := publishedGeneration(t, catalog, nil, adversarialTerm+" pervoe")
	second, _ := candidateWithDocument(
		t,
		catalog,
		&first,
		adversarialTerm+" vtoroe",
	)
	// A btree index steals the bm25 index name; a second bm25 index on the
	// same column is rejected by the pg_textsearch planner hook instead.
	mustExec(t, catalog, fmt.Sprintf(
		"CREATE INDEX %s ON %s.generation (id)",
		quoteIdentifier(bm25IndexName(second)),
		catalog.schema,
	))
	if err := catalog.BuildIndexes(ctx, second); err == nil {
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
	active, present, err := catalog.ActiveGeneration(ctx)
	if err != nil {
		t.Fatalf("read the active generation: %v", err)
	}
	if !present || active != first {
		t.Fatalf("active generation = %d (present=%t), want %d",
			active, present, first)
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
	initial, err := catalog.AttachEmbedding(
		ctx, first, firstDoc, space, hash, vector,
	)
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
	reused, err := catalog.AttachEmbedding(
		ctx, second, secondDoc, space, hash, nil,
	)
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
		"SELECT count(*) FROM %s.%s WHERE embedding IS NOT NULL",
		catalog.schema,
		quoteIdentifier(documentTable(second)),
	)); stored != 1 {
		t.Fatalf("documents with embeddings = %d, want 1", stored)
	}
}

func TestCleanupRefusesTheActiveGenerationAndBoundsTheReaderWait(t *testing.T) {
	dsn := testEndpoint(t, primaryEndpointEnv)
	catalog := newTestCatalog(t, dsn)
	mustInit(t, catalog)
	ctx := context.Background()
	first, _ := publishedGeneration(t, catalog, nil, adversarialTerm+" pervoe")
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
	var visible int64
	if err := transaction.QueryRow(ctx, fmt.Sprintf(
		"SELECT count(*) FROM %s.%s",
		catalog.schema,
		quoteIdentifier(documentTable(first)),
	)).Scan(&visible); err != nil {
		t.Fatalf("read the superseded generation: %v", err)
	}
	if err := catalog.Cleanup(ctx, first); !errors.Is(err, ErrCleanupBusy) {
		t.Fatalf("cleanup error = %v, want ErrCleanupBusy", err)
	}
	if err := transaction.Commit(ctx); err != nil {
		t.Fatalf("commit the reader transaction: %v", err)
	}
	if err := catalog.Cleanup(ctx, first); err != nil {
		t.Fatalf("cleanup after the reader finished: %v", err)
	}
	if remaining := queryInt(t, catalog,
		"SELECT count(*) FROM pg_tables WHERE schemaname = $1"+
			" AND tablename = ANY($2)",
		catalog.settings.SchemaName,
		[]string{documentTable(first), facetTable(first)},
	); remaining != 0 {
		t.Fatalf("cleanup left %d generation tables", remaining)
	}
}

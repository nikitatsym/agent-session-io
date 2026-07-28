package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// GenerationID identifies one catalog generation.
type GenerationID int64

const (
	StateBuilding   = "building"
	StateComplete   = "complete"
	StatePartial    = "partial"
	StateFailed     = "failed"
	StateSuperseded = "superseded"
)

type Document struct {
	SessionRef  string
	Harness     string
	Body        string
	ContentHash []byte
}

type EmbeddingSpace struct {
	Name       string
	Provider   string
	Model      string
	Dimensions int
	Distance   string
}

type AttachResult struct {
	Reused bool
}

type FacetFilter struct {
	Namespace string
	Key       string
	Value     string
}

type RankMode string

const (
	RankBM25   RankMode = "bm25"
	RankVector RankMode = "vector"
)

type RankQuery struct {
	Mode       RankMode
	Text       string
	SpaceID    int64
	Dimensions int
	Vector     []float32
	Limit      int
}

type RankedDocument struct {
	DocID      int64
	SessionRef string
	Score      float64
}

// RankResult carries the executed SQL so tests can prove generation isolation.
type RankResult struct {
	SQL       string
	Documents []RankedDocument
}

// BeginCandidate opens a candidate generation. It runs no DDL: a generation is
// a membership set over the shared derived tables.
func (catalog *Catalog) BeginCandidate(
	ctx context.Context,
	parent *GenerationID,
) (generation GenerationID, err error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return 0, err
	}
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		"INSERT INTO %s.generation (state, parent_id) VALUES ($1, $2)"+
			" RETURNING id",
		catalog.schema,
	), StateBuilding, parent).Scan(&generation); err != nil {
		return 0, fmt.Errorf("record candidate generation: %w", err)
	}
	return generation, nil
}

// AddDocument writes one projection row for an existing derived session.
func (catalog *Catalog) AddDocument(
	ctx context.Context,
	derived int64,
	document Document,
) (int64, error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return 0, err
	}
	var docID int64
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		"INSERT INTO %s (doc_id, derived_id, session_ref, harness, body,"+
			" content_hash) VALUES (nextval('%s'), $1, $2, $3, $4, $5)"+
			" RETURNING doc_id",
		catalog.table(tableSearchDocument),
		catalog.table(derivedSequences[tableSearchDocument]),
	),
		derived,
		document.SessionRef,
		document.Harness,
		document.Body,
		document.ContentHash,
	).Scan(&docID); err != nil {
		return 0, fmt.Errorf("add document: %w", err)
	}
	return docID, nil
}

func (catalog *Catalog) AddFacet(
	ctx context.Context,
	derived int64,
	docID int64,
	facet FacetFilter,
) error {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s (doc_id, derived_id, namespace, "key", value)`+
			" VALUES ($1, $2, $3, $4, $5)",
		catalog.table(tableSearchFacet),
	), docID, derived, facet.Namespace, facet.Key, facet.Value); err != nil {
		return fmt.Errorf("add facet: %w", err)
	}
	return nil
}

func (catalog *Catalog) EnsureEmbeddingSpace(
	ctx context.Context,
	space EmbeddingSpace,
) (int64, error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return 0, err
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO %s.embedding_space"+
			" (name, provider, model, dimensions, distance)"+
			" VALUES ($1, $2, $3, $4, $5) ON CONFLICT (name) DO NOTHING",
		catalog.schema,
	),
		space.Name,
		space.Provider,
		space.Model,
		space.Dimensions,
		space.Distance,
	); err != nil {
		return 0, fmt.Errorf("record embedding space: %w", err)
	}
	var id int64
	var stored EmbeddingSpace
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT id, provider, model, dimensions, distance"+
			" FROM %s.embedding_space WHERE name = $1",
		catalog.schema,
	), space.Name).Scan(
		&id,
		&stored.Provider,
		&stored.Model,
		&stored.Dimensions,
		&stored.Distance,
	); err != nil {
		return 0, fmt.Errorf("read embedding space: %w", err)
	}
	stored.Name = space.Name
	if stored != space {
		return 0, fmt.Errorf(
			"embedding space %q is already recorded with a different identity",
			space.Name,
		)
	}
	return id, nil
}

func (catalog *Catalog) AttachEmbedding(
	ctx context.Context,
	docID int64,
	spaceID int64,
	contentHash []byte,
	vector []float32,
) (result AttachResult, err error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return AttachResult{}, err
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		return AttachResult{}, fmt.Errorf("begin embedding attach: %w", err)
	}
	defer func() {
		err = errors.Join(err, discard(ctx, transaction))
	}()
	var cached string
	scanErr := transaction.QueryRow(ctx, fmt.Sprintf(
		"SELECT embedding::text FROM %s.embedding_cache"+
			" WHERE space_id = $1 AND content_hash = $2",
		catalog.schema,
	), spaceID, contentHash).Scan(&cached)
	switch {
	case scanErr == nil:
		result.Reused = true
	case errors.Is(scanErr, pgx.ErrNoRows):
		cached = vectorLiteral(vector)
		if _, err := transaction.Exec(ctx, fmt.Sprintf(
			"INSERT INTO %s.embedding_cache (space_id, content_hash, embedding)"+
				" VALUES ($1, $2, $3::vector)",
			catalog.schema,
		), spaceID, contentHash, cached); err != nil {
			return AttachResult{}, fmt.Errorf("cache embedding: %w", err)
		}
	default:
		return AttachResult{}, fmt.Errorf("read embedding cache: %w", scanErr)
	}
	if _, err := transaction.Exec(ctx, fmt.Sprintf(
		"UPDATE %s SET embedding_space_id = $1, embedding = $2::vector"+
			" WHERE doc_id = $3",
		catalog.table(tableSearchDocument),
	), spaceID, cached, docID); err != nil {
		return AttachResult{}, fmt.Errorf("attach embedding: %w", err)
	}
	if err := transaction.Commit(ctx); err != nil {
		return AttachResult{}, fmt.Errorf("commit embedding attach: %w", err)
	}
	return result, nil
}

func vectorLiteral(vector []float32) string {
	parts := make([]string, len(vector))
	for index, value := range vector {
		parts[index] = strconv.FormatFloat(float64(value), 'g', -1, 32)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// MaintainIndexes adds the per-space vector indexes the shared document table
// is still missing and settles the shared retrieval indexes after new rows.
// The BM25 and trigram indexes are substrate: they exist from catalog init and
// every insert maintains them, so a generation builds no index of its own.
func (catalog *Catalog) MaintainIndexes(
	ctx context.Context,
	generation GenerationID,
	changed bool,
) error {
	if err := catalog.maintainIndexes(ctx, changed); err != nil {
		// A failed index build must never leave a publishable generation.
		return errors.Join(err, catalog.markFailed(ctx, generation))
	}
	return nil
}

func (catalog *Catalog) maintainIndexes(
	ctx context.Context,
	changed bool,
) error {
	statements, err := catalog.indexStatements(ctx)
	if err != nil {
		return err
	}
	if err := catalog.createIndexes(ctx, statements); err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return catalog.settleIndexes(ctx)
}

func (catalog *Catalog) createIndexes(
	ctx context.Context,
	statements []string,
) (err error) {
	if len(statements) == 0 {
		return nil
	}
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return err
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin index build: %w", err)
	}
	defer func() {
		err = errors.Join(err, discard(ctx, transaction))
	}()
	if err := liftStatementTimeout(ctx, transaction); err != nil {
		return err
	}
	for _, statement := range statements {
		if _, err := transaction.Exec(ctx, statement); err != nil {
			return fmt.Errorf("build search index: %w", err)
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("commit index build: %w", err)
	}
	return nil
}

// settleIndexes flushes the trigram index's pending list and refreshes the
// statistics of every table this scan wrote. Without the flush a literal query
// pays for the unmerged inserts of the last scan and the planner abandons the
// trigram path; without the statistics the whole-corpus maintenance aggregates
// pick a plan that costs minutes instead of seconds.
func (catalog *Catalog) settleIndexes(ctx context.Context) error {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return err
	}
	connection, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire PostgreSQL connection: %w", err)
	}
	defer connection.Release()
	if _, err := connection.Exec(
		ctx,
		"SET statement_timeout = "+maintenanceStatementTimeout,
	); err != nil {
		return fmt.Errorf("lift the maintenance statement timeout: %w", err)
	}
	if _, err := connection.Exec(
		ctx,
		"SELECT gin_clean_pending_list($1::regclass)",
		catalog.settings.SchemaName+"."+trigramIndexName,
	); err != nil {
		return fmt.Errorf("flush the trigram pending list: %w", err)
	}
	for _, table := range analyzedTables() {
		if _, err := connection.Exec(
			ctx,
			"ANALYZE "+catalog.table(table),
		); err != nil {
			return fmt.Errorf("refresh %s statistics: %w", table, err)
		}
	}
	return nil
}

// analyzedTables are every table a scan writes, membership included.
func analyzedTables() []string {
	tables := make([]string, 0, len(derivedTables)+1)
	tables = append(tables, derivedTables...)
	return append(tables, "generation_member")
}

func (catalog *Catalog) indexStatements(ctx context.Context) ([]string, error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, fmt.Sprintf(
		"SELECT DISTINCT space.id, space.dimensions"+
			" FROM %s document"+
			" JOIN %s.embedding_space space"+
			" ON space.id = document.embedding_space_id"+
			" WHERE NOT EXISTS (SELECT 1 FROM pg_indexes"+
			" WHERE schemaname = $1 AND indexname ="+
			" '%s_hnsw_s' || space.id)"+
			" ORDER BY space.id",
		catalog.table(tableSearchDocument),
		catalog.schema,
		tableSearchDocument,
	), catalog.settings.SchemaName)
	if err != nil {
		return nil, fmt.Errorf("list indexable embedding spaces: %w", err)
	}
	defer rows.Close()
	var statements []string
	for rows.Next() {
		var spaceID int64
		var dimensions int
		if err := rows.Scan(&spaceID, &dimensions); err != nil {
			return nil, fmt.Errorf("read indexable embedding space: %w", err)
		}
		statements = append(statements, fmt.Sprintf(
			"CREATE INDEX %s ON %s"+
				" USING hnsw ((embedding::vector(%d)) vector_cosine_ops)"+
				" WHERE embedding_space_id = %d",
			quoteIdentifier(vectorIndexName(spaceID)),
			catalog.table(tableSearchDocument),
			dimensions,
			spaceID,
		))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read indexable embedding space: %w", err)
	}
	return statements, nil
}

func (catalog *Catalog) markFailed(
	ctx context.Context,
	generation GenerationID,
) error {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		"UPDATE %s.generation SET state = $1 WHERE id = $2",
		catalog.schema,
	), StateFailed, generation); err != nil {
		return fmt.Errorf("mark generation failed: %w", err)
	}
	return nil
}

// Publish makes a complete candidate the active generation.
func (catalog *Catalog) Publish(
	ctx context.Context,
	generation GenerationID,
) error {
	return catalog.publish(ctx, generation, StateComplete, nil)
}

// PublishPartial publishes a generation that is knowingly missing sources. The
// failed set travels with the generation so no query can read it as complete.
func (catalog *Catalog) PublishPartial(
	ctx context.Context,
	generation GenerationID,
	failed []SourceFailure,
) error {
	if len(failed) == 0 {
		return errors.New("a partial generation requires its failed source set")
	}
	return catalog.publish(ctx, generation, StatePartial, failed)
}

// SourceFailure records why one source is missing from a partial generation.
type SourceFailure struct {
	SourceID string `json:"source_id"`
	Harness  string `json:"harness"`
	Reason   string `json:"reason"`
}

// publish is one metadata transaction: no DDL and no derived row is moved.
func (catalog *Catalog) publish(
	ctx context.Context,
	generation GenerationID,
	state string,
	failed []SourceFailure,
) (err error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return err
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin publication: %w", err)
	}
	defer func() {
		err = errors.Join(err, discard(ctx, transaction))
	}()
	var current string
	if err := transaction.QueryRow(ctx, fmt.Sprintf(
		"SELECT state FROM %s.generation WHERE id = $1 FOR UPDATE",
		catalog.schema,
	), generation).Scan(&current); err != nil {
		return fmt.Errorf("read generation state: %w", err)
	}
	if current != StateBuilding {
		return fmt.Errorf(
			"generation %d is in state %s and cannot be published",
			generation,
			current,
		)
	}
	if err := catalog.verifyIndexes(ctx, transaction, generation); err != nil {
		return err
	}
	diagnostics, err := publicationDiagnostics(failed)
	if err != nil {
		return err
	}
	if _, err := transaction.Exec(ctx, fmt.Sprintf(
		"UPDATE %s.generation SET state = $1"+
			" WHERE id IN (SELECT generation_id FROM %s.active_generation)",
		catalog.schema,
		catalog.schema,
	), StateSuperseded); err != nil {
		return fmt.Errorf("supersede the previous generation: %w", err)
	}
	if _, err := transaction.Exec(ctx, fmt.Sprintf(
		"UPDATE %s.generation SET state = $1, published_at = now(),"+
			" diagnostics = $2 WHERE id = $3",
		catalog.schema,
	), state, diagnostics, generation); err != nil {
		return fmt.Errorf("complete generation: %w", err)
	}
	if _, err := transaction.Exec(ctx, fmt.Sprintf(
		"INSERT INTO %s.active_generation (one, generation_id)"+
			" VALUES (true, $1)"+
			" ON CONFLICT (one) DO UPDATE SET generation_id = EXCLUDED.generation_id",
		catalog.schema,
	), generation); err != nil {
		return fmt.Errorf("move the active generation pointer: %w", err)
	}
	if catalog.publishHook != nil {
		if err := catalog.publishHook(ctx); err != nil {
			return fmt.Errorf("publication was interrupted: %w", err)
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("commit publication: %w", err)
	}
	return nil
}

func publicationDiagnostics(failed []SourceFailure) ([]byte, error) {
	if len(failed) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(map[string]any{"failed_sources": failed})
	if err != nil {
		return nil, fmt.Errorf("encode generation diagnostics: %w", err)
	}
	return encoded, nil
}

// verifyIndexes refuses to publish while a retrieval index is missing or was
// left invalid by a failed build.
func (catalog *Catalog) verifyIndexes(
	ctx context.Context,
	transaction pgx.Tx,
	generation GenerationID,
) error {
	required := []string{bm25IndexName, trigramIndexName}
	var found int
	if err := transaction.QueryRow(
		ctx,
		"SELECT count(*) FROM pg_index index"+
			" JOIN pg_class class ON class.oid = index.indexrelid"+
			" JOIN pg_namespace namespace ON namespace.oid = class.relnamespace"+
			" WHERE namespace.nspname = $1 AND class.relname = ANY($2)"+
			" AND index.indisvalid AND index.indisready",
		catalog.settings.SchemaName,
		required,
	).Scan(&found); err != nil {
		return fmt.Errorf("verify search indexes: %w", err)
	}
	if found != len(required) {
		return fmt.Errorf(
			"generation %d found %d of %d valid search indexes and cannot be"+
				" published",
			generation,
			found,
			len(required),
		)
	}
	return nil
}

func (catalog *Catalog) ActiveGeneration(
	ctx context.Context,
) (GenerationID, bool, error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return 0, false, err
	}
	var generation GenerationID
	err = pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT generation_id FROM %s.active_generation",
		catalog.schema,
	)).Scan(&generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read the active generation: %w", err)
	}
	return generation, true, nil
}

func (catalog *Catalog) GenerationState(
	ctx context.Context,
	generation GenerationID,
) (string, error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return "", err
	}
	var state string
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT state FROM %s.generation WHERE id = $1",
		catalog.schema,
	), generation).Scan(&state); err != nil {
		return "", fmt.Errorf("read generation state: %w", err)
	}
	return state, nil
}

// Cleanup drops one reclaimable generation's membership and every derived row
// no live generation still presents. A concurrent reader keeps its own MVCC
// snapshot, so no table is dropped and no reader is interrupted.
func (catalog *Catalog) Cleanup(
	ctx context.Context,
	generation GenerationID,
) (err error) {
	active, present, err := catalog.ActiveGeneration(ctx)
	if err != nil {
		return err
	}
	if present && active == generation {
		return fmt.Errorf(
			"generation %d is active and cannot be cleaned up",
			generation,
		)
	}
	state, err := catalog.GenerationState(ctx, generation)
	if err != nil {
		return err
	}
	if state != StateFailed && state != StateSuperseded {
		return fmt.Errorf(
			"generation %d is in state %s and cannot be cleaned up",
			generation,
			state,
		)
	}
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return err
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin generation cleanup: %w", err)
	}
	defer func() {
		err = errors.Join(err, discard(ctx, transaction))
	}()
	if err := liftStatementTimeout(ctx, transaction); err != nil {
		return err
	}
	if _, err := transaction.Exec(ctx, fmt.Sprintf(
		"DELETE FROM %s.generation_member WHERE generation_id = $1",
		catalog.schema,
	), generation); err != nil {
		return fmt.Errorf("drop generation membership: %w", err)
	}
	if err := catalog.deleteOrphanedDerived(ctx, transaction); err != nil {
		return err
	}
	// reclaimed_at is what keeps a later scan from re-walking the derived
	// tables for a generation whose rows are already gone.
	if _, err := transaction.Exec(ctx, fmt.Sprintf(
		"UPDATE %s.generation SET reclaimed_at = now() WHERE id = $1",
		catalog.schema,
	), generation); err != nil {
		return fmt.Errorf("record generation cleanup: %w", err)
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("commit generation cleanup: %w", err)
	}
	return nil
}

// orphanedDerivedQuery selects every derived session no generation presents.
const orphanedDerivedQuery = `SELECT session.id FROM %s session
WHERE NOT EXISTS (
	SELECT 1 FROM %s.generation_member member
	WHERE member.derived_id = session.id)`

func (catalog *Catalog) deleteOrphanedDerived(
	ctx context.Context,
	transaction pgx.Tx,
) error {
	rows, err := transaction.Query(ctx, fmt.Sprintf(
		orphanedDerivedQuery,
		catalog.table(tableDerivedSession),
		catalog.schema,
	))
	if err != nil {
		return fmt.Errorf("list orphaned derived sessions: %w", err)
	}
	var orphaned []int64
	for rows.Next() {
		var derived int64
		if err := rows.Scan(&derived); err != nil {
			rows.Close()
			return fmt.Errorf("read orphaned derived session: %w", err)
		}
		orphaned = append(orphaned, derived)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read orphaned derived session: %w", err)
	}
	if len(orphaned) == 0 {
		return nil
	}
	for _, statement := range catalog.orphanDeletions() {
		if _, err := transaction.Exec(ctx, statement, orphaned); err != nil {
			return fmt.Errorf("reclaim derived rows: %w", err)
		}
	}
	return nil
}

// orphanDeletions remove one orphaned session's rows in dependency order.
func (catalog *Catalog) orphanDeletions() []string {
	document := catalog.table(tableSearchDocument)
	passage := catalog.table(tableDerivedPassage)
	event := catalog.table(tableDerivedEvent)
	return []string{
		fmt.Sprintf(
			"DELETE FROM %s WHERE derived_id = ANY($1)",
			catalog.table(tableSearchFacet),
		),
		fmt.Sprintf(
			"DELETE FROM %s WHERE doc_id IN"+
				" (SELECT doc_id FROM %s WHERE derived_id = ANY($1))",
			catalog.table(tableProjectionLimit),
			document,
		),
		fmt.Sprintf("DELETE FROM %s WHERE derived_id = ANY($1)", document),
		fmt.Sprintf(
			"DELETE FROM %s WHERE passage_id IN"+
				" (SELECT id FROM %s WHERE derived_id = ANY($1))",
			catalog.table(tableDerivedPassageEvent),
			passage,
		),
		fmt.Sprintf("DELETE FROM %s WHERE derived_id = ANY($1)", passage),
		fmt.Sprintf(
			"DELETE FROM %s WHERE derived_id = ANY($1)",
			catalog.table(tableDerivedRelation),
		),
		fmt.Sprintf(
			"DELETE FROM %s WHERE event_id IN"+
				" (SELECT id FROM %s WHERE derived_id = ANY($1))",
			catalog.table(tableDerivedEvidence),
			event,
		),
		fmt.Sprintf("DELETE FROM %s WHERE derived_id = ANY($1)", event),
		fmt.Sprintf(
			"DELETE FROM %s WHERE id = ANY($1)",
			catalog.table(tableDerivedSession),
		),
	}
}

// EligibleThenRank computes the eligible set before any candidate limit.
func (catalog *Catalog) EligibleThenRank(
	ctx context.Context,
	generation GenerationID,
	filter FacetFilter,
	query RankQuery,
) (result RankResult, err error) {
	statement, arguments, err := catalog.rankStatement(generation, filter, query)
	if err != nil {
		return RankResult{}, err
	}
	result.SQL = statement
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return RankResult{}, err
	}
	transaction, err := pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return RankResult{}, fmt.Errorf("begin retrieval: %w", err)
	}
	defer func() {
		err = errors.Join(err, discard(ctx, transaction))
	}()
	rows, err := transaction.Query(ctx, statement, arguments...)
	if err != nil {
		return RankResult{}, fmt.Errorf("rank documents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var document RankedDocument
		if err := rows.Scan(
			&document.DocID,
			&document.SessionRef,
			&document.Score,
		); err != nil {
			return RankResult{}, fmt.Errorf("read ranked document: %w", err)
		}
		result.Documents = append(result.Documents, document)
	}
	if err := rows.Err(); err != nil {
		return RankResult{}, fmt.Errorf("read ranked document: %w", err)
	}
	if err := transaction.Commit(ctx); err != nil {
		return RankResult{}, fmt.Errorf("commit retrieval: %w", err)
	}
	return result, nil
}

func (catalog *Catalog) rankStatement(
	generation GenerationID,
	filter FacetFilter,
	query RankQuery,
) (string, []any, error) {
	if query.Limit <= 0 {
		return "", nil, errors.New("rank query requires a positive limit")
	}
	document := catalog.table(tableSearchDocument)
	eligible := fmt.Sprintf(
		"WITH eligible AS (SELECT facet.doc_id FROM %s facet"+
			" JOIN %s.generation_member member"+
			" ON member.derived_id = facet.derived_id"+
			` WHERE member.generation_id = $1 AND facet.namespace = $2`+
			` AND facet."key" = $3 AND facet.value = $4)`,
		catalog.table(tableSearchFacet),
		catalog.schema,
	)
	arguments := []any{
		int64(generation),
		filter.Namespace,
		filter.Key,
		filter.Value,
	}
	switch query.Mode {
	case RankBM25:
		arguments = append(
			arguments,
			query.Text,
			catalog.settings.SchemaName+"."+bm25IndexName,
			query.Limit,
		)
		return fmt.Sprintf(
			"%s SELECT document.doc_id, document.session_ref,"+
				" (document.body <@> to_bm25query($5, $6))::float8 AS score"+
				" FROM %s document"+
				" WHERE document.doc_id IN (SELECT doc_id FROM eligible)"+
				" ORDER BY document.body <@> to_bm25query($5, $6),"+
				" document.doc_id"+
				" LIMIT $7",
			eligible,
			document,
		), arguments, nil
	case RankVector:
		if query.Dimensions <= 0 {
			return "", nil, errors.New("vector rank query requires dimensions")
		}
		arguments = append(
			arguments,
			vectorLiteral(query.Vector),
			query.SpaceID,
			query.Limit,
		)
		cast := fmt.Sprintf("vector(%d)", query.Dimensions)
		return fmt.Sprintf(
			"%s SELECT document.doc_id, document.session_ref,"+
				" ((document.embedding::%s) <=> $5::%s)::float8 AS score"+
				" FROM %s document"+
				" WHERE document.doc_id IN (SELECT doc_id FROM eligible)"+
				" AND document.embedding_space_id = $6"+
				" ORDER BY (document.embedding::%s) <=> $5::%s,"+
				" document.doc_id"+
				" LIMIT $7",
			eligible,
			cast,
			cast,
			document,
			cast,
			cast,
		), arguments, nil
	default:
		return "", nil, fmt.Errorf("unsupported rank mode %q", query.Mode)
	}
}

func liftStatementTimeout(ctx context.Context, transaction pgx.Tx) error {
	if _, err := transaction.Exec(
		ctx,
		"SET LOCAL statement_timeout = "+maintenanceStatementTimeout,
	); err != nil {
		return fmt.Errorf("lift the maintenance statement timeout: %w", err)
	}
	return nil
}

func discard(ctx context.Context, transaction pgx.Tx) error {
	err := transaction.Rollback(ctx)
	if err == nil || errors.Is(err, pgx.ErrTxClosed) {
		return nil
	}
	if ctx.Err() != nil {
		// The caller cancelled; the server rolls the transaction back anyway.
		return nil
	}
	return fmt.Errorf("roll back transaction: %w", err)
}

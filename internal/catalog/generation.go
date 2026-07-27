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

// GenerationID identifies one catalog generation and its own tables.
type GenerationID int64

const (
	StateBuilding   = "building"
	StateComplete   = "complete"
	StatePartial    = "partial"
	StateFailed     = "failed"
	StateSuperseded = "superseded"
)

// ErrCleanupBusy reports that a reader still holds the generation tables.
var ErrCleanupBusy = errors.New("generation tables are locked by a reader")

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

func (catalog *Catalog) BeginCandidate(
	ctx context.Context,
	parent *GenerationID,
) (generation GenerationID, err error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return 0, err
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin candidate generation: %w", err)
	}
	defer func() {
		err = errors.Join(err, discard(ctx, transaction))
	}()
	if err := transaction.QueryRow(ctx, fmt.Sprintf(
		"INSERT INTO %s.generation (state, parent_id) VALUES ($1, $2)"+
			" RETURNING id",
		catalog.schema,
	), StateBuilding, parent).Scan(&generation); err != nil {
		return 0, fmt.Errorf("record candidate generation: %w", err)
	}
	for _, statement := range generationStatements(catalog.schema, generation) {
		if _, err := transaction.Exec(ctx, statement); err != nil {
			return 0, fmt.Errorf("create generation tables: %w", err)
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit candidate generation: %w", err)
	}
	return generation, nil
}

func (catalog *Catalog) AddDocument(
	ctx context.Context,
	generation GenerationID,
	document Document,
) (int64, error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return 0, err
	}
	var docID int64
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		"INSERT INTO %s.%s (session_ref, harness, body, content_hash)"+
			" VALUES ($1, $2, $3, $4) RETURNING doc_id",
		catalog.schema,
		quoteIdentifier(documentTable(generation)),
	),
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
	generation GenerationID,
	docID int64,
	facet FacetFilter,
) error {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s.%s (doc_id, namespace, "key", value)`+
			" VALUES ($1, $2, $3, $4)",
		catalog.schema,
		quoteIdentifier(facetTable(generation)),
	), docID, facet.Namespace, facet.Key, facet.Value); err != nil {
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
	generation GenerationID,
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
		"UPDATE %s.%s SET embedding_space_id = $1, embedding = $2::vector"+
			" WHERE doc_id = $3",
		catalog.schema,
		quoteIdentifier(documentTable(generation)),
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

func (catalog *Catalog) BuildIndexes(
	ctx context.Context,
	generation GenerationID,
) error {
	if err := catalog.buildIndexes(ctx, generation); err != nil {
		// A failed index build must never leave a publishable generation.
		return errors.Join(err, catalog.markFailed(ctx, generation))
	}
	return nil
}

func (catalog *Catalog) buildIndexes(
	ctx context.Context,
	generation GenerationID,
) (err error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return err
	}
	statements, err := catalog.indexStatements(ctx, generation)
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
			return fmt.Errorf("build generation index: %w", err)
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("commit index build: %w", err)
	}
	return nil
}

func (catalog *Catalog) indexStatements(
	ctx context.Context,
	generation GenerationID,
) ([]string, error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return nil, err
	}
	document := quoteIdentifier(documentTable(generation))
	statements := []string{
		fmt.Sprintf(
			"CREATE INDEX %s ON %s.%s USING bm25 (body)"+
				" WITH (text_config='%s')",
			quoteIdentifier(bm25IndexName(generation)),
			catalog.schema,
			document,
			BM25TextConfig,
		),
		fmt.Sprintf(
			"CREATE INDEX %s ON %s.%s USING %s (body %s_trgm_ops)",
			quoteIdentifier(trigramIndexName(generation)),
			catalog.schema,
			document,
			TrigramIndex,
			TrigramIndex,
		),
	}
	rows, err := pool.Query(ctx, fmt.Sprintf(
		"SELECT DISTINCT space.id, space.dimensions"+
			" FROM %s.%s document"+
			" JOIN %s.embedding_space space"+
			" ON space.id = document.embedding_space_id"+
			" ORDER BY space.id",
		catalog.schema,
		document,
		catalog.schema,
	))
	if err != nil {
		return nil, fmt.Errorf("list generation embedding spaces: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var spaceID int64
		var dimensions int
		if err := rows.Scan(&spaceID, &dimensions); err != nil {
			return nil, fmt.Errorf("read generation embedding space: %w", err)
		}
		statements = append(statements, fmt.Sprintf(
			"CREATE INDEX %s ON %s.%s"+
				" USING hnsw ((embedding::vector(%d)) vector_cosine_ops)"+
				" WHERE embedding_space_id = %d",
			quoteIdentifier(vectorIndexName(generation, spaceID)),
			catalog.schema,
			document,
			dimensions,
			spaceID,
		))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read generation embedding space: %w", err)
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

func (catalog *Catalog) verifyIndexes(
	ctx context.Context,
	transaction pgx.Tx,
	generation GenerationID,
) error {
	required := []string{
		bm25IndexName(generation),
		trigramIndexName(generation),
	}
	var found int
	if err := transaction.QueryRow(
		ctx,
		"SELECT count(*) FROM pg_indexes"+
			" WHERE schemaname = $1 AND tablename = $2 AND indexname = ANY($3)",
		catalog.settings.SchemaName,
		documentTable(generation),
		required,
	).Scan(&found); err != nil {
		return fmt.Errorf("verify generation indexes: %w", err)
	}
	if found != len(required) {
		return fmt.Errorf(
			"generation %d has %d of %d required indexes and cannot be published",
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

// Cleanup drops a reclaimable generation without waiting out a live reader.
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
	if _, err := transaction.Exec(
		ctx,
		"SET LOCAL lock_timeout = '"+cleanupLockTimeout+"'",
	); err != nil {
		return fmt.Errorf("bound the cleanup lock wait: %w", err)
	}
	for _, table := range generationTables(generation) {
		if _, err := transaction.Exec(ctx, fmt.Sprintf(
			"DROP TABLE IF EXISTS %s.%s",
			catalog.schema,
			quoteIdentifier(table),
		)); err != nil {
			if isSQLState(err, sqlStateLockNotAvailable) {
				return fmt.Errorf(
					"%w: generation %d: %w",
					ErrCleanupBusy,
					generation,
					err,
				)
			}
			return fmt.Errorf("drop generation table %s: %w", table, err)
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("commit generation cleanup: %w", err)
	}
	return nil
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
	// Readers hold AccessShare on the active generation for the whole read.
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
	document := quoteIdentifier(documentTable(generation))
	facet := quoteIdentifier(facetTable(generation))
	eligible := fmt.Sprintf(
		"WITH eligible AS (SELECT doc_id FROM %s.%s"+
			` WHERE namespace = $1 AND "key" = $2 AND value = $3)`,
		catalog.schema,
		facet,
	)
	arguments := []any{filter.Namespace, filter.Key, filter.Value}
	switch query.Mode {
	case RankBM25:
		arguments = append(
			arguments,
			query.Text,
			catalog.settings.SchemaName+"."+bm25IndexName(generation),
			query.Limit,
		)
		return fmt.Sprintf(
			"%s SELECT document.doc_id, document.session_ref,"+
				" (document.body <@> to_bm25query($4, $5))::float8 AS score"+
				" FROM %s.%s document"+
				" WHERE document.doc_id IN (SELECT doc_id FROM eligible)"+
				" ORDER BY document.body <@> to_bm25query($4, $5),"+
				" document.doc_id"+
				" LIMIT $6",
			eligible,
			catalog.schema,
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
				" ((document.embedding::%s) <=> $4::%s)::float8 AS score"+
				" FROM %s.%s document"+
				" WHERE document.doc_id IN (SELECT doc_id FROM eligible)"+
				" AND document.embedding_space_id = $5"+
				" ORDER BY (document.embedding::%s) <=> $4::%s,"+
				" document.doc_id"+
				" LIMIT $6",
			eligible,
			cast,
			cast,
			catalog.schema,
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

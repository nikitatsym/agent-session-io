package catalog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// SearchMode selects which retrieval leg answers a request.
type SearchMode string

const (
	SearchModeLexical SearchMode = "lexical"
	SearchModeLiteral SearchMode = "literal"
)

// Legs name the retrieval path that produced a hit.
const (
	LegBM25    = "bm25"
	LegLiteral = "literal"
)

// Literal execution paths reported to the caller.
const (
	LiteralPathTrigram = "trigram"
	LiteralPathScan    = "scan"
)

// DefaultSearchLimit bounds a request that names no limit.
const DefaultSearchLimit = 20

// SearchRequest is the typed retrieval seam; no caller supplies SQL.
type SearchRequest struct {
	Query string
	Mode  SearchMode
	// Filter is the hard predicate applied before the candidate limit.
	Filter *FacetFilter
	Limit  int
}

// HitEvidence locates one native observation behind a passage.
type HitEvidence struct {
	Observation string
	Locator     Locator
}

// SearchHit carries one passage with its provenance and match explanation.
type SearchHit struct {
	Rank                  int
	MatchedLeg            string
	BM25Score             *float64
	SessionKey            string
	Harness               string
	NativeID              string
	Title                 string
	SourceID              string
	OccurrenceID          string
	DiscoveryRevision     string
	SessionLocator        Locator
	SessionStartedAt      *time.Time
	SessionUpdatedAt      *time.Time
	PassageID             int64
	PassageOrdinal        int
	PassageKind           string
	PassageBuilderVersion string
	PassageOccurredAt     *time.Time
	ProjectionKind        string
	ProjectionVersion     string
	Body                  string
	// Limitations name every way this body deviates from the native bytes.
	Limitations []ProjectionLimitation
	EventKeys   []string
	Evidence    []HitEvidence
}

// SearchResult reports the generation that answered the request.
type SearchResult struct {
	Generation      GenerationID
	GenerationState string
	Complete        bool
	LiteralPath     string
	SQL             string
	Hits            []SearchHit
}

// Search reads exactly one active generation inside one read-only snapshot,
// so a concurrent scan or reclaim can never change the answer mid-read.
func (catalog *Catalog) Search(
	ctx context.Context,
	request SearchRequest,
) (result SearchResult, err error) {
	if strings.TrimSpace(request.Query) == "" {
		return SearchResult{}, &Error{
			Kind:        KindSearchRequestInvalid,
			Message:     "the search query is empty",
			Remediation: "pass a non-empty query string",
		}
	}
	if request.Mode != SearchModeLexical && request.Mode != SearchModeLiteral {
		return SearchResult{}, &Error{
			Kind: KindSearchRequestInvalid,
			Message: fmt.Sprintf(
				"search mode %q is unsupported",
				request.Mode,
			),
			Remediation: "use --mode lexical or --mode literal",
			Details:     map[string]any{"mode": string(request.Mode)},
		}
	}
	if request.Limit <= 0 {
		request.Limit = DefaultSearchLimit
	}
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return SearchResult{}, err
	}
	connection, err := pool.Acquire(ctx)
	if err != nil {
		return SearchResult{}, fmt.Errorf("acquire PostgreSQL connection: %w", err)
	}
	defer connection.Release()
	if _, err := catalog.status(ctx, connection); err != nil {
		return SearchResult{}, err
	}
	transaction, err := connection.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return SearchResult{}, fmt.Errorf("begin retrieval: %w", err)
	}
	defer func() {
		err = errors.Join(err, discard(ctx, transaction))
	}()
	result, err = catalog.search(ctx, transaction, request)
	if err != nil {
		return SearchResult{}, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return SearchResult{}, fmt.Errorf("commit retrieval: %w", err)
	}
	return result, nil
}

func (catalog *Catalog) search(
	ctx context.Context,
	transaction pgx.Tx,
	request SearchRequest,
) (SearchResult, error) {
	generation, state, err := catalog.activeGenerationForRead(ctx, transaction)
	if err != nil {
		return SearchResult{}, err
	}
	result := SearchResult{
		Generation:      generation,
		GenerationState: state,
		Complete:        state == StateComplete,
	}
	statement, arguments := catalog.searchStatement(generation, request)
	result.SQL = statement
	if request.Mode == SearchModeLiteral {
		result.LiteralPath, err = literalPath(
			ctx,
			transaction,
			trigramIndexName,
			statement,
			arguments,
		)
		if err != nil {
			return SearchResult{}, err
		}
	}
	ranked, err := readRankedDocuments(ctx, transaction, statement, arguments)
	if err != nil {
		return SearchResult{}, err
	}
	if len(ranked) == 0 {
		return result, nil
	}
	result.Hits, err = catalog.hydrate(
		ctx,
		transaction,
		generation,
		request,
		ranked,
	)
	if err != nil {
		return SearchResult{}, err
	}
	return result, nil
}

func (catalog *Catalog) activeGenerationForRead(
	ctx context.Context,
	transaction pgx.Tx,
) (GenerationID, string, error) {
	var generation GenerationID
	var state string
	err := transaction.QueryRow(ctx, fmt.Sprintf(
		"SELECT generation.id, generation.state"+
			" FROM %s.active_generation active"+
			" JOIN %s.generation generation"+
			" ON generation.id = active.generation_id",
		catalog.schema,
		catalog.schema,
	)).Scan(&generation, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", &Error{
			Kind: KindCatalogGenerationIncomplete,
			Message: fmt.Sprintf(
				"catalog schema %s has no active generation",
				catalog.settings.SchemaName,
			),
			Remediation: "sessionio scan",
			Details: map[string]any{
				"catalog_schema": catalog.settings.SchemaName,
			},
		}
	}
	if err != nil {
		return 0, "", fmt.Errorf("read the active generation: %w", err)
	}
	return generation, state, nil
}

// literalPath reports the plan PostgreSQL actually chose, so a query without
// usable trigrams is named as the bounded scan instead of passing for an
// accelerated one.
func literalPath(
	ctx context.Context,
	transaction pgx.Tx,
	index string,
	statement string,
	arguments []any,
) (string, error) {
	rows, err := transaction.Query(ctx, "EXPLAIN "+statement, arguments...)
	if err != nil {
		return "", fmt.Errorf("explain the literal query: %w", err)
	}
	defer rows.Close()
	path := LiteralPathScan
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", fmt.Errorf("read the literal query plan: %w", err)
		}
		if strings.Contains(line, index) {
			path = LiteralPathTrigram
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("read the literal query plan: %w", err)
	}
	return path, nil
}

type rankedDocument struct {
	docID int64
	score *float64
}

func readRankedDocuments(
	ctx context.Context,
	transaction pgx.Tx,
	statement string,
	arguments []any,
) ([]rankedDocument, error) {
	rows, err := transaction.Query(ctx, statement, arguments...)
	if err != nil {
		return nil, fmt.Errorf("rank documents: %w", err)
	}
	defer rows.Close()
	var ranked []rankedDocument
	for rows.Next() {
		var document rankedDocument
		if err := rows.Scan(&document.docID, &document.score); err != nil {
			return nil, fmt.Errorf("read ranked document: %w", err)
		}
		ranked = append(ranked, document)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read ranked document: %w", err)
	}
	return ranked, nil
}

// searchStatement keeps the eligible-then-rank shape: the generation
// membership and the hard filter are a CTE consumed by IN, so PostgreSQL
// cannot apply the limit before either predicate.
func (catalog *Catalog) searchStatement(
	generation GenerationID,
	request SearchRequest,
) (string, []any) {
	document := catalog.table(tableSearchDocument)
	eligible, arguments := catalog.eligibleClause(generation, request.Filter)
	if request.Mode == SearchModeLiteral {
		arguments = append(arguments, likePattern(request.Query), request.Limit)
		return fmt.Sprintf(
			"%s SELECT document.doc_id, NULL::float8 AS score"+
				" FROM %s document"+
				" WHERE document.doc_id IN (SELECT doc_id FROM eligible)"+
				" AND document.body LIKE $%d"+
				" ORDER BY document.doc_id"+
				" LIMIT $%d",
			eligible,
			document,
			len(arguments)-1,
			len(arguments),
		), arguments
	}
	arguments = append(
		arguments,
		request.Query,
		catalog.settings.SchemaName+"."+bm25IndexName,
		request.Limit,
	)
	query := fmt.Sprintf(
		"document.body <@> to_bm25query($%d, $%d)",
		len(arguments)-2,
		len(arguments)-1,
	)
	// A sequential scan scores every row as 0, so the negative-score predicate
	// is what makes BM25 a match test rather than an ordering.
	return fmt.Sprintf(
		"%s SELECT document.doc_id, (%s)::float8 AS score"+
			" FROM %s document"+
			" WHERE document.doc_id IN (SELECT doc_id FROM eligible)"+
			" AND (%s) < 0"+
			" ORDER BY %s, document.doc_id"+
			" LIMIT $%d",
		eligible,
		query,
		document,
		query,
		query,
		len(arguments),
	), arguments
}

// eligibleClause restricts retrieval to the revisions the active generation
// presents. The shared tables also hold superseded revisions until reclaim, so
// this membership join is a hard predicate, not an optimization.
func (catalog *Catalog) eligibleClause(
	generation GenerationID,
	filter *FacetFilter,
) (string, []any) {
	arguments := []any{int64(generation)}
	if filter == nil {
		return fmt.Sprintf(
			"WITH eligible AS (SELECT document.doc_id FROM %s document"+
				" JOIN %s.generation_member member"+
				" ON member.derived_id = document.derived_id"+
				" WHERE member.generation_id = $1)",
			catalog.table(tableSearchDocument),
			catalog.schema,
		), arguments
	}
	return fmt.Sprintf(
		"WITH eligible AS (SELECT facet.doc_id FROM %s facet"+
			" JOIN %s.generation_member member"+
			" ON member.derived_id = facet.derived_id"+
			" WHERE member.generation_id = $1 AND facet.namespace = $2"+
			` AND facet."key" = $3 AND facet.value = $4)`,
		catalog.table(tableSearchFacet),
		catalog.schema,
	), append(arguments, filter.Namespace, filter.Key, filter.Value)
}

// likePattern keeps LIKE containment exact: every wildcard the caller typed is
// escaped, so the literal leg never widens the requested match.
func likePattern(query string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`%`, `\%`,
		`_`, `\_`,
	)
	return "%" + replacer.Replace(query) + "%"
}

const hydrateQuery = `SELECT document.doc_id,
	document.session_ref,
	document.harness,
	document.projection_kind,
	document.projection_version,
	document.body,
	passage.id,
	passage.ordinal,
	passage.kind,
	passage.builder_version,
	passage.started_at,
	session.native_id,
	session.title,
	session.source_id,
	session.occurrence_id,
	session.discovery_revision,
	session.locator_kind,
	session.locator_root,
	session.locator_path,
	session.started_at,
	session.updated_at
FROM %s document
JOIN %s passage ON passage.id = document.passage_id
JOIN %s session ON session.id = passage.derived_id
WHERE document.doc_id = ANY($1)`

func (catalog *Catalog) hydrate(
	ctx context.Context,
	transaction pgx.Tx,
	generation GenerationID,
	request SearchRequest,
	ranked []rankedDocument,
) ([]SearchHit, error) {
	docIDs := make([]int64, len(ranked))
	for index, document := range ranked {
		docIDs[index] = document.docID
	}
	rows, err := transaction.Query(ctx, fmt.Sprintf(
		hydrateQuery,
		catalog.table(tableSearchDocument),
		catalog.table(tableDerivedPassage),
		catalog.table(tableDerivedSession),
	), docIDs)
	if err != nil {
		return nil, fmt.Errorf("hydrate ranked passages: %w", err)
	}
	defer rows.Close()
	hits := make(map[int64]SearchHit, len(ranked))
	for rows.Next() {
		var docID int64
		var hit SearchHit
		if err := rows.Scan(
			&docID,
			&hit.SessionKey,
			&hit.Harness,
			&hit.ProjectionKind,
			&hit.ProjectionVersion,
			&hit.Body,
			&hit.PassageID,
			&hit.PassageOrdinal,
			&hit.PassageKind,
			&hit.PassageBuilderVersion,
			&hit.PassageOccurredAt,
			&hit.NativeID,
			&hit.Title,
			&hit.SourceID,
			&hit.OccurrenceID,
			&hit.DiscoveryRevision,
			&hit.SessionLocator.Kind,
			&hit.SessionLocator.Root,
			&hit.SessionLocator.Path,
			&hit.SessionStartedAt,
			&hit.SessionUpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("read hydrated passage: %w", err)
		}
		hits[docID] = hit
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read hydrated passage: %w", err)
	}
	limitations, err := catalog.readLimitations(ctx, transaction, docIDs)
	if err != nil {
		return nil, err
	}
	ordered := make([]SearchHit, 0, len(ranked))
	passageIDs := make([]int64, 0, len(ranked))
	for index, document := range ranked {
		hit, found := hits[document.docID]
		if !found {
			return nil, fmt.Errorf(
				"ranked document %d has no passage in generation %d",
				document.docID,
				generation,
			)
		}
		hit.Limitations = limitations[document.docID]
		hit.Rank = index + 1
		hit.MatchedLeg = LegBM25
		if request.Mode == SearchModeLiteral {
			hit.MatchedLeg = LegLiteral
		} else {
			hit.BM25Score = document.score
		}
		ordered = append(ordered, hit)
		passageIDs = append(passageIDs, hit.PassageID)
	}
	return catalog.attachProvenance(ctx, transaction, ordered, passageIDs)
}

func (catalog *Catalog) readLimitations(
	ctx context.Context,
	transaction pgx.Tx,
	docIDs []int64,
) (map[int64][]ProjectionLimitation, error) {
	rows, err := transaction.Query(ctx, fmt.Sprintf(
		"SELECT doc_id, kind, removed_bytes FROM %s"+
			" WHERE doc_id = ANY($1) ORDER BY doc_id, kind",
		catalog.table(tableProjectionLimit),
	), docIDs)
	if err != nil {
		return nil, fmt.Errorf("read projection limitations: %w", err)
	}
	defer rows.Close()
	limitations := map[int64][]ProjectionLimitation{}
	for rows.Next() {
		var docID int64
		var limitation ProjectionLimitation
		if err := rows.Scan(
			&docID,
			&limitation.Kind,
			&limitation.RemovedBytes,
		); err != nil {
			return nil, fmt.Errorf("read projection limitation: %w", err)
		}
		limitations[docID] = append(limitations[docID], limitation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read projection limitation: %w", err)
	}
	return limitations, nil
}

const provenanceQuery = `SELECT passage_event.passage_id,
	event.event_key,
	evidence.observation_id,
	evidence.locator_kind,
	evidence.locator_root,
	evidence.locator_path,
	evidence.locator_record,
	evidence.locator_line,
	evidence.byte_start,
	evidence.byte_end
FROM %s passage_event
JOIN %s event ON event.id = passage_event.event_id
LEFT JOIN %s evidence ON evidence.event_id = event.id
WHERE passage_event.passage_id = ANY($1)
ORDER BY passage_event.passage_id, passage_event.position, evidence.position`

func (catalog *Catalog) attachProvenance(
	ctx context.Context,
	transaction pgx.Tx,
	hits []SearchHit,
	passageIDs []int64,
) ([]SearchHit, error) {
	rows, err := transaction.Query(ctx, fmt.Sprintf(
		provenanceQuery,
		catalog.table(tableDerivedPassageEvent),
		catalog.table(tableDerivedEvent),
		catalog.table(tableDerivedEvidence),
	), passageIDs)
	if err != nil {
		return nil, fmt.Errorf("read passage provenance: %w", err)
	}
	defer rows.Close()
	events := map[int64][]string{}
	evidence := map[int64][]HitEvidence{}
	for rows.Next() {
		var passageID int64
		var eventKey string
		var observation *string
		var found HitEvidence
		var kind, root, path *string
		if err := rows.Scan(
			&passageID,
			&eventKey,
			&observation,
			&kind,
			&root,
			&path,
			&found.Locator.Record,
			&found.Locator.Line,
			&found.Locator.ByteStart,
			&found.Locator.ByteEnd,
		); err != nil {
			return nil, fmt.Errorf("read passage provenance: %w", err)
		}
		if last := events[passageID]; len(last) == 0 ||
			last[len(last)-1] != eventKey {
			events[passageID] = append(events[passageID], eventKey)
		}
		if observation == nil {
			continue
		}
		found.Observation = *observation
		found.Locator.Kind = *kind
		found.Locator.Root = *root
		found.Locator.Path = *path
		evidence[passageID] = append(evidence[passageID], found)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read passage provenance: %w", err)
	}
	for index := range hits {
		hits[index].EventKeys = events[hits[index].PassageID]
		hits[index].Evidence = evidence[hits[index].PassageID]
	}
	return hits, nil
}

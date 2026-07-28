package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Locator is the structured evidence locator retained by the catalog.
type Locator struct {
	Kind      string
	Root      string
	Path      string
	Record    *int64
	Line      *int64
	ByteStart *int64
	ByteEnd   *int64
}

// ScanEvidence ties one retained event to one native observation.
type ScanEvidence struct {
	Observation string
	Locator     Locator
}

// ScanEvent is one retained normalized event.
type ScanEvent struct {
	Key string
	// NativeKey is the harness's own record identifier, empty where a harness
	// has none. It is retained so a relation can name a peer record.
	NativeKey   string
	Kind        string
	Role        string
	Observation string
	OccurredAt  *time.Time
	Evidence    []ScanEvidence
}

// ProjectionLimitation reports a projection that is not byte-exact to the
// native content behind it.
type ProjectionLimitation struct {
	Kind         string `json:"kind"`
	RemovedBytes int64  `json:"removed_bytes"`
}

// ScanPassage is one retained passage and its single searchable projection.
type ScanPassage struct {
	Kind              string
	BuilderVersion    string
	ProjectionKind    string
	ProjectionVersion string
	// Events indexes into ScanSession.Events in source order.
	Events      []int
	Body        string
	ContentHash []byte
	OccurredAt  *time.Time
	Limitations []ProjectionLimitation
	Facets      []FacetFilter
	Part        int
	Parts       int
}

// ScanRelation is one retained structural relation. Its target names an
// identity; which session that identity reaches depends on the generation.
type ScanRelation struct {
	Kind        string
	Origin      string
	FromKind    string
	FromRef     string
	ToKind      string
	ToRef       string
	Observation string
}

// ToKindSessionNative addresses a session by its native identity, which only
// the retained revisions of the same generation can resolve.
const ToKindSessionNative = "session_native"

// ToKindNativeRecord addresses one native record by the key its harness gave
// it, so a record-level target resolves from retained rows alone.
const ToKindNativeRecord = "native_record"

// ScanSession is one reader session retained into the shared derived tables.
type ScanSession struct {
	Key                 string
	Harness             string
	NativeID            string
	Title               string
	SourceID            string
	OccurrenceID        string
	DiscoveryRevision   string
	RevisionHash        []byte
	SourceRevisionKind  string
	SourceRevisionValue string
	Locator             Locator
	StartedAt           *time.Time
	UpdatedAt           *time.Time
	Events              []ScanEvent
	Passages            []ScanPassage
	Relations           []ScanRelation
}

// ScanCounts are the retained row counts one generation presents. Relation
// resolution depends on the whole presented set, so it is a diagnostic that
// only a full build computes; a refresh reports it as absent rather than
// paying a corpus-wide pass for it.
type ScanCounts struct {
	Sessions            int64  `json:"sessions"`
	Events              int64  `json:"events"`
	Evidence            int64  `json:"evidence"`
	Passages            int64  `json:"passages"`
	Projections         int64  `json:"projections"`
	Limitations         int64  `json:"projection_limitations"`
	Relations           int64  `json:"relations"`
	ResolvedRelations   *int64 `json:"resolved_relations"`
	UnresolvedRelations *int64 `json:"unresolved_relations"`
}

// Equal compares two count records by value, resolution included.
func (counts ScanCounts) Equal(other ScanCounts) bool {
	return counts.rowCounts() == other.rowCounts() &&
		equalOptional(counts.ResolvedRelations, other.ResolvedRelations) &&
		equalOptional(counts.UnresolvedRelations, other.UnresolvedRelations)
}

// rowCounts is every count a generation always knows.
type rowCounts struct {
	sessions    int64
	events      int64
	evidence    int64
	passages    int64
	projections int64
	limitations int64
	relations   int64
}

func (counts ScanCounts) rowCounts() rowCounts {
	return rowCounts{
		sessions:    counts.Sessions,
		events:      counts.Events,
		evidence:    counts.Evidence,
		passages:    counts.Passages,
		projections: counts.Projections,
		limitations: counts.Limitations,
		relations:   counts.Relations,
	}
}

func equalOptional(left *int64, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

// BuildFacts describe how a candidate generation was produced.
type BuildFacts struct {
	Sources         []string
	BuilderVersions map[string]string
	Counts          ScanCounts
}

// DerivedWriter writes the shared immutable derived rows of one builder-version
// set. Identifiers are reserved from the catalog sequences, so every table is
// filled by one COPY per session and no insert learns its own row identity.
type DerivedWriter struct {
	catalog    *Catalog
	builderKey string
	sessions   int64
	rows       int64
}

// NewDerivedWriter opens a writer for one builder-version set.
func (catalog *Catalog) NewDerivedWriter(builderKey string) *DerivedWriter {
	return &DerivedWriter{catalog: catalog, builderKey: builderKey}
}

// SessionsWritten counts the sessions whose derived rows this scan built.
func (writer *DerivedWriter) SessionsWritten() int64 {
	return writer.sessions
}

// RowsWritten counts every derived row this scan built. A scan that reuses
// every session leaves it at zero.
func (writer *DerivedWriter) RowsWritten() int64 {
	return writer.rows
}

// WriteSession builds one session's derived rows in one transaction and
// returns the derived session they belong to.
func (writer *DerivedWriter) WriteSession(
	ctx context.Context,
	session ScanSession,
) (derived int64, err error) {
	pool, err := writer.catalog.acquire(ctx)
	if err != nil {
		return 0, err
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin session retention: %w", err)
	}
	defer func() {
		err = errors.Join(err, discard(ctx, transaction))
	}()
	if err := liftStatementTimeout(ctx, transaction); err != nil {
		return 0, err
	}
	if err := writer.catalog.lockMaintenance(ctx, transaction); err != nil {
		return 0, err
	}
	bases, err := writer.catalog.reserve(ctx, transaction, sessionSize(session))
	if err != nil {
		return 0, err
	}
	plan := buildPlan(session, writer.builderKey, bases)
	if err := plan.copy(ctx, transaction, writer.catalog); err != nil {
		return 0, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit session retention: %w", err)
	}
	writer.sessions++
	writer.rows += plan.rows()
	return plan.session.id, nil
}

// lockMaintenance serializes identifier reservation against another writer.
func (catalog *Catalog) lockMaintenance(
	ctx context.Context,
	transaction pgx.Tx,
) error {
	if _, err := transaction.Exec(
		ctx,
		"SELECT pg_advisory_xact_lock(hashtext($1), $2)",
		"sessionio:"+catalog.settings.SchemaName,
		int32(advisoryLockKey),
	); err != nil {
		return fmt.Errorf("lock catalog maintenance: %w", err)
	}
	return nil
}

// idBases are the first identifiers reserved for one session in each table.
type idBases struct {
	session  int64
	event    int64
	evidence int64
	relation int64
	passage  int64
	document int64
}

// idSizes are how many identifiers one session needs in each table.
type idSizes struct {
	event    int64
	evidence int64
	relation int64
	passage  int64
	document int64
}

func sessionSize(session ScanSession) idSizes {
	sizes := idSizes{
		event:    int64(len(session.Events)),
		relation: int64(len(session.Relations)),
		passage:  int64(len(session.Passages)),
		document: int64(len(session.Passages)),
	}
	for _, event := range session.Events {
		sizes.evidence += int64(len(event.Evidence))
	}
	return sizes
}

// reserveExpression allocates one contiguous identifier block and returns its
// first value. An empty block leaves the sequence untouched.
const reserveExpression = "CASE WHEN $%d > 0 THEN" +
	" setval('%s', nextval('%s') + $%d - 1, true) - $%d + 1 ELSE 0 END"

func (catalog *Catalog) reserve(
	ctx context.Context,
	transaction pgx.Tx,
	sizes idSizes,
) (idBases, error) {
	tables := []string{
		tableDerivedSession,
		tableDerivedEvent,
		tableDerivedEvidence,
		tableDerivedRelation,
		tableDerivedPassage,
		tableSearchDocument,
	}
	arguments := []any{
		int64(1),
		sizes.event,
		sizes.evidence,
		sizes.relation,
		sizes.passage,
		sizes.document,
	}
	expressions := make([]string, 0, len(tables))
	for index, table := range tables {
		sequence := catalog.settings.SchemaName + "." + derivedSequences[table]
		expressions = append(expressions, fmt.Sprintf(
			reserveExpression,
			index+1,
			sequence,
			sequence,
			index+1,
			index+1,
		))
	}
	var bases idBases
	if err := transaction.QueryRow(
		ctx,
		"SELECT "+joinComma(expressions),
		arguments...,
	).Scan(
		&bases.session,
		&bases.event,
		&bases.evidence,
		&bases.relation,
		&bases.passage,
		&bases.document,
	); err != nil {
		return idBases{}, fmt.Errorf("reserve derived identifiers: %w", err)
	}
	return bases, nil
}

func joinComma(parts []string) string {
	joined := ""
	for index, part := range parts {
		if index > 0 {
			joined += ", "
		}
		joined += part
	}
	return joined
}

type sessionRow struct {
	id                  int64
	revisionHash        []byte
	builderKey          string
	key                 string
	harness             string
	nativeID            string
	title               string
	sourceID            string
	occurrenceID        string
	discoveryRevision   string
	sourceRevisionKind  string
	sourceRevisionValue string
	locator             Locator
	startedAt           *time.Time
	updatedAt           *time.Time
}

type eventRow struct {
	id          int64
	derivedID   int64
	ordinal     int32
	key         string
	nativeKey   string
	kind        string
	role        string
	observation string
	occurredAt  *time.Time
}

type evidenceRow struct {
	id          int64
	eventID     int64
	position    int32
	observation string
	locator     Locator
}

type passageRow struct {
	id             int64
	derivedID      int64
	ordinal        int32
	kind           string
	builderVersion string
	part           int32
	parts          int32
	occurredAt     *time.Time
}

type relationRow struct {
	id        int64
	derivedID int64
	ordinal   int32
	relation  ScanRelation
}

type passageEventRow struct {
	passageID int64
	eventID   int64
	position  int32
}

type documentRow struct {
	id                int64
	derivedID         int64
	sessionRef        string
	harness           string
	passageID         int64
	projectionKind    string
	projectionVersion string
	body              string
	contentHash       []byte
}

type facetRow struct {
	documentID int64
	derivedID  int64
	facet      FacetFilter
}

type limitationRow struct {
	documentID int64
	limitation ProjectionLimitation
}

type sessionPlan struct {
	session       sessionRow
	events        []eventRow
	evidence      []evidenceRow
	passages      []passageRow
	passageEvents []passageEventRow
	documents     []documentRow
	limitations   []limitationRow
	facets        []facetRow
	relations     []relationRow
}

func (plan sessionPlan) rows() int64 {
	return 1 +
		int64(len(plan.events)) +
		int64(len(plan.evidence)) +
		int64(len(plan.passages)) +
		int64(len(plan.passageEvents)) +
		int64(len(plan.documents)) +
		int64(len(plan.limitations)) +
		int64(len(plan.facets)) +
		int64(len(plan.relations))
}

// buildPlan assigns every identifier up front from the reserved blocks, so
// foreign keys are known before the first COPY.
func buildPlan(
	session ScanSession,
	builderKey string,
	bases idBases,
) sessionPlan {
	plan := sessionPlan{session: sessionRow{
		id:                  bases.session,
		revisionHash:        session.RevisionHash,
		builderKey:          builderKey,
		key:                 session.Key,
		harness:             session.Harness,
		nativeID:            session.NativeID,
		title:               session.Title,
		sourceID:            session.SourceID,
		occurrenceID:        session.OccurrenceID,
		discoveryRevision:   session.DiscoveryRevision,
		sourceRevisionKind:  session.SourceRevisionKind,
		sourceRevisionValue: session.SourceRevisionValue,
		locator:             session.Locator,
		startedAt:           session.StartedAt,
		updatedAt:           session.UpdatedAt,
	}}
	eventIDs := make([]int64, len(session.Events))
	nextEvidence := bases.evidence
	for index, event := range session.Events {
		eventID := bases.event + int64(index)
		eventIDs[index] = eventID
		plan.events = append(plan.events, eventRow{
			id:          eventID,
			derivedID:   plan.session.id,
			ordinal:     int32(index),
			key:         event.Key,
			nativeKey:   event.NativeKey,
			kind:        event.Kind,
			role:        event.Role,
			observation: event.Observation,
			occurredAt:  event.OccurredAt,
		})
		for position, evidence := range event.Evidence {
			plan.evidence = append(plan.evidence, evidenceRow{
				id:          nextEvidence,
				eventID:     eventID,
				position:    int32(position),
				observation: evidence.Observation,
				locator:     evidence.Locator,
			})
			nextEvidence++
		}
	}
	for index, relation := range session.Relations {
		plan.relations = append(plan.relations, relationRow{
			id:        bases.relation + int64(index),
			derivedID: plan.session.id,
			ordinal:   int32(index),
			relation:  relation,
		})
	}
	for index, passage := range session.Passages {
		passageID := bases.passage + int64(index)
		documentID := bases.document + int64(index)
		plan.passages = append(plan.passages, passageRow{
			id:             passageID,
			derivedID:      plan.session.id,
			ordinal:        int32(index),
			kind:           passage.Kind,
			builderVersion: passage.BuilderVersion,
			part:           int32(passage.Part),
			parts:          int32(passage.Parts),
			occurredAt:     passage.OccurredAt,
		})
		for position, eventIndex := range passage.Events {
			plan.passageEvents = append(plan.passageEvents, passageEventRow{
				passageID: passageID,
				eventID:   eventIDs[eventIndex],
				position:  int32(position),
			})
		}
		plan.documents = append(plan.documents, documentRow{
			id:                documentID,
			derivedID:         plan.session.id,
			sessionRef:        session.Key,
			harness:           session.Harness,
			passageID:         passageID,
			projectionKind:    passage.ProjectionKind,
			projectionVersion: passage.ProjectionVersion,
			body:              passage.Body,
			contentHash:       passage.ContentHash,
		})
		for _, limitation := range passage.Limitations {
			plan.limitations = append(plan.limitations, limitationRow{
				documentID: documentID,
				limitation: limitation,
			})
		}
		for _, facet := range passage.Facets {
			plan.facets = append(plan.facets, facetRow{
				documentID: documentID,
				derivedID:  plan.session.id,
				facet:      facet,
			})
		}
	}
	return plan
}

func (plan sessionPlan) copy(
	ctx context.Context,
	transaction pgx.Tx,
	catalog *Catalog,
) error {
	schema := catalog.settings.SchemaName
	copies := []struct {
		table   string
		columns []string
		source  pgx.CopyFromSource
	}{
		{
			table: tableDerivedSession,
			columns: []string{
				"id", "revision_hash", "builder_key", "session_key", "harness",
				"native_id", "title", "source_id", "occurrence_id",
				"discovery_revision", "source_revision_kind",
				"source_revision_value", "locator_kind", "locator_root",
				"locator_path", "started_at", "updated_at",
			},
			source: pgx.CopyFromSlice(1, func(int) ([]any, error) {
				row := plan.session
				return []any{
					row.id, row.revisionHash, row.builderKey, row.key,
					row.harness, row.nativeID, row.title, row.sourceID,
					row.occurrenceID, row.discoveryRevision,
					row.sourceRevisionKind, row.sourceRevisionValue,
					row.locator.Kind, row.locator.Root, row.locator.Path,
					row.startedAt, row.updatedAt,
				}, nil
			}),
		},
		{
			table: tableDerivedRelation,
			columns: []string{
				"id", "derived_id", "ordinal", "kind", "origin",
				"from_kind", "from_ref", "to_kind", "to_ref", "observation_id",
			},
			source: pgx.CopyFromSlice(
				len(plan.relations),
				func(index int) ([]any, error) {
					row := plan.relations[index]
					return []any{
						row.id, row.derivedID, row.ordinal,
						row.relation.Kind, row.relation.Origin,
						row.relation.FromKind, row.relation.FromRef,
						row.relation.ToKind, row.relation.ToRef,
						row.relation.Observation,
					}, nil
				},
			),
		},
		{
			table: tableDerivedEvent,
			columns: []string{
				"id", "derived_id", "ordinal", "event_key", "native_key",
				"kind", "role", "observation_id", "occurred_at",
			},
			source: pgx.CopyFromSlice(
				len(plan.events),
				func(index int) ([]any, error) {
					row := plan.events[index]
					return []any{
						row.id, row.derivedID, row.ordinal, row.key,
						row.nativeKey, row.kind, row.role, row.observation,
						row.occurredAt,
					}, nil
				},
			),
		},
		{
			table: tableDerivedEvidence,
			columns: []string{
				"id", "event_id", "position", "observation_id",
				"locator_kind", "locator_root", "locator_path",
				"locator_record", "locator_line", "byte_start", "byte_end",
			},
			source: pgx.CopyFromSlice(
				len(plan.evidence),
				func(index int) ([]any, error) {
					row := plan.evidence[index]
					return []any{
						row.id, row.eventID, row.position, row.observation,
						row.locator.Kind, row.locator.Root, row.locator.Path,
						row.locator.Record, row.locator.Line,
						row.locator.ByteStart, row.locator.ByteEnd,
					}, nil
				},
			),
		},
		{
			table: tableDerivedPassage,
			columns: []string{
				"id", "derived_id", "ordinal", "kind", "builder_version",
				"part", "parts", "started_at",
			},
			source: pgx.CopyFromSlice(
				len(plan.passages),
				func(index int) ([]any, error) {
					row := plan.passages[index]
					return []any{
						row.id, row.derivedID, row.ordinal, row.kind,
						row.builderVersion, row.part, row.parts, row.occurredAt,
					}, nil
				},
			),
		},
		{
			table:   tableDerivedPassageEvent,
			columns: []string{"passage_id", "event_id", "position"},
			source: pgx.CopyFromSlice(
				len(plan.passageEvents),
				func(index int) ([]any, error) {
					row := plan.passageEvents[index]
					return []any{row.passageID, row.eventID, row.position}, nil
				},
			),
		},
		{
			table: tableSearchDocument,
			columns: []string{
				"doc_id", "derived_id", "session_ref", "harness", "passage_id",
				"projection_kind", "projection_version", "body", "content_hash",
			},
			source: pgx.CopyFromSlice(
				len(plan.documents),
				func(index int) ([]any, error) {
					row := plan.documents[index]
					return []any{
						row.id, row.derivedID, row.sessionRef, row.harness,
						row.passageID, row.projectionKind,
						row.projectionVersion, row.body, row.contentHash,
					}, nil
				},
			),
		},
		{
			table:   tableProjectionLimit,
			columns: []string{"doc_id", "kind", "removed_bytes"},
			source: pgx.CopyFromSlice(
				len(plan.limitations),
				func(index int) ([]any, error) {
					row := plan.limitations[index]
					return []any{
						row.documentID,
						row.limitation.Kind,
						row.limitation.RemovedBytes,
					}, nil
				},
			),
		},
		{
			table:   tableSearchFacet,
			columns: []string{"doc_id", "derived_id", "namespace", "key", "value"},
			source: pgx.CopyFromSlice(
				len(plan.facets),
				func(index int) ([]any, error) {
					row := plan.facets[index]
					return []any{
						row.documentID,
						row.derivedID,
						row.facet.Namespace,
						row.facet.Key,
						row.facet.Value,
					}, nil
				},
			),
		},
	}
	for _, batch := range copies {
		if _, err := transaction.CopyFrom(
			ctx,
			pgx.Identifier{schema, batch.table},
			batch.columns,
			batch.source,
		); err != nil {
			return fmt.Errorf("retain rows into %s: %w", batch.table, err)
		}
	}
	return nil
}

// RecordBuild stores how a candidate generation was produced.
func (catalog *Catalog) RecordBuild(
	ctx context.Context,
	generation GenerationID,
	facts BuildFacts,
) error {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return err
	}
	sources, err := json.Marshal(facts.Sources)
	if err != nil {
		return fmt.Errorf("encode generation source set: %w", err)
	}
	versions, err := json.Marshal(facts.BuilderVersions)
	if err != nil {
		return fmt.Errorf("encode generation builder versions: %w", err)
	}
	counts, err := json.Marshal(facts.Counts)
	if err != nil {
		return fmt.Errorf("encode generation counts: %w", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		"UPDATE %s.generation SET source_set = $1, builder_versions = $2,"+
			" counts = $3 WHERE id = $4",
		catalog.schema,
	), sources, versions, counts, generation); err != nil {
		return fmt.Errorf("record generation build facts: %w", err)
	}
	return nil
}

// MarkFailed makes an abandoned candidate generation reclaimable.
func (catalog *Catalog) MarkFailed(
	ctx context.Context,
	generation GenerationID,
) error {
	return catalog.markFailed(ctx, generation)
}

// Reclaim releases every failed or superseded generation and the derived rows
// no live generation still presents.
func (catalog *Catalog) Reclaim(ctx context.Context) (int, error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return 0, err
	}
	rows, err := pool.Query(ctx, fmt.Sprintf(
		"SELECT id FROM %s.generation"+
			" WHERE state = ANY($1) AND reclaimed_at IS NULL ORDER BY id",
		catalog.schema,
	), []string{StateFailed, StateSuperseded})
	if err != nil {
		return 0, fmt.Errorf("list reclaimable generations: %w", err)
	}
	var reclaimable []GenerationID
	for rows.Next() {
		var generation GenerationID
		if err := rows.Scan(&generation); err != nil {
			rows.Close()
			return 0, fmt.Errorf("read reclaimable generation: %w", err)
		}
		reclaimable = append(reclaimable, generation)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read reclaimable generation: %w", err)
	}
	dropped := 0
	for _, generation := range reclaimable {
		if err := catalog.Cleanup(ctx, generation); err != nil {
			return dropped, err
		}
		dropped++
	}
	if dropped == 0 {
		return 0, nil
	}
	// Settling once per reclaim rather than once per generation also makes the
	// step self-healing: a vacuum an older snapshot held back is retried by the
	// next scan instead of leaving reclaimed documents in the statistics.
	return dropped, catalog.settleProjections(ctx)
}

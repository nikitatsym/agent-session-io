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
	Key         string
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

// ScanRelation is one retained structural relation. A target outside this
// session stays unresolved until ResolveRelations reads the retained revisions.
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

// ScanSession is one reader session retained into a candidate generation.
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

// ScanCounts are the retained row counts of one candidate generation.
type ScanCounts struct {
	Sessions            int64 `json:"sessions"`
	Events              int64 `json:"events"`
	Evidence            int64 `json:"evidence"`
	Passages            int64 `json:"passages"`
	Projections         int64 `json:"projections"`
	Limitations         int64 `json:"projection_limitations"`
	Relations           int64 `json:"relations"`
	ResolvedRelations   int64 `json:"resolved_relations"`
	UnresolvedRelations int64 `json:"unresolved_relations"`
}

// BuildFacts describe how a candidate generation was produced.
type BuildFacts struct {
	Sources         []string
	BuilderVersions map[string]string
	Counts          ScanCounts
}

// GenerationWriter fills exactly one candidate generation. Identifiers are
// assigned by the writer, so every table is filled by one COPY per session.
type GenerationWriter struct {
	catalog      *Catalog
	generation   GenerationID
	nextSession  int64
	nextEvent    int64
	nextEvidence int64
	nextPassage  int64
	nextDocument int64
	nextRelation int64
	counts       ScanCounts
}

func (catalog *Catalog) NewGenerationWriter(
	generation GenerationID,
) *GenerationWriter {
	return &GenerationWriter{catalog: catalog, generation: generation}
}

// Counts reports the rows written so far.
func (writer *GenerationWriter) Counts() ScanCounts {
	return writer.counts
}

// WriteSession retains one session and its derived rows in one transaction.
func (writer *GenerationWriter) WriteSession(
	ctx context.Context,
	session ScanSession,
) (err error) {
	pool, err := writer.catalog.acquire(ctx)
	if err != nil {
		return err
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin session retention: %w", err)
	}
	defer func() {
		err = errors.Join(err, discard(ctx, transaction))
	}()
	if err := liftStatementTimeout(ctx, transaction); err != nil {
		return err
	}
	plan := writer.plan(session)
	if err := plan.copy(
		ctx,
		transaction,
		writer.catalog,
		writer.generation,
	); err != nil {
		return err
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("commit session retention: %w", err)
	}
	writer.commitCounts(plan)
	return nil
}

func (writer *GenerationWriter) commitCounts(plan sessionPlan) {
	writer.counts.Sessions++
	writer.counts.Events += int64(len(plan.events))
	writer.counts.Evidence += int64(len(plan.evidence))
	writer.counts.Passages += int64(len(plan.passages))
	writer.counts.Projections += int64(len(plan.documents))
	writer.counts.Limitations += int64(len(plan.limitations))
	writer.counts.Relations += int64(len(plan.relations))
}

type sessionRow struct {
	id                  int64
	key                 string
	harness             string
	nativeID            string
	title               string
	sourceID            string
	occurrenceID        string
	discoveryRevision   string
	revisionHash        []byte
	sourceRevisionKind  string
	sourceRevisionValue string
	locator             Locator
	startedAt           *time.Time
	updatedAt           *time.Time
}

type eventRow struct {
	id          int64
	sessionID   int64
	ordinal     int32
	key         string
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
	sessionID      int64
	ordinal        int32
	kind           string
	builderVersion string
	part           int32
	parts          int32
	occurredAt     *time.Time
}

type relationRow struct {
	id        int64
	sessionID int64
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

// plan assigns every identifier up front, so foreign keys are known before the
// first COPY and no insert needs a round trip to learn its own row identity.
func (writer *GenerationWriter) plan(session ScanSession) sessionPlan {
	writer.nextSession++
	plan := sessionPlan{session: sessionRow{
		id:                  writer.nextSession,
		key:                 session.Key,
		harness:             session.Harness,
		nativeID:            session.NativeID,
		title:               session.Title,
		sourceID:            session.SourceID,
		occurrenceID:        session.OccurrenceID,
		discoveryRevision:   session.DiscoveryRevision,
		revisionHash:        session.RevisionHash,
		sourceRevisionKind:  session.SourceRevisionKind,
		sourceRevisionValue: session.SourceRevisionValue,
		locator:             session.Locator,
		startedAt:           session.StartedAt,
		updatedAt:           session.UpdatedAt,
	}}
	eventIDs := make([]int64, len(session.Events))
	for index, event := range session.Events {
		writer.nextEvent++
		eventIDs[index] = writer.nextEvent
		plan.events = append(plan.events, eventRow{
			id:          writer.nextEvent,
			sessionID:   plan.session.id,
			ordinal:     int32(index),
			key:         event.Key,
			kind:        event.Kind,
			role:        event.Role,
			observation: event.Observation,
			occurredAt:  event.OccurredAt,
		})
		for position, evidence := range event.Evidence {
			writer.nextEvidence++
			plan.evidence = append(plan.evidence, evidenceRow{
				id:          writer.nextEvidence,
				eventID:     writer.nextEvent,
				position:    int32(position),
				observation: evidence.Observation,
				locator:     evidence.Locator,
			})
		}
	}
	for index, relation := range session.Relations {
		writer.nextRelation++
		plan.relations = append(plan.relations, relationRow{
			id:        writer.nextRelation,
			sessionID: plan.session.id,
			ordinal:   int32(index),
			relation:  relation,
		})
	}
	for index, passage := range session.Passages {
		writer.nextPassage++
		writer.nextDocument++
		plan.passages = append(plan.passages, passageRow{
			id:             writer.nextPassage,
			sessionID:      plan.session.id,
			ordinal:        int32(index),
			kind:           passage.Kind,
			builderVersion: passage.BuilderVersion,
			part:           int32(passage.Part),
			parts:          int32(passage.Parts),
			occurredAt:     passage.OccurredAt,
		})
		for position, eventIndex := range passage.Events {
			plan.passageEvents = append(plan.passageEvents, passageEventRow{
				passageID: writer.nextPassage,
				eventID:   eventIDs[eventIndex],
				position:  int32(position),
			})
		}
		plan.documents = append(plan.documents, documentRow{
			id:                writer.nextDocument,
			sessionRef:        session.Key,
			harness:           session.Harness,
			passageID:         writer.nextPassage,
			projectionKind:    passage.ProjectionKind,
			projectionVersion: passage.ProjectionVersion,
			body:              passage.Body,
			contentHash:       passage.ContentHash,
		})
		for _, limitation := range passage.Limitations {
			plan.limitations = append(plan.limitations, limitationRow{
				documentID: writer.nextDocument,
				limitation: limitation,
			})
		}
		for _, facet := range passage.Facets {
			plan.facets = append(plan.facets, facetRow{
				documentID: writer.nextDocument,
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
	generation GenerationID,
) error {
	schema := catalog.settings.SchemaName
	copies := []struct {
		table   string
		columns []string
		source  pgx.CopyFromSource
	}{
		{
			table: sessionTable(generation),
			columns: []string{
				"id", "session_key", "harness", "native_id", "title",
				"source_id", "occurrence_id", "discovery_revision",
				"revision_hash", "source_revision_kind", "source_revision_value",
				"locator_kind", "locator_root", "locator_path",
				"started_at", "updated_at",
			},
			source: pgx.CopyFromSlice(1, func(int) ([]any, error) {
				row := plan.session
				return []any{
					row.id, row.key, row.harness, row.nativeID, row.title,
					row.sourceID, row.occurrenceID, row.discoveryRevision,
					row.revisionHash, row.sourceRevisionKind,
					row.sourceRevisionValue,
					row.locator.Kind, row.locator.Root, row.locator.Path,
					row.startedAt, row.updatedAt,
				}, nil
			}),
		},
		{
			table: relationTable(generation),
			columns: []string{
				"id", "session_id", "ordinal", "kind", "origin",
				"from_kind", "from_ref", "to_kind", "to_ref", "observation_id",
			},
			source: pgx.CopyFromSlice(
				len(plan.relations),
				func(index int) ([]any, error) {
					row := plan.relations[index]
					return []any{
						row.id, row.sessionID, row.ordinal,
						row.relation.Kind, row.relation.Origin,
						row.relation.FromKind, row.relation.FromRef,
						row.relation.ToKind, row.relation.ToRef,
						row.relation.Observation,
					}, nil
				},
			),
		},
		{
			table: eventTable(generation),
			columns: []string{
				"id", "session_id", "ordinal", "event_key", "kind", "role",
				"observation_id", "occurred_at",
			},
			source: pgx.CopyFromSlice(
				len(plan.events),
				func(index int) ([]any, error) {
					row := plan.events[index]
					return []any{
						row.id, row.sessionID, row.ordinal, row.key, row.kind,
						row.role, row.observation, row.occurredAt,
					}, nil
				},
			),
		},
		{
			table: evidenceTable(generation),
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
			table: passageTable(generation),
			columns: []string{
				"id", "session_id", "ordinal", "kind", "builder_version",
				"part", "parts", "started_at",
			},
			source: pgx.CopyFromSlice(
				len(plan.passages),
				func(index int) ([]any, error) {
					row := plan.passages[index]
					return []any{
						row.id, row.sessionID, row.ordinal, row.kind,
						row.builderVersion, row.part, row.parts, row.occurredAt,
					}, nil
				},
			),
		},
		{
			table:   passageEventTable(generation),
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
			table: documentTable(generation),
			columns: []string{
				"doc_id", "session_ref", "harness", "passage_id",
				"projection_kind", "projection_version", "body", "content_hash",
			},
			source: pgx.CopyFromSlice(
				len(plan.documents),
				func(index int) ([]any, error) {
					row := plan.documents[index]
					return []any{
						row.id, row.sessionRef, row.harness, row.passageID,
						row.projectionKind, row.projectionVersion, row.body,
						row.contentHash,
					}, nil
				},
			),
		},
		{
			table:   limitationTable(generation),
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
			table:   facetTable(generation),
			columns: []string{"doc_id", "namespace", "key", "value"},
			source: pgx.CopyFromSlice(
				len(plan.facets),
				func(index int) ([]any, error) {
					row := plan.facets[index]
					return []any{
						row.documentID,
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

// Reclaim drops every failed or superseded generation. A generation still held
// by a reader is left in place and counted as retained.
func (catalog *Catalog) Reclaim(
	ctx context.Context,
) (dropped int, retained int, err error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return 0, 0, err
	}
	rows, err := pool.Query(ctx, fmt.Sprintf(
		"SELECT id FROM %s.generation WHERE state = ANY($1) ORDER BY id",
		catalog.schema,
	), []string{StateFailed, StateSuperseded})
	if err != nil {
		return 0, 0, fmt.Errorf("list reclaimable generations: %w", err)
	}
	var reclaimable []GenerationID
	for rows.Next() {
		var generation GenerationID
		if err := rows.Scan(&generation); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("read reclaimable generation: %w", err)
		}
		reclaimable = append(reclaimable, generation)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("read reclaimable generation: %w", err)
	}
	for _, generation := range reclaimable {
		err := catalog.Cleanup(ctx, generation)
		if errors.Is(err, ErrCleanupBusy) {
			retained++
			continue
		}
		if err != nil {
			return dropped, retained, err
		}
		dropped++
	}
	return dropped, retained, nil
}

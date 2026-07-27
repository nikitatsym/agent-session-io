package catalog

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// idRange is the identifier span one session occupies in one generation table.
// The writer allocates a session's rows consecutively, so a copy only shifts
// the whole span by a constant.
type idRange struct {
	low  int64
	high int64
}

func (span idRange) empty() bool {
	return span.high < span.low
}

// sessionSpans locates every row span of one session in a source generation.
type sessionSpans struct {
	sessionID int64
	events    idRange
	evidence  idRange
	passages  idRange
	documents idRange
	relations idRange
}

// CopySession reuses an unchanged session by copying its retained rows from
// another generation instead of rereading and reparsing its transcript.
func (writer *GenerationWriter) CopySession(
	ctx context.Context,
	source GenerationID,
	sessionKey string,
) (copied bool, err error) {
	spans, found, err := writer.catalog.sessionSpans(ctx, source, sessionKey)
	if err != nil || !found {
		return false, err
	}
	pool, err := writer.catalog.acquire(ctx)
	if err != nil {
		return false, err
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin session reuse: %w", err)
	}
	defer func() {
		err = errors.Join(err, discard(ctx, transaction))
	}()
	if err := liftStatementTimeout(ctx, transaction); err != nil {
		return false, err
	}
	writer.nextSession++
	shifts := writer.shifts(spans)
	for _, statement := range copyStatements(
		writer.catalog.schema,
		source,
		writer.generation,
		spans,
		shifts,
		writer.nextSession,
	) {
		if _, err := transaction.Exec(ctx, statement.sql, statement.arguments...); err != nil {
			return false, fmt.Errorf("reuse retained rows: %w", err)
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit session reuse: %w", err)
	}
	writer.advance(spans, shifts)
	return true, nil
}

// shifts are the constants added to every copied identifier.
type idShifts struct {
	events    int64
	evidence  int64
	passages  int64
	documents int64
	relations int64
}

func (writer *GenerationWriter) shifts(spans sessionSpans) idShifts {
	return idShifts{
		events:    writer.nextEvent + 1 - spans.events.low,
		evidence:  writer.nextEvidence + 1 - spans.evidence.low,
		passages:  writer.nextPassage + 1 - spans.passages.low,
		documents: writer.nextDocument + 1 - spans.documents.low,
		relations: writer.nextRelation + 1 - spans.relations.low,
	}
}

func (writer *GenerationWriter) advance(spans sessionSpans, shifts idShifts) {
	writer.counts.Sessions++
	if !spans.events.empty() {
		writer.nextEvent = spans.events.high + shifts.events
		writer.counts.Events += spans.events.high - spans.events.low + 1
	}
	if !spans.evidence.empty() {
		writer.nextEvidence = spans.evidence.high + shifts.evidence
		writer.counts.Evidence += spans.evidence.high - spans.evidence.low + 1
	}
	if !spans.passages.empty() {
		writer.nextPassage = spans.passages.high + shifts.passages
		writer.counts.Passages += spans.passages.high - spans.passages.low + 1
	}
	if !spans.documents.empty() {
		writer.nextDocument = spans.documents.high + shifts.documents
		writer.counts.Projections += spans.documents.high - spans.documents.low + 1
	}
	if !spans.relations.empty() {
		writer.nextRelation = spans.relations.high + shifts.relations
		writer.counts.Relations += spans.relations.high - spans.relations.low + 1
	}
}

type copyStatement struct {
	sql       string
	arguments []any
}

func copyStatements(
	schema string,
	source GenerationID,
	target GenerationID,
	spans sessionSpans,
	shifts idShifts,
	sessionID int64,
) []copyStatement {
	table := func(name func(GenerationID) string, generation GenerationID) string {
		return schema + "." + quoteIdentifier(name(generation))
	}
	return []copyStatement{
		{
			sql: fmt.Sprintf(
				"INSERT INTO %s (id, session_key, harness, native_id, title,"+
					" source_id, occurrence_id, discovery_revision, revision_hash,"+
					" source_revision_kind, source_revision_value, locator_kind,"+
					" locator_root, locator_path, started_at, updated_at)"+
					" SELECT $1, session_key, harness, native_id, title,"+
					" source_id, occurrence_id, discovery_revision, revision_hash,"+
					" source_revision_kind, source_revision_value, locator_kind,"+
					" locator_root, locator_path, started_at, updated_at"+
					" FROM %s WHERE id = $2",
				table(sessionTable, target),
				table(sessionTable, source),
			),
			arguments: []any{sessionID, spans.sessionID},
		},
		{
			sql: fmt.Sprintf(
				"INSERT INTO %s (id, session_id, ordinal, event_key, kind, role,"+
					" observation_id, occurred_at)"+
					" SELECT id + $1, $2, ordinal, event_key, kind, role,"+
					" observation_id, occurred_at FROM %s WHERE session_id = $3",
				table(eventTable, target),
				table(eventTable, source),
			),
			arguments: []any{shifts.events, sessionID, spans.sessionID},
		},
		{
			sql: fmt.Sprintf(
				"INSERT INTO %s (id, session_id, ordinal, kind, origin,"+
					" from_kind, from_ref, to_kind, to_ref, observation_id)"+
					" SELECT id + $1, $2, ordinal, kind, origin,"+
					" from_kind, from_ref, to_kind, to_ref, observation_id"+
					" FROM %s WHERE session_id = $3",
				table(relationTable, target),
				table(relationTable, source),
			),
			arguments: []any{shifts.relations, sessionID, spans.sessionID},
		},
		{
			sql: fmt.Sprintf(
				"INSERT INTO %s (id, event_id, position, observation_id,"+
					" locator_kind, locator_root, locator_path, locator_record,"+
					" locator_line, byte_start, byte_end)"+
					" SELECT evidence.id + $1, evidence.event_id + $2,"+
					" evidence.position, evidence.observation_id,"+
					" evidence.locator_kind, evidence.locator_root,"+
					" evidence.locator_path, evidence.locator_record,"+
					" evidence.locator_line, evidence.byte_start, evidence.byte_end"+
					" FROM %s evidence JOIN %s event"+
					" ON event.id = evidence.event_id WHERE event.session_id = $3",
				table(evidenceTable, target),
				table(evidenceTable, source),
				table(eventTable, source),
			),
			arguments: []any{shifts.evidence, shifts.events, spans.sessionID},
		},
		{
			sql: fmt.Sprintf(
				"INSERT INTO %s (id, session_id, ordinal, kind, builder_version,"+
					" part, parts, started_at)"+
					" SELECT id + $1, $2, ordinal, kind, builder_version,"+
					" part, parts, started_at FROM %s WHERE session_id = $3",
				table(passageTable, target),
				table(passageTable, source),
			),
			arguments: []any{shifts.passages, sessionID, spans.sessionID},
		},
		{
			sql: fmt.Sprintf(
				"INSERT INTO %s (passage_id, event_id, position)"+
					" SELECT link.passage_id + $1, link.event_id + $2, link.position"+
					" FROM %s link JOIN %s passage ON passage.id = link.passage_id"+
					" WHERE passage.session_id = $3",
				table(passageEventTable, target),
				table(passageEventTable, source),
				table(passageTable, source),
			),
			arguments: []any{shifts.passages, shifts.events, spans.sessionID},
		},
		{
			sql: fmt.Sprintf(
				"INSERT INTO %s (doc_id, session_ref, harness, passage_id,"+
					" projection_kind, projection_version, body, content_hash)"+
					" SELECT document.doc_id + $1, document.session_ref,"+
					" document.harness, document.passage_id + $2,"+
					" document.projection_kind, document.projection_version,"+
					" document.body, document.content_hash"+
					" FROM %s document JOIN %s passage"+
					" ON passage.id = document.passage_id"+
					" WHERE passage.session_id = $3",
				table(documentTable, target),
				table(documentTable, source),
				table(passageTable, source),
			),
			arguments: []any{shifts.documents, shifts.passages, spans.sessionID},
		},
		{
			sql: fmt.Sprintf(
				"INSERT INTO %s (doc_id, kind, removed_bytes)"+
					" SELECT limitation.doc_id + $1, limitation.kind,"+
					" limitation.removed_bytes FROM %s limitation"+
					" JOIN %s document ON document.doc_id = limitation.doc_id"+
					" JOIN %s passage ON passage.id = document.passage_id"+
					" WHERE passage.session_id = $2",
				table(limitationTable, target),
				table(limitationTable, source),
				table(documentTable, source),
				table(passageTable, source),
			),
			arguments: []any{shifts.documents, spans.sessionID},
		},
		{
			sql: fmt.Sprintf(
				`INSERT INTO %s (doc_id, namespace, "key", value)`+
					` SELECT facet.doc_id + $1, facet.namespace, facet."key",`+
					" facet.value FROM %s facet"+
					" JOIN %s document ON document.doc_id = facet.doc_id"+
					" JOIN %s passage ON passage.id = document.passage_id"+
					" WHERE passage.session_id = $2",
				table(facetTable, target),
				table(facetTable, source),
				table(documentTable, source),
				table(passageTable, source),
			),
			arguments: []any{shifts.documents, spans.sessionID},
		},
	}
}

const spansQuery = `SELECT
	(SELECT COALESCE(min(id), 1) FROM %[1]s WHERE session_id = $1),
	(SELECT COALESCE(max(id), 0) FROM %[1]s WHERE session_id = $1),
	(SELECT COALESCE(min(evidence.id), 1) FROM %[2]s evidence
		JOIN %[1]s event ON event.id = evidence.event_id
		WHERE event.session_id = $1),
	(SELECT COALESCE(max(evidence.id), 0) FROM %[2]s evidence
		JOIN %[1]s event ON event.id = evidence.event_id
		WHERE event.session_id = $1),
	(SELECT COALESCE(min(id), 1) FROM %[3]s WHERE session_id = $1),
	(SELECT COALESCE(max(id), 0) FROM %[3]s WHERE session_id = $1),
	(SELECT COALESCE(min(document.doc_id), 1) FROM %[4]s document
		JOIN %[3]s passage ON passage.id = document.passage_id
		WHERE passage.session_id = $1),
	(SELECT COALESCE(max(document.doc_id), 0) FROM %[4]s document
		JOIN %[3]s passage ON passage.id = document.passage_id
		WHERE passage.session_id = $1),
	(SELECT COALESCE(min(id), 1) FROM %[5]s WHERE session_id = $1),
	(SELECT COALESCE(max(id), 0) FROM %[5]s WHERE session_id = $1)`

func (catalog *Catalog) sessionSpans(
	ctx context.Context,
	generation GenerationID,
	sessionKey string,
) (sessionSpans, bool, error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return sessionSpans{}, false, err
	}
	var spans sessionSpans
	err = pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT id FROM %s.%s WHERE session_key = $1",
		catalog.schema,
		quoteIdentifier(sessionTable(generation)),
	), sessionKey).Scan(&spans.sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return sessionSpans{}, false, nil
	}
	if err != nil {
		return sessionSpans{}, false, fmt.Errorf("locate reusable session: %w", err)
	}
	statement := fmt.Sprintf(
		spansQuery,
		catalog.schema+"."+quoteIdentifier(eventTable(generation)),
		catalog.schema+"."+quoteIdentifier(evidenceTable(generation)),
		catalog.schema+"."+quoteIdentifier(passageTable(generation)),
		catalog.schema+"."+quoteIdentifier(documentTable(generation)),
		catalog.schema+"."+quoteIdentifier(relationTable(generation)),
	)
	if err := pool.QueryRow(ctx, statement, spans.sessionID).Scan(
		&spans.events.low,
		&spans.events.high,
		&spans.evidence.low,
		&spans.evidence.high,
		&spans.passages.low,
		&spans.passages.high,
		&spans.documents.low,
		&spans.documents.high,
		&spans.relations.low,
		&spans.relations.high,
	); err != nil {
		return sessionSpans{}, false, fmt.Errorf("measure reusable session: %w", err)
	}
	return spans, true, nil
}

// ResolveRelations connects every relation target to the revisions retained in
// the same generation. No transcript is reopened to resolve a peer.
func (catalog *Catalog) ResolveRelations(
	ctx context.Context,
	generation GenerationID,
) (resolved int64, unresolved int64, err error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return 0, 0, err
	}
	relation := catalog.schema + "." + quoteIdentifier(relationTable(generation))
	session := catalog.schema + "." + quoteIdentifier(sessionTable(generation))
	event := catalog.schema + "." + quoteIdentifier(eventTable(generation))
	statements := []string{
		fmt.Sprintf(
			"UPDATE %s relation SET resolved_kind = 'session',"+
				" resolved_ref = target.session_key FROM ("+
				" SELECT event.observation_id AS observation_id,"+
				" min(session.session_key) AS session_key"+
				" FROM %s event JOIN %s session ON session.id = event.session_id"+
				" GROUP BY event.observation_id"+
				" HAVING count(DISTINCT session.session_key) = 1) target"+
				" WHERE relation.to_kind = 'observation'"+
				" AND relation.to_ref = target.observation_id",
			relation,
			event,
			session,
		),
		fmt.Sprintf(
			"UPDATE %s relation SET resolved_kind = 'session',"+
				" resolved_ref = target.session_key FROM ("+
				" SELECT event.event_key AS event_key,"+
				" min(session.session_key) AS session_key"+
				" FROM %s event JOIN %s session ON session.id = event.session_id"+
				" GROUP BY event.event_key"+
				" HAVING count(DISTINCT session.session_key) = 1) target"+
				" WHERE relation.to_kind = 'event'"+
				" AND relation.to_ref = target.event_key",
			relation,
			event,
			session,
		),
		fmt.Sprintf(
			"UPDATE %s relation SET resolved_kind = 'session',"+
				" resolved_ref = target.session_key FROM ("+
				" SELECT native_id, min(session_key) AS session_key FROM %s"+
				" GROUP BY native_id HAVING count(*) = 1) target"+
				" WHERE relation.to_kind = '%s'"+
				" AND relation.to_ref = target.native_id",
			relation,
			session,
			ToKindSessionNative,
		),
		fmt.Sprintf(
			"UPDATE %s relation SET resolved_kind = 'session',"+
				" resolved_ref = session.session_key FROM %s session"+
				" WHERE relation.to_kind = 'session'"+
				" AND relation.to_ref = session.session_key",
			relation,
			session,
		),
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			return 0, 0, fmt.Errorf("resolve retained relations: %w", err)
		}
	}
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT count(*) FILTER (WHERE resolved_ref IS NOT NULL),"+
			" count(*) FILTER (WHERE resolved_ref IS NULL) FROM %s",
		relation,
	)).Scan(&resolved, &unresolved); err != nil {
		return 0, 0, fmt.Errorf("count resolved relations: %w", err)
	}
	return resolved, unresolved, nil
}

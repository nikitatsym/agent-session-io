package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// FindDerivedSession locates the retained derived rows of one session revision
// built by one builder-version set. A miss is what makes a scan rebuild.
func (catalog *Catalog) FindDerivedSession(
	ctx context.Context,
	revisionHash []byte,
	builderKey string,
) (int64, bool, error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return 0, false, err
	}
	var derived int64
	err = pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT id FROM %s WHERE revision_hash = $1 AND builder_key = $2",
		catalog.table(tableDerivedSession),
	), revisionHash, builderKey).Scan(&derived)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("locate retained derived session: %w", err)
	}
	return derived, true, nil
}

// AddGenerationMember records that a generation presents one derived session.
func (catalog *Catalog) AddGenerationMember(
	ctx context.Context,
	generation GenerationID,
	derived int64,
) error {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO %s.generation_member (generation_id, derived_id)"+
			" VALUES ($1, $2) ON CONFLICT DO NOTHING",
		catalog.schema,
	), generation, derived); err != nil {
		return fmt.Errorf("record generation membership: %w", err)
	}
	return nil
}

const generationCountsQuery = `WITH member AS (
	SELECT derived_id FROM %[1]s.generation_member WHERE generation_id = $1)
SELECT
	(SELECT count(*) FROM member),
	(SELECT count(*) FROM %[2]s event
		JOIN member ON member.derived_id = event.derived_id),
	(SELECT count(*) FROM %[3]s evidence
		JOIN %[2]s event ON event.id = evidence.event_id
		JOIN member ON member.derived_id = event.derived_id),
	(SELECT count(*) FROM %[4]s passage
		JOIN member ON member.derived_id = passage.derived_id),
	(SELECT count(*) FROM %[5]s document
		JOIN member ON member.derived_id = document.derived_id),
	(SELECT count(*) FROM %[6]s limitation
		JOIN %[5]s document ON document.doc_id = limitation.doc_id
		JOIN member ON member.derived_id = document.derived_id),
	(SELECT count(*) FROM %[7]s relation
		JOIN member ON member.derived_id = relation.derived_id)`

// GenerationCounts reports what one generation presents. Reuse writes no row,
// so the counts come from the membership rather than from the writer.
func (catalog *Catalog) GenerationCounts(
	ctx context.Context,
	generation GenerationID,
) (counts ScanCounts, err error) {
	err = catalog.maintenanceQuery(ctx, fmt.Sprintf(
		generationCountsQuery,
		catalog.schema,
		catalog.table(tableDerivedEvent),
		catalog.table(tableDerivedEvidence),
		catalog.table(tableDerivedPassage),
		catalog.table(tableSearchDocument),
		catalog.table(tableProjectionLimit),
		catalog.table(tableDerivedRelation),
	), []any{generation},
		&counts.Sessions,
		&counts.Events,
		&counts.Evidence,
		&counts.Passages,
		&counts.Projections,
		&counts.Limitations,
		&counts.Relations,
	)
	if err != nil {
		return ScanCounts{}, fmt.Errorf("count generation rows: %w", err)
	}
	return counts, nil
}

// deltaCountsQuery counts what one generation presents and its parent did not,
// minus what the parent presented and it does not. Every join is driven by the
// membership difference, so the cost scales with the sessions a refresh
// changed rather than with the corpus.
const deltaCountsQuery = `WITH added AS (
	SELECT derived_id FROM %[1]s.generation_member WHERE generation_id = $1
	EXCEPT SELECT derived_id FROM %[1]s.generation_member WHERE generation_id = $2),
removed AS (
	SELECT derived_id FROM %[1]s.generation_member WHERE generation_id = $2
	EXCEPT SELECT derived_id FROM %[1]s.generation_member WHERE generation_id = $1),
delta AS (
	SELECT derived_id, 1 AS sign FROM added
	UNION ALL SELECT derived_id, -1 FROM removed)
SELECT
	(SELECT count(*) FROM added),
	(SELECT count(*) FROM removed),
	(SELECT COALESCE(sum(sign), 0) FROM delta
		JOIN %[2]s event ON event.derived_id = delta.derived_id),
	(SELECT COALESCE(sum(sign), 0) FROM delta
		JOIN %[2]s event ON event.derived_id = delta.derived_id
		JOIN %[3]s evidence ON evidence.event_id = event.id),
	(SELECT COALESCE(sum(sign), 0) FROM delta
		JOIN %[4]s passage ON passage.derived_id = delta.derived_id),
	(SELECT COALESCE(sum(sign), 0) FROM delta
		JOIN %[5]s document ON document.derived_id = delta.derived_id),
	(SELECT COALESCE(sum(sign), 0) FROM delta
		JOIN %[5]s document ON document.derived_id = delta.derived_id
		JOIN %[6]s limitation ON limitation.doc_id = document.doc_id),
	(SELECT COALESCE(sum(sign), 0) FROM delta
		JOIN %[7]s relation ON relation.derived_id = delta.derived_id)`

// IncrementalCounts derives what a generation presents from what its parent
// presented plus the difference between their memberships. An unchanged
// membership keeps the parent's relation resolution; any difference leaves it
// uncomputed, because resolution is a property of the whole presented set.
func (catalog *Catalog) IncrementalCounts(
	ctx context.Context,
	generation GenerationID,
	parent GenerationID,
	inherited ScanCounts,
) (ScanCounts, error) {
	var delta ScanCounts
	var added, removed int64
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return ScanCounts{}, err
	}
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		deltaCountsQuery,
		catalog.schema,
		catalog.table(tableDerivedEvent),
		catalog.table(tableDerivedEvidence),
		catalog.table(tableDerivedPassage),
		catalog.table(tableSearchDocument),
		catalog.table(tableProjectionLimit),
		catalog.table(tableDerivedRelation),
	), generation, parent).Scan(
		&added,
		&removed,
		&delta.Events,
		&delta.Evidence,
		&delta.Passages,
		&delta.Projections,
		&delta.Limitations,
		&delta.Relations,
	); err != nil {
		return ScanCounts{}, fmt.Errorf("count the membership delta: %w", err)
	}
	counts := ScanCounts{
		Sessions:            inherited.Sessions + added - removed,
		Events:              inherited.Events + delta.Events,
		Evidence:            inherited.Evidence + delta.Evidence,
		Passages:            inherited.Passages + delta.Passages,
		Projections:         inherited.Projections + delta.Projections,
		Limitations:         inherited.Limitations + delta.Limitations,
		Relations:           inherited.Relations + delta.Relations,
		ResolvedRelations:   inherited.ResolvedRelations,
		UnresolvedRelations: inherited.UnresolvedRelations,
	}
	if added != 0 || removed != 0 {
		counts.ResolvedRelations = nil
		counts.UnresolvedRelations = nil
	}
	return counts, nil
}

// RecordedCounts returns the counts a generation stored when it was built.
func (catalog *Catalog) RecordedCounts(
	ctx context.Context,
	generation GenerationID,
) (ScanCounts, bool, error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return ScanCounts{}, false, err
	}
	var encoded []byte
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT counts FROM %s.generation WHERE id = $1",
		catalog.schema,
	), generation).Scan(&encoded); err != nil {
		return ScanCounts{}, false, fmt.Errorf("read generation counts: %w", err)
	}
	if len(encoded) == 0 {
		return ScanCounts{}, false, nil
	}
	var counts ScanCounts
	if err := json.Unmarshal(encoded, &counts); err != nil {
		return ScanCounts{}, false, fmt.Errorf(
			"decode generation counts: %w",
			err,
		)
	}
	return counts, true, nil
}

// maintenanceQuery runs one whole-corpus aggregate under the lifted statement
// timeout. Its cost scales with the catalog, not with a user request, and the
// command context still cancels it.
func (catalog *Catalog) maintenanceQuery(
	ctx context.Context,
	statement string,
	arguments []any,
	destinations ...any,
) (err error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return err
	}
	transaction, err := pool.BeginTx(ctx, pgx.TxOptions{
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return fmt.Errorf("begin catalog maintenance query: %w", err)
	}
	defer func() {
		err = errors.Join(err, discard(ctx, transaction))
	}()
	if err := liftStatementTimeout(ctx, transaction); err != nil {
		return err
	}
	if err := transaction.QueryRow(
		ctx,
		statement,
		arguments...,
	).Scan(destinations...); err != nil {
		return err
	}
	return transaction.Commit(ctx)
}

// relationResolutionQuery resolves every relation target against the revisions
// one generation presents. Resolution depends on the membership, so it is
// computed per generation and never stored on the immutable relation row.
// The presented events are grouped in one pass: restricting them to the
// identities relations actually name invites a nested loop of random index
// probes that costs minutes on a corpus-sized catalog.
const relationResolutionQuery = `WITH member AS (
	SELECT session.id, session.session_key, session.native_id
	FROM %[1]s.generation_member gm
	JOIN %[2]s session ON session.id = gm.derived_id
	WHERE gm.generation_id = $1),
event AS (
	SELECT event.observation_id, event.event_key, event.native_key,
		event.derived_id
	FROM %[3]s event JOIN member ON member.id = event.derived_id),
relation AS (
	SELECT relation.to_kind, relation.to_ref
	FROM %[4]s relation JOIN member ON member.id = relation.derived_id),
target AS (
	SELECT 'observation' AS kind, observation_id AS ref FROM event
		GROUP BY observation_id HAVING count(DISTINCT derived_id) = 1
	UNION ALL
	SELECT 'event', event_key FROM event
		GROUP BY event_key HAVING count(DISTINCT derived_id) = 1
	UNION ALL
	SELECT '%[6]s', native_key FROM event WHERE native_key <> ''
		GROUP BY native_key HAVING count(DISTINCT observation_id) = 1
	UNION ALL
	SELECT '%[5]s', native_id FROM member
		GROUP BY native_id HAVING count(*) = 1
	UNION ALL
	SELECT 'session', session_key FROM member)
SELECT count(*) FILTER (WHERE target.ref IS NOT NULL),
	count(*) FILTER (WHERE target.ref IS NULL)
FROM relation LEFT JOIN target
	ON target.kind = relation.to_kind AND target.ref = relation.to_ref`

// ResolveRelations counts how many of a generation's relations reach a session
// it presents. No transcript is reopened to resolve a peer.
func (catalog *Catalog) ResolveRelations(
	ctx context.Context,
	generation GenerationID,
) (resolved int64, unresolved int64, err error) {
	err = catalog.maintenanceQuery(ctx, fmt.Sprintf(
		relationResolutionQuery,
		catalog.schema,
		catalog.table(tableDerivedSession),
		catalog.table(tableDerivedEvent),
		catalog.table(tableDerivedRelation),
		ToKindSessionNative,
		ToKindNativeRecord,
	), []any{generation}, &resolved, &unresolved)
	if err != nil {
		return 0, 0, fmt.Errorf("resolve retained relations: %w", err)
	}
	return resolved, unresolved, nil
}

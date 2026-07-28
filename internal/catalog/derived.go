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

// membershipQuery compares two generations by the derived sessions they
// present. Both sides are covered by the membership primary key.
const membershipQuery = `SELECT NOT EXISTS (
	SELECT derived_id FROM %[1]s.generation_member WHERE generation_id = $1
	EXCEPT SELECT derived_id FROM %[1]s.generation_member WHERE generation_id = $2
) AND NOT EXISTS (
	SELECT derived_id FROM %[1]s.generation_member WHERE generation_id = $2
	EXCEPT SELECT derived_id FROM %[1]s.generation_member WHERE generation_id = $1
)`

// PresentsSameAs reports whether two generations present exactly the same
// derived sessions. Equal membership means equal rows, so the counts and the
// relation resolution of one are the counts and resolution of the other.
func (catalog *Catalog) PresentsSameAs(
	ctx context.Context,
	generation GenerationID,
	other GenerationID,
) (bool, error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return false, err
	}
	var same bool
	if err := pool.QueryRow(
		ctx,
		fmt.Sprintf(membershipQuery, catalog.schema),
		generation,
		other,
	).Scan(&same); err != nil {
		return false, fmt.Errorf("compare generation membership: %w", err)
	}
	return same, nil
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
	SELECT event.observation_id, event.event_key, event.derived_id
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
	), []any{generation}, &resolved, &unresolved)
	if err != nil {
		return 0, 0, fmt.Errorf("resolve retained relations: %w", err)
	}
	return resolved, unresolved, nil
}

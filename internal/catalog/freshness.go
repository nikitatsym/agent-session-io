package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// writerLeaseKey keeps the catalog single-writer for a whole scan lifecycle:
// write, publish, and reclaim. It is a second advisory key rather than the
// maintenance one, because the writer's own reservation transactions take that
// one on other connections and would deadlock against their own lease.
const writerLeaseKey = 0x5e5511

// ScanLease is the exclusive right to write this catalog. It lives on one
// connection: closing the connection releases it, so a killed writer never
// leaves the catalog locked.
type ScanLease struct {
	catalog    *Catalog
	connection *pgxpool.Conn
	done       bool
}

// AcquireScanLease takes the single-writer lease or reports who holds it.
func (catalog *Catalog) AcquireScanLease(ctx context.Context) (*ScanLease, error) {
	connection, err := catalog.connection(ctx)
	if err != nil {
		return nil, err
	}
	var taken bool
	if err := connection.QueryRow(
		ctx,
		"SELECT pg_try_advisory_lock(hashtext($1), $2)",
		catalog.leaseName(),
		int32(writerLeaseKey),
	).Scan(&taken); err != nil {
		connection.Release()
		return nil, fmt.Errorf("take the catalog writer lease: %w", err)
	}
	if !taken {
		connection.Release()
		return nil, catalog.scanInProgress()
	}
	lease := &ScanLease{catalog: catalog, connection: connection}
	catalog.mutex.Lock()
	defer catalog.mutex.Unlock()
	catalog.lease = lease
	return lease, nil
}

// Release returns the lease. A failed unlock destroys the connection instead of
// returning it to the pool, because a pooled connection would keep the lock.
func (lease *ScanLease) Release(ctx context.Context) error {
	if !lease.claim() {
		return nil
	}
	var released bool
	err := lease.connection.QueryRow(
		ctx,
		"SELECT pg_advisory_unlock(hashtext($1), $2)",
		lease.catalog.leaseName(),
		int32(writerLeaseKey),
	).Scan(&released)
	if err == nil && released {
		lease.connection.Release()
		return nil
	}
	closeErr := lease.close(ctx)
	if err != nil {
		return errors.Join(
			fmt.Errorf("release the catalog writer lease: %w", err),
			closeErr,
		)
	}
	return closeErr
}

// discard drops a lease nobody released, so closing the catalog never waits for
// a connection that is still checked out.
func (lease *ScanLease) discard(ctx context.Context) {
	if !lease.claim() {
		return
	}
	_ = lease.close(ctx)
}

// claim reports whether this call owns the release; a second call is a no-op.
func (lease *ScanLease) claim() bool {
	lease.catalog.mutex.Lock()
	defer lease.catalog.mutex.Unlock()
	if lease.done {
		return false
	}
	lease.done = true
	if lease.catalog.lease == lease {
		lease.catalog.lease = nil
	}
	return true
}

func (lease *ScanLease) close(ctx context.Context) error {
	return lease.connection.Hijack().Close(ctx)
}

// RequireQuiescentWriter refuses to answer while another session is writing
// this catalog: a generation being replaced is not a state to serve from.
func (catalog *Catalog) RequireQuiescentWriter(ctx context.Context) error {
	active, err := catalog.writerActive(ctx)
	if err != nil {
		return err
	}
	if active {
		return catalog.scanInProgress()
	}
	return nil
}

// writerActive reports whether another session holds the writer lease. It takes
// the shared lock, so two readers never refuse each other.
func (catalog *Catalog) writerActive(ctx context.Context) (bool, error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return false, err
	}
	var active bool
	if err := pool.QueryRow(
		ctx,
		"SELECT CASE WHEN pg_try_advisory_lock_shared(hashtext($1), $2)"+
			" THEN NOT pg_advisory_unlock_shared(hashtext($1), $2)"+
			" ELSE true END",
		catalog.leaseName(),
		int32(writerLeaseKey),
	).Scan(&active); err != nil {
		return false, fmt.Errorf("probe the catalog writer lease: %w", err)
	}
	return active, nil
}

func (catalog *Catalog) leaseName() string {
	return "sessionio:" + catalog.settings.SchemaName
}

func (catalog *Catalog) scanInProgress() error {
	return &Error{
		Kind: KindScanInProgress,
		Message: fmt.Sprintf(
			"another sessionio scan is writing catalog schema %s",
			catalog.settings.SchemaName,
		),
		Remediation: "wait for the running scan to finish and run the command" +
			" again",
		Details: map[string]any{
			"catalog_schema": catalog.settings.SchemaName,
		},
	}
}

// PresentedRevision is one session revision an active generation answers from.
type PresentedRevision struct {
	OccurrenceID      string
	DiscoveryRevision string
	SourceID          string
	Harness           string
}

const presentedRevisionQuery = `SELECT session.occurrence_id,
	session.discovery_revision, session.source_id, session.harness
FROM %[1]s.generation_member member
JOIN %[2]s session ON session.id = member.derived_id
WHERE member.generation_id = $1`

// PresentedRevisions lists what one generation presents, occurrence by
// occurrence. It reads no derived row beyond the session header.
func (catalog *Catalog) PresentedRevisions(
	ctx context.Context,
	generation GenerationID,
) ([]PresentedRevision, error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, fmt.Sprintf(
		presentedRevisionQuery,
		catalog.schema,
		catalog.table(tableDerivedSession),
	), generation)
	if err != nil {
		return nil, fmt.Errorf("list the presented revisions: %w", err)
	}
	defer rows.Close()
	var presented []PresentedRevision
	for rows.Next() {
		var revision PresentedRevision
		if err := rows.Scan(
			&revision.OccurrenceID,
			&revision.DiscoveryRevision,
			&revision.SourceID,
			&revision.Harness,
		); err != nil {
			return nil, fmt.Errorf("read a presented revision: %w", err)
		}
		presented = append(presented, revision)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read a presented revision: %w", err)
	}
	return presented, nil
}

// UnreclaimedGenerations counts the generations still holding derived rows no
// search presents: a killed candidate, a failed scan, or a reclaim that could
// not finish. One is enough to make the catalog dirty.
func (catalog *Catalog) UnreclaimedGenerations(
	ctx context.Context,
) (int64, error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return 0, err
	}
	var count int64
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT count(*) FROM %[1]s.generation WHERE reclaimed_at IS NULL"+
			" AND id NOT IN (SELECT generation_id FROM %[1]s.active_generation)",
		catalog.schema,
	)).Scan(&count); err != nil {
		return 0, fmt.Errorf("count unreclaimed generations: %w", err)
	}
	return count, nil
}

// FailedSources names the sources a partial generation knowingly omits. Their
// sessions are as fresh as this catalog can be, so the freshness gate does not
// count them as behind.
func (catalog *Catalog) FailedSources(
	ctx context.Context,
	generation GenerationID,
) ([]SourceFailure, error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return nil, err
	}
	var encoded []byte
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT diagnostics FROM %s.generation WHERE id = $1",
		catalog.schema,
	), generation).Scan(&encoded); err != nil {
		return nil, fmt.Errorf("read generation diagnostics: %w", err)
	}
	if len(encoded) == 0 {
		return nil, nil
	}
	var diagnostics struct {
		FailedSources []SourceFailure `json:"failed_sources"`
	}
	if err := json.Unmarshal(encoded, &diagnostics); err != nil {
		return nil, fmt.Errorf("decode generation diagnostics: %w", err)
	}
	return diagnostics.FailedSources, nil
}

// SweepAbandonedCandidates fails every candidate generation left behind by a
// writer that no longer exists. The caller holds the writer lease and has not
// opened its own candidate yet, so every building generation is abandoned; the
// reclaim that follows releases their rows.
func (catalog *Catalog) SweepAbandonedCandidates(
	ctx context.Context,
) (int64, error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return 0, err
	}
	tag, err := pool.Exec(ctx, fmt.Sprintf(
		"UPDATE %s.generation SET state = $1 WHERE state = $2",
		catalog.schema,
	), StateFailed, StateBuilding)
	if err != nil {
		return 0, fmt.Errorf("sweep abandoned candidate generations: %w", err)
	}
	return tag.RowsAffected(), nil
}

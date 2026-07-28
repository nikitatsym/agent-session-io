package catalog

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
)

// SnapshotCodec and SnapshotCodecVersion pin the retention compression. They
// are recorded per blob so a later codec change stays identifiable.
const (
	SnapshotCodec        = "gzip"
	SnapshotCodecVersion = "compress/gzip:9"
)

// Change classifies one observed source revision against its checkpoint.
const (
	ChangeInitial   = "initial"
	ChangeUnchanged = "unchanged"
	ChangeGrown     = "grown"
	ChangeTruncated = "truncated"
	ChangeRewritten = "rewritten"
	ChangeReplaced  = "replaced"
)

// TailClean and TailPending distinguish a complete final record from bytes
// that no complete record covers yet.
const (
	TailClean   = "clean"
	TailPending = "pending"
)

// RetainedSource is one observed source identity.
type RetainedSource struct {
	SourceID string
	Harness  string
	Locator  Locator
}

// RetainedOccurrence is one observed instance of a source.
type RetainedOccurrence struct {
	OccurrenceID string
	SourceID     string
	Harness      string
	Locator      Locator
}

// SnapshotBlob is one content-addressed compressed native snapshot.
type SnapshotBlob struct {
	ContentHash      []byte
	Codec            string
	CodecVersion     string
	UncompressedSize int64
	CompressedSize   int64
	Checksum         []byte
	Data             []byte
}

// SessionRevision is one immutable observed session state.
type SessionRevision struct {
	RevisionHash        []byte
	SessionKey          string
	OccurrenceID        string
	Harness             string
	NativeID            string
	Title               string
	DiscoveryRevision   string
	SourceRevisionKind  string
	SourceRevisionValue string
	SnapshotHash        []byte
	Locator             Locator
	StartedAt           *time.Time
	UpdatedAt           *time.Time
	EventCount          int64
}

// Checkpoint is the last confirmed scan position of one source occurrence.
type Checkpoint struct {
	OccurrenceID        string
	RevisionHash        []byte
	DiscoveryRevision   string
	SourceRevisionValue string
	SnapshotHash        []byte
	SnapshotSize        int64
	SourceSize          int64
	RecordCount         int64
	FileIdentity        string
	TailKind            string
	ChangeKind          string
}

// CompressSnapshot produces the deterministic retained blob for one snapshot.
func CompressSnapshot(data []byte) (SnapshotBlob, error) {
	contentHash := sha256.Sum256(data)
	var buffer bytes.Buffer
	compressor, err := gzip.NewWriterLevel(&buffer, gzip.BestCompression)
	if err != nil {
		return SnapshotBlob{}, fmt.Errorf("configure snapshot compression: %w", err)
	}
	if _, err := compressor.Write(data); err != nil {
		return SnapshotBlob{}, fmt.Errorf("compress snapshot: %w", err)
	}
	if err := compressor.Close(); err != nil {
		return SnapshotBlob{}, fmt.Errorf("finish snapshot compression: %w", err)
	}
	compressed := buffer.Bytes()
	checksum := sha256.Sum256(compressed)
	return SnapshotBlob{
		ContentHash:      contentHash[:],
		Codec:            SnapshotCodec,
		CodecVersion:     SnapshotCodecVersion,
		UncompressedSize: int64(len(data)),
		CompressedSize:   int64(len(compressed)),
		Checksum:         checksum[:],
		Data:             compressed,
	}, nil
}

// DecompressSnapshot restores a blob and proves both of its digests.
func DecompressSnapshot(blob SnapshotBlob) ([]byte, error) {
	if blob.Codec != SnapshotCodec {
		return nil, fmt.Errorf("snapshot codec %q is unsupported", blob.Codec)
	}
	checksum := sha256.Sum256(blob.Data)
	if !bytes.Equal(checksum[:], blob.Checksum) {
		return nil, errors.New("snapshot blob fails its compressed checksum")
	}
	reader, err := gzip.NewReader(bytes.NewReader(blob.Data))
	if err != nil {
		return nil, fmt.Errorf("open snapshot blob: %w", err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("read snapshot blob: %w", err), reader.Close())
	}
	if err := reader.Close(); err != nil {
		return nil, fmt.Errorf("close snapshot blob: %w", err)
	}
	contentHash := sha256.Sum256(data)
	if !bytes.Equal(contentHash[:], blob.ContentHash) {
		return nil, errors.New("snapshot blob fails its content hash")
	}
	return data, nil
}

// RevisionHash identifies one immutable session revision. Distinct occurrences
// never share a revision even when their snapshot bytes are equal.
func RevisionHash(revision SessionRevision) []byte {
	digest := sha256.New()
	for _, part := range []string{
		"sessionio.session-revision/v1",
		revision.OccurrenceID,
		revision.SessionKey,
		revision.NativeID,
		revision.DiscoveryRevision,
		revision.SourceRevisionValue,
		revision.Title,
	} {
		digest.Write([]byte(part))
		digest.Write([]byte{0})
	}
	digest.Write(revision.SnapshotHash)
	return digest.Sum(nil)
}

// ObserveSource records a currently visible source and clears its tombstone.
func (catalog *Catalog) ObserveSource(
	ctx context.Context,
	source RetainedSource,
	now time.Time,
) error {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO %s.source (source_id, harness, locator_kind, locator_root,"+
			" locator_path, first_seen_at, last_seen_at)"+
			" VALUES ($1, $2, $3, $4, $5, $6, $6)"+
			" ON CONFLICT (source_id) DO UPDATE SET last_seen_at = EXCLUDED.last_seen_at,"+
			" locator_kind = EXCLUDED.locator_kind,"+
			" locator_root = EXCLUDED.locator_root,"+
			" locator_path = EXCLUDED.locator_path,"+
			" disappeared_at = NULL",
		catalog.schema,
	),
		source.SourceID,
		source.Harness,
		source.Locator.Kind,
		source.Locator.Root,
		source.Locator.Path,
		now,
	); err != nil {
		return fmt.Errorf("retain source %s: %w", source.SourceID, err)
	}
	return nil
}

// ObserveOccurrence records a currently visible source occurrence.
func (catalog *Catalog) ObserveOccurrence(
	ctx context.Context,
	occurrence RetainedOccurrence,
	now time.Time,
) error {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO %s.source_occurrence (occurrence_id, source_id, harness,"+
			" locator_kind, locator_root, locator_path, first_seen_at, last_seen_at)"+
			" VALUES ($1, $2, $3, $4, $5, $6, $7, $7)"+
			" ON CONFLICT (occurrence_id) DO UPDATE SET"+
			" last_seen_at = EXCLUDED.last_seen_at,"+
			" locator_kind = EXCLUDED.locator_kind,"+
			" locator_root = EXCLUDED.locator_root,"+
			" locator_path = EXCLUDED.locator_path,"+
			" disappeared_at = NULL",
		catalog.schema,
	),
		occurrence.OccurrenceID,
		occurrence.SourceID,
		occurrence.Harness,
		occurrence.Locator.Kind,
		occurrence.Locator.Root,
		occurrence.Locator.Path,
		now,
	); err != nil {
		return fmt.Errorf(
			"retain source occurrence %s: %w",
			occurrence.OccurrenceID,
			err,
		)
	}
	return nil
}

// PutSnapshot retains one compressed snapshot. Equal bytes reuse one blob.
func (catalog *Catalog) PutSnapshot(
	ctx context.Context,
	blob SnapshotBlob,
	now time.Time,
) (reused bool, err error) {
	return catalog.insertIfAbsent(
		ctx,
		"retain snapshot blob",
		fmt.Sprintf(
			"INSERT INTO %s.snapshot_blob (content_hash, codec, codec_version,"+
				" uncompressed_size, compressed_size, checksum, data, created_at)"+
				" VALUES ($1, $2, $3, $4, $5, $6, $7, $8)"+
				" ON CONFLICT (content_hash) DO NOTHING",
			catalog.schema,
		),
		blob.ContentHash,
		blob.Codec,
		blob.CodecVersion,
		blob.UncompressedSize,
		blob.CompressedSize,
		blob.Checksum,
		blob.Data,
		now,
	)
}

// insertIfAbsent reports whether a content-addressed row already existed.
func (catalog *Catalog) insertIfAbsent(
	ctx context.Context,
	action string,
	statement string,
	arguments ...any,
) (bool, error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return false, err
	}
	tag, err := pool.Exec(ctx, statement, arguments...)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return tag.RowsAffected() == 0, nil
}

// LoadSnapshot returns the retained bytes behind one content hash.
func (catalog *Catalog) LoadSnapshot(
	ctx context.Context,
	contentHash []byte,
) ([]byte, bool, error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var blob SnapshotBlob
	blob.ContentHash = contentHash
	err = pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT codec, checksum, data FROM %s.snapshot_blob"+
			" WHERE content_hash = $1",
		catalog.schema,
	), contentHash).Scan(&blob.Codec, &blob.Checksum, &blob.Data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read snapshot blob: %w", err)
	}
	data, err := DecompressSnapshot(blob)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// PutSessionRevision retains one immutable session revision.
func (catalog *Catalog) PutSessionRevision(
	ctx context.Context,
	revision SessionRevision,
	now time.Time,
) (reused bool, err error) {
	return catalog.insertIfAbsent(
		ctx,
		"retain session revision",
		fmt.Sprintf(
			"INSERT INTO %s.session_revision (revision_hash, session_key,"+
				" occurrence_id, harness, native_id, title, discovery_revision,"+
				" source_revision_kind, source_revision_value, snapshot_hash,"+
				" locator_kind, locator_root, locator_path, started_at,"+
				" updated_at, event_count, observed_at)"+
				" VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,"+
				" $13, $14, $15, $16, $17)"+
				" ON CONFLICT (revision_hash) DO NOTHING",
			catalog.schema,
		),
		revision.RevisionHash,
		revision.SessionKey,
		revision.OccurrenceID,
		revision.Harness,
		revision.NativeID,
		revision.Title,
		revision.DiscoveryRevision,
		revision.SourceRevisionKind,
		revision.SourceRevisionValue,
		revision.SnapshotHash,
		revision.Locator.Kind,
		revision.Locator.Root,
		revision.Locator.Path,
		revision.StartedAt,
		revision.UpdatedAt,
		revision.EventCount,
		now,
	)
}

// PutCheckpoint stores the confirmed scan position of one occurrence.
func (catalog *Catalog) PutCheckpoint(
	ctx context.Context,
	checkpoint Checkpoint,
	now time.Time,
) error {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO %s.scan_checkpoint (occurrence_id, revision_hash,"+
			" discovery_revision, source_revision_value, snapshot_hash,"+
			" snapshot_size, source_size, record_count, file_identity,"+
			" tail_kind, change_kind, observed_at)"+
			" VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)"+
			" ON CONFLICT (occurrence_id) DO UPDATE SET"+
			" revision_hash = EXCLUDED.revision_hash,"+
			" discovery_revision = EXCLUDED.discovery_revision,"+
			" source_revision_value = EXCLUDED.source_revision_value,"+
			" snapshot_hash = EXCLUDED.snapshot_hash,"+
			" snapshot_size = EXCLUDED.snapshot_size,"+
			" source_size = EXCLUDED.source_size,"+
			" record_count = EXCLUDED.record_count,"+
			" file_identity = EXCLUDED.file_identity,"+
			" tail_kind = EXCLUDED.tail_kind,"+
			" change_kind = EXCLUDED.change_kind,"+
			" observed_at = EXCLUDED.observed_at",
		catalog.schema,
	),
		checkpoint.OccurrenceID,
		checkpoint.RevisionHash,
		checkpoint.DiscoveryRevision,
		checkpoint.SourceRevisionValue,
		checkpoint.SnapshotHash,
		checkpoint.SnapshotSize,
		checkpoint.SourceSize,
		checkpoint.RecordCount,
		checkpoint.FileIdentity,
		checkpoint.TailKind,
		checkpoint.ChangeKind,
		now,
	); err != nil {
		return fmt.Errorf(
			"retain scan checkpoint for %s: %w",
			checkpoint.OccurrenceID,
			err,
		)
	}
	return nil
}

// LoadCheckpoint returns the stored checkpoint of one occurrence.
func (catalog *Catalog) LoadCheckpoint(
	ctx context.Context,
	occurrenceID string,
) (Checkpoint, bool, error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return Checkpoint{}, false, err
	}
	checkpoint := Checkpoint{OccurrenceID: occurrenceID}
	err = pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT revision_hash, discovery_revision, source_revision_value,"+
			" snapshot_hash, snapshot_size, source_size, record_count,"+
			" file_identity, tail_kind, change_kind"+
			" FROM %s.scan_checkpoint WHERE occurrence_id = $1",
		catalog.schema,
	), occurrenceID).Scan(
		&checkpoint.RevisionHash,
		&checkpoint.DiscoveryRevision,
		&checkpoint.SourceRevisionValue,
		&checkpoint.SnapshotHash,
		&checkpoint.SnapshotSize,
		&checkpoint.SourceSize,
		&checkpoint.RecordCount,
		&checkpoint.FileIdentity,
		&checkpoint.TailKind,
		&checkpoint.ChangeKind,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Checkpoint{}, false, nil
	}
	if err != nil {
		return Checkpoint{}, false, fmt.Errorf("read scan checkpoint: %w", err)
	}
	return checkpoint, true, nil
}

// TombstoneCounts report how many identities disappeared during one scan.
type TombstoneCounts struct {
	Sources     int64 `json:"sources"`
	Occurrences int64 `json:"occurrences"`
}

// Tombstone marks every retained identity that this scan did not observe.
func (catalog *Catalog) Tombstone(
	ctx context.Context,
	seenSources []string,
	seenOccurrences []string,
	now time.Time,
) (TombstoneCounts, error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return TombstoneCounts{}, err
	}
	var counts TombstoneCounts
	occurrences, err := pool.Exec(ctx, fmt.Sprintf(
		"UPDATE %s.source_occurrence SET disappeared_at = $1"+
			" WHERE disappeared_at IS NULL AND NOT (occurrence_id = ANY($2))",
		catalog.schema,
	), now, seenOccurrences)
	if err != nil {
		return TombstoneCounts{}, fmt.Errorf("tombstone occurrences: %w", err)
	}
	counts.Occurrences = occurrences.RowsAffected()
	sources, err := pool.Exec(ctx, fmt.Sprintf(
		"UPDATE %s.source SET disappeared_at = $1"+
			" WHERE disappeared_at IS NULL AND NOT (source_id = ANY($2))",
		catalog.schema,
	), now, seenSources)
	if err != nil {
		return TombstoneCounts{}, fmt.Errorf("tombstone sources: %w", err)
	}
	counts.Sources = sources.RowsAffected()
	return counts, nil
}

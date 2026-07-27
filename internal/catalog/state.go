package catalog

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
)

// State stream record schemas. The stream carries state class 1 only:
// retained observations, immutable revisions, and checkpoints.
const (
	StateManifestSchema   = "sessionio.catalog.state-manifest/v1"
	StateSourceSchema     = "sessionio.catalog.source/v1"
	StateOccurrenceSchema = "sessionio.catalog.source-occurrence/v1"
	StateBlobSchema       = "sessionio.catalog.snapshot-blob/v1"
	StateRevisionSchema   = "sessionio.catalog.session-revision/v1"
	StateCheckpointSchema = "sessionio.catalog.scan-checkpoint/v1"
)

// StateCounts are the retained record counts of one state stream.
type StateCounts struct {
	Sources          int64 `json:"sources"`
	Occurrences      int64 `json:"source_occurrences"`
	SnapshotBlobs    int64 `json:"snapshot_blobs"`
	SessionRevisions int64 `json:"session_revisions"`
	Checkpoints      int64 `json:"scan_checkpoints"`
}

// StateSummary identifies exactly one exported or imported stream.
type StateSummary struct {
	Counts   StateCounts `json:"counts"`
	Checksum string      `json:"checksum"`
}

type stateManifest struct {
	Schema          string      `json:"schema"`
	CatalogSchema   string      `json:"catalog_schema"`
	CatalogRevision int         `json:"catalog_revision"`
	ExportedAt      time.Time   `json:"exported_at"`
	Counts          StateCounts `json:"counts"`
	Checksum        string      `json:"checksum"`
}

// stateIdentity carries the sighting facts every observed identity records.
// disappeared_at is the tombstone.
type stateIdentity struct {
	Harness       string     `json:"harness"`
	LocatorKind   string     `json:"locator_kind"`
	LocatorRoot   string     `json:"locator_root"`
	LocatorPath   string     `json:"locator_path"`
	FirstSeenAt   time.Time  `json:"first_seen_at"`
	LastSeenAt    time.Time  `json:"last_seen_at"`
	DisappearedAt *time.Time `json:"disappeared_at"`
}

func (identity *stateIdentity) targets() []any {
	return []any{
		&identity.Harness, &identity.LocatorKind, &identity.LocatorRoot,
		&identity.LocatorPath, &identity.FirstSeenAt, &identity.LastSeenAt,
		&identity.DisappearedAt,
	}
}

func (identity stateIdentity) values() []any {
	return []any{
		identity.Harness, identity.LocatorKind, identity.LocatorRoot,
		identity.LocatorPath, identity.FirstSeenAt, identity.LastSeenAt,
		identity.DisappearedAt,
	}
}

type stateSource struct {
	Schema   string `json:"schema"`
	SourceID string `json:"source_id"`
	stateIdentity
}

type stateOccurrence struct {
	Schema       string `json:"schema"`
	OccurrenceID string `json:"occurrence_id"`
	SourceID     string `json:"source_id"`
	stateIdentity
}

type stateBlob struct {
	Schema           string    `json:"schema"`
	ContentHash      string    `json:"content_hash"`
	Codec            string    `json:"codec"`
	CodecVersion     string    `json:"codec_version"`
	UncompressedSize int64     `json:"uncompressed_size"`
	CompressedSize   int64     `json:"compressed_size"`
	Checksum         string    `json:"checksum"`
	Data             string    `json:"data"`
	CreatedAt        time.Time `json:"created_at"`
}

type stateRevision struct {
	Schema              string     `json:"schema"`
	RevisionHash        string     `json:"revision_hash"`
	SessionKey          string     `json:"session_key"`
	OccurrenceID        string     `json:"occurrence_id"`
	Harness             string     `json:"harness"`
	NativeID            string     `json:"native_id"`
	Title               string     `json:"title"`
	DiscoveryRevision   string     `json:"discovery_revision"`
	SourceRevisionKind  string     `json:"source_revision_kind"`
	SourceRevisionValue string     `json:"source_revision_value"`
	SnapshotHash        string     `json:"snapshot_hash"`
	LocatorKind         string     `json:"locator_kind"`
	LocatorRoot         string     `json:"locator_root"`
	LocatorPath         string     `json:"locator_path"`
	StartedAt           *time.Time `json:"started_at"`
	UpdatedAt           *time.Time `json:"updated_at"`
	EventCount          int64      `json:"event_count"`
	ObservedAt          time.Time  `json:"observed_at"`
}

type stateCheckpoint struct {
	Schema              string    `json:"schema"`
	OccurrenceID        string    `json:"occurrence_id"`
	RevisionHash        string    `json:"revision_hash"`
	DiscoveryRevision   string    `json:"discovery_revision"`
	SourceRevisionValue string    `json:"source_revision_value"`
	SnapshotHash        string    `json:"snapshot_hash"`
	SnapshotSize        int64     `json:"snapshot_size"`
	SourceSize          int64     `json:"source_size"`
	RecordCount         int64     `json:"record_count"`
	FileIdentity        string    `json:"file_identity"`
	TailKind            string    `json:"tail_kind"`
	ChangeKind          string    `json:"change_kind"`
	ObservedAt          time.Time `json:"observed_at"`
}

// stateStream is the fully decoded stream held before a single transaction.
type stateStream struct {
	sources     []stateSource
	occurrences []stateOccurrence
	blobs       []stateBlob
	revisions   []stateRevision
	checkpoints []stateCheckpoint
}

func (stream stateStream) counts() StateCounts {
	return StateCounts{
		Sources:          int64(len(stream.sources)),
		Occurrences:      int64(len(stream.occurrences)),
		SnapshotBlobs:    int64(len(stream.blobs)),
		SessionRevisions: int64(len(stream.revisions)),
		Checkpoints:      int64(len(stream.checkpoints)),
	}
}

const maxStateLineBytes = 64 * 1024 * 1024

// ExportState writes every retained record as one versioned NDJSON stream.
func (catalog *Catalog) ExportState(
	ctx context.Context,
	writer io.Writer,
	now time.Time,
) (StateSummary, error) {
	if err := catalog.requireInitialized(ctx); err != nil {
		return StateSummary{}, err
	}
	stream, err := catalog.readState(ctx)
	if err != nil {
		return StateSummary{}, err
	}
	lines, err := encodeState(stream)
	if err != nil {
		return StateSummary{}, err
	}
	summary := StateSummary{Counts: stream.counts(), Checksum: checksumLines(lines)}
	manifest, err := json.Marshal(stateManifest{
		Schema:          StateManifestSchema,
		CatalogSchema:   catalog.settings.SchemaName,
		CatalogRevision: Revision,
		ExportedAt:      now.UTC(),
		Counts:          summary.Counts,
		Checksum:        summary.Checksum,
	})
	if err != nil {
		return StateSummary{}, fmt.Errorf("encode state manifest: %w", err)
	}
	buffered := bufio.NewWriter(writer)
	if _, err := buffered.Write(append(manifest, '\n')); err != nil {
		return StateSummary{}, fmt.Errorf("write state manifest: %w", err)
	}
	for _, line := range lines {
		if _, err := buffered.Write(line); err != nil {
			return StateSummary{}, fmt.Errorf("write state record: %w", err)
		}
	}
	if err := buffered.Flush(); err != nil {
		return StateSummary{}, fmt.Errorf("flush state stream: %w", err)
	}
	return summary, nil
}

func checksumLines(lines [][]byte) string {
	digest := sha256.New()
	for _, line := range lines {
		digest.Write(line)
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil))
}

func encodeState(stream stateStream) ([][]byte, error) {
	records := make([]any, 0, len(stream.sources)+len(stream.occurrences)+
		len(stream.blobs)+len(stream.revisions)+len(stream.checkpoints))
	for _, record := range stream.sources {
		records = append(records, record)
	}
	for _, record := range stream.occurrences {
		records = append(records, record)
	}
	for _, record := range stream.blobs {
		records = append(records, record)
	}
	for _, record := range stream.revisions {
		records = append(records, record)
	}
	for _, record := range stream.checkpoints {
		records = append(records, record)
	}
	lines := make([][]byte, 0, len(records))
	for _, record := range records {
		encoded, err := json.Marshal(record)
		if err != nil {
			return nil, fmt.Errorf("encode state record: %w", err)
		}
		lines = append(lines, append(encoded, '\n'))
	}
	return lines, nil
}

const (
	sourceQuery = "SELECT source_id, harness, locator_kind, locator_root," +
		" locator_path, first_seen_at, last_seen_at, disappeared_at" +
		" FROM %s.source ORDER BY source_id"
	occurrenceQuery = "SELECT occurrence_id, source_id, harness, locator_kind," +
		" locator_root, locator_path, first_seen_at, last_seen_at," +
		" disappeared_at FROM %s.source_occurrence ORDER BY occurrence_id"
	blobQuery = "SELECT content_hash, codec, codec_version, uncompressed_size," +
		" compressed_size, checksum, data, created_at" +
		" FROM %s.snapshot_blob ORDER BY content_hash"
	revisionQuery = "SELECT revision_hash, session_key, occurrence_id, harness," +
		" native_id, title, discovery_revision, source_revision_kind," +
		" source_revision_value, snapshot_hash, locator_kind, locator_root," +
		" locator_path, started_at, updated_at, event_count, observed_at" +
		" FROM %s.session_revision ORDER BY revision_hash"
	checkpointQuery = "SELECT occurrence_id, revision_hash, discovery_revision," +
		" source_revision_value, snapshot_hash, snapshot_size, source_size," +
		" record_count, file_identity, tail_kind, change_kind, observed_at" +
		" FROM %s.scan_checkpoint ORDER BY occurrence_id"
)

func (catalog *Catalog) readState(ctx context.Context) (stateStream, error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return stateStream{}, err
	}
	var stream stateStream
	if err := collect(ctx, pool, fmt.Sprintf(sourceQuery, catalog.schema),
		func(rows pgx.Rows) error {
			record := stateSource{Schema: StateSourceSchema}
			targets := append(
				[]any{&record.SourceID},
				record.stateIdentity.targets()...,
			)
			if err := rows.Scan(targets...); err != nil {
				return err
			}
			stream.sources = append(stream.sources, record)
			return nil
		}); err != nil {
		return stateStream{}, err
	}
	if err := collect(ctx, pool, fmt.Sprintf(occurrenceQuery, catalog.schema),
		func(rows pgx.Rows) error {
			record := stateOccurrence{Schema: StateOccurrenceSchema}
			targets := append(
				[]any{&record.OccurrenceID, &record.SourceID},
				record.stateIdentity.targets()...,
			)
			if err := rows.Scan(targets...); err != nil {
				return err
			}
			stream.occurrences = append(stream.occurrences, record)
			return nil
		}); err != nil {
		return stateStream{}, err
	}
	if err := collect(ctx, pool, fmt.Sprintf(blobQuery, catalog.schema),
		func(rows pgx.Rows) error {
			record := stateBlob{Schema: StateBlobSchema}
			var contentHash, checksum, data []byte
			if err := rows.Scan(
				&contentHash, &record.Codec, &record.CodecVersion,
				&record.UncompressedSize, &record.CompressedSize, &checksum,
				&data, &record.CreatedAt,
			); err != nil {
				return err
			}
			record.ContentHash = hex.EncodeToString(contentHash)
			record.Checksum = hex.EncodeToString(checksum)
			record.Data = base64.StdEncoding.EncodeToString(data)
			stream.blobs = append(stream.blobs, record)
			return nil
		}); err != nil {
		return stateStream{}, err
	}
	if err := collect(ctx, pool, fmt.Sprintf(revisionQuery, catalog.schema),
		func(rows pgx.Rows) error {
			record := stateRevision{Schema: StateRevisionSchema}
			var revisionHash, snapshotHash []byte
			if err := rows.Scan(
				&revisionHash, &record.SessionKey, &record.OccurrenceID,
				&record.Harness, &record.NativeID, &record.Title,
				&record.DiscoveryRevision, &record.SourceRevisionKind,
				&record.SourceRevisionValue, &snapshotHash, &record.LocatorKind,
				&record.LocatorRoot, &record.LocatorPath, &record.StartedAt,
				&record.UpdatedAt, &record.EventCount, &record.ObservedAt,
			); err != nil {
				return err
			}
			record.RevisionHash = hex.EncodeToString(revisionHash)
			record.SnapshotHash = hex.EncodeToString(snapshotHash)
			stream.revisions = append(stream.revisions, record)
			return nil
		}); err != nil {
		return stateStream{}, err
	}
	if err := collect(ctx, pool, fmt.Sprintf(checkpointQuery, catalog.schema),
		func(rows pgx.Rows) error {
			record := stateCheckpoint{Schema: StateCheckpointSchema}
			var revisionHash, snapshotHash []byte
			if err := rows.Scan(
				&record.OccurrenceID, &revisionHash, &record.DiscoveryRevision,
				&record.SourceRevisionValue, &snapshotHash, &record.SnapshotSize,
				&record.SourceSize, &record.RecordCount, &record.FileIdentity,
				&record.TailKind, &record.ChangeKind, &record.ObservedAt,
			); err != nil {
				return err
			}
			record.RevisionHash = hex.EncodeToString(revisionHash)
			record.SnapshotHash = hex.EncodeToString(snapshotHash)
			stream.checkpoints = append(stream.checkpoints, record)
			return nil
		}); err != nil {
		return stateStream{}, err
	}
	return stream, nil
}

func collect(
	ctx context.Context,
	pool queryRunner,
	statement string,
	scan func(pgx.Rows) error,
) error {
	rows, err := pool.Query(ctx, statement)
	if err != nil {
		return fmt.Errorf("read retained state: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return fmt.Errorf("read retained state row: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read retained state: %w", err)
	}
	return nil
}

func stateCorrupt(cause error, message string, details map[string]any) error {
	return &Error{
		Kind:        KindCatalogStateCorrupt,
		Message:     message,
		Remediation: "re-export the state stream from its source catalog",
		Details:     details,
		Cause:       cause,
	}
}

// ImportState validates the entire stream before one all-or-nothing
// transaction, so a corrupt byte leaves the target exactly as it was.
func (catalog *Catalog) ImportState(
	ctx context.Context,
	reader io.Reader,
) (StateSummary, error) {
	if err := catalog.requireInitialized(ctx); err != nil {
		return StateSummary{}, err
	}
	manifest, stream, err := decodeState(reader)
	if err != nil {
		return StateSummary{}, err
	}
	if err := catalog.requireEmptyState(ctx); err != nil {
		return StateSummary{}, err
	}
	if err := catalog.writeState(ctx, stream); err != nil {
		return StateSummary{}, err
	}
	return StateSummary{Counts: manifest.Counts, Checksum: manifest.Checksum}, nil
}

func decodeState(reader io.Reader) (stateManifest, stateStream, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), maxStateLineBytes)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return stateManifest{}, stateStream{}, fmt.Errorf(
				"read state stream: %w",
				err,
			)
		}
		return stateManifest{}, stateStream{}, stateCorrupt(
			nil,
			"the state stream is empty and carries no manifest",
			map[string]any{"expected_schema": StateManifestSchema},
		)
	}
	var manifest stateManifest
	if err := json.Unmarshal(scanner.Bytes(), &manifest); err != nil {
		return stateManifest{}, stateStream{}, stateCorrupt(
			err,
			"the first state record is not a manifest: "+err.Error(),
			map[string]any{"expected_schema": StateManifestSchema},
		)
	}
	if manifest.Schema != StateManifestSchema {
		return stateManifest{}, stateStream{}, stateCorrupt(
			nil,
			fmt.Sprintf(
				"state manifest schema %q is unsupported",
				manifest.Schema,
			),
			map[string]any{"expected_schema": StateManifestSchema},
		)
	}
	if manifest.CatalogRevision != Revision {
		return stateManifest{}, stateStream{}, stateCorrupt(
			nil,
			fmt.Sprintf(
				"state stream carries catalog revision %d, this build reads %d",
				manifest.CatalogRevision,
				Revision,
			),
			map[string]any{
				"found":    manifest.CatalogRevision,
				"expected": Revision,
			},
		)
	}
	var lines [][]byte
	for scanner.Scan() {
		lines = append(lines, append(append([]byte{}, scanner.Bytes()...), '\n'))
	}
	if err := scanner.Err(); err != nil {
		return stateManifest{}, stateStream{}, fmt.Errorf(
			"read state stream: %w",
			err,
		)
	}
	if checksum := checksumLines(lines); checksum != manifest.Checksum {
		return stateManifest{}, stateStream{}, stateCorrupt(
			nil,
			"the state stream fails its manifest checksum",
			map[string]any{
				"expected": manifest.Checksum,
				"found":    checksum,
			},
		)
	}
	stream, err := decodeRecords(lines)
	if err != nil {
		return stateManifest{}, stateStream{}, err
	}
	if stream.counts() != manifest.Counts {
		return stateManifest{}, stateStream{}, stateCorrupt(
			nil,
			"the state stream does not carry its manifest record counts",
			map[string]any{"expected": manifest.Counts, "found": stream.counts()},
		)
	}
	if err := validateState(stream); err != nil {
		return stateManifest{}, stateStream{}, err
	}
	return manifest, stream, nil
}

func decodeRecords(lines [][]byte) (stateStream, error) {
	var stream stateStream
	for index, line := range lines {
		var typed struct {
			Schema string `json:"schema"`
		}
		if err := json.Unmarshal(line, &typed); err != nil {
			return stateStream{}, stateCorrupt(
				err,
				fmt.Sprintf("state record %d is not JSON: %v", index+1, err),
				map[string]any{"record": index + 1},
			)
		}
		var err error
		switch typed.Schema {
		case StateSourceSchema:
			var record stateSource
			err = json.Unmarshal(line, &record)
			stream.sources = append(stream.sources, record)
		case StateOccurrenceSchema:
			var record stateOccurrence
			err = json.Unmarshal(line, &record)
			stream.occurrences = append(stream.occurrences, record)
		case StateBlobSchema:
			var record stateBlob
			err = json.Unmarshal(line, &record)
			stream.blobs = append(stream.blobs, record)
		case StateRevisionSchema:
			var record stateRevision
			err = json.Unmarshal(line, &record)
			stream.revisions = append(stream.revisions, record)
		case StateCheckpointSchema:
			var record stateCheckpoint
			err = json.Unmarshal(line, &record)
			stream.checkpoints = append(stream.checkpoints, record)
		default:
			return stateStream{}, stateCorrupt(
				nil,
				fmt.Sprintf(
					"state record %d carries unsupported schema %q",
					index+1,
					typed.Schema,
				),
				map[string]any{"record": index + 1, "schema": typed.Schema},
			)
		}
		if err != nil {
			return stateStream{}, stateCorrupt(
				err,
				fmt.Sprintf("state record %d is malformed: %v", index+1, err),
				map[string]any{"record": index + 1, "schema": typed.Schema},
			)
		}
	}
	return stream, nil
}

// validateState proves every reference and every retained digest before the
// import transaction opens.
func validateState(stream stateStream) error {
	sources := map[string]struct{}{}
	for _, record := range stream.sources {
		sources[record.SourceID] = struct{}{}
	}
	occurrences := map[string]struct{}{}
	for _, record := range stream.occurrences {
		if _, found := sources[record.SourceID]; !found {
			return stateCorrupt(
				nil,
				fmt.Sprintf(
					"occurrence %s references absent source %s",
					record.OccurrenceID,
					record.SourceID,
				),
				map[string]any{"occurrence_id": record.OccurrenceID},
			)
		}
		occurrences[record.OccurrenceID] = struct{}{}
	}
	blobs := map[string]struct{}{}
	for _, record := range stream.blobs {
		if err := validateBlobRecord(record); err != nil {
			return err
		}
		blobs[record.ContentHash] = struct{}{}
	}
	revisions := map[string]struct{}{}
	for _, record := range stream.revisions {
		if _, found := occurrences[record.OccurrenceID]; !found {
			return stateCorrupt(
				nil,
				fmt.Sprintf(
					"session revision %s references absent occurrence %s",
					record.RevisionHash,
					record.OccurrenceID,
				),
				map[string]any{"revision_hash": record.RevisionHash},
			)
		}
		if _, found := blobs[record.SnapshotHash]; !found {
			return stateCorrupt(
				nil,
				fmt.Sprintf(
					"session revision %s references absent snapshot %s",
					record.RevisionHash,
					record.SnapshotHash,
				),
				map[string]any{"revision_hash": record.RevisionHash},
			)
		}
		revisions[record.RevisionHash] = struct{}{}
	}
	for _, record := range stream.checkpoints {
		if _, found := occurrences[record.OccurrenceID]; !found {
			return stateCorrupt(
				nil,
				fmt.Sprintf(
					"checkpoint for %s references an absent occurrence",
					record.OccurrenceID,
				),
				map[string]any{"occurrence_id": record.OccurrenceID},
			)
		}
		if _, found := revisions[record.RevisionHash]; !found {
			return stateCorrupt(
				nil,
				fmt.Sprintf(
					"checkpoint for %s references absent revision %s",
					record.OccurrenceID,
					record.RevisionHash,
				),
				map[string]any{"occurrence_id": record.OccurrenceID},
			)
		}
	}
	return nil
}

func validateBlobRecord(record stateBlob) error {
	contentHash, err := hex.DecodeString(record.ContentHash)
	if err != nil {
		return stateCorrupt(
			err,
			"snapshot blob carries a malformed content hash",
			map[string]any{"content_hash": record.ContentHash},
		)
	}
	checksum, err := hex.DecodeString(record.Checksum)
	if err != nil {
		return stateCorrupt(
			err,
			"snapshot blob carries a malformed checksum",
			map[string]any{"content_hash": record.ContentHash},
		)
	}
	data, err := base64.StdEncoding.DecodeString(record.Data)
	if err != nil {
		return stateCorrupt(
			err,
			"snapshot blob payload is not base64",
			map[string]any{"content_hash": record.ContentHash},
		)
	}
	if int64(len(data)) != record.CompressedSize {
		return stateCorrupt(
			nil,
			"snapshot blob payload does not match its recorded size",
			map[string]any{"content_hash": record.ContentHash},
		)
	}
	restored, err := DecompressSnapshot(SnapshotBlob{
		ContentHash: contentHash,
		Codec:       record.Codec,
		Checksum:    checksum,
		Data:        data,
	})
	if err != nil {
		return stateCorrupt(
			err,
			"snapshot blob fails its retained integrity check: "+err.Error(),
			map[string]any{"content_hash": record.ContentHash},
		)
	}
	if int64(len(restored)) != record.UncompressedSize {
		return stateCorrupt(
			nil,
			"snapshot blob does not restore to its recorded size",
			map[string]any{"content_hash": record.ContentHash},
		)
	}
	return nil
}

func (catalog *Catalog) requireEmptyState(ctx context.Context) error {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return err
	}
	for _, table := range retainedTables {
		var present bool
		if err := pool.QueryRow(ctx, fmt.Sprintf(
			"SELECT EXISTS (SELECT 1 FROM %s.%s)",
			catalog.schema,
			quoteIdentifier(table),
		)).Scan(&present); err != nil {
			return fmt.Errorf("inspect retained table %s: %w", table, err)
		}
		if present {
			return &Error{
				Kind: KindCatalogStateTargetNotEmpty,
				Message: fmt.Sprintf(
					"catalog schema %s already retains %s rows",
					catalog.settings.SchemaName,
					table,
				),
				Remediation: "import into an empty catalog; merging into a" +
					" non-empty target is not part of this contract",
				Details: map[string]any{
					"catalog_schema": catalog.settings.SchemaName,
					"table":          table,
				},
			}
		}
	}
	return nil
}

func (catalog *Catalog) writeState(
	ctx context.Context,
	stream stateStream,
) (err error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return err
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin state import: %w", err)
	}
	defer func() {
		err = errors.Join(err, discard(ctx, transaction))
	}()
	if err := liftStatementTimeout(ctx, transaction); err != nil {
		return err
	}
	for _, record := range stream.sources {
		if _, err := transaction.Exec(ctx, fmt.Sprintf(
			"INSERT INTO %s.source (source_id, harness, locator_kind,"+
				" locator_root, locator_path, first_seen_at, last_seen_at,"+
				" disappeared_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)",
			catalog.schema,
		),
			append([]any{record.SourceID}, record.stateIdentity.values()...)...,
		); err != nil {
			return fmt.Errorf("import source: %w", err)
		}
	}
	for _, record := range stream.occurrences {
		if _, err := transaction.Exec(ctx, fmt.Sprintf(
			"INSERT INTO %s.source_occurrence (occurrence_id, source_id,"+
				" harness, locator_kind, locator_root, locator_path,"+
				" first_seen_at, last_seen_at, disappeared_at)"+
				" VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)",
			catalog.schema,
		),
			append(
				[]any{record.OccurrenceID, record.SourceID},
				record.stateIdentity.values()...,
			)...,
		); err != nil {
			return fmt.Errorf("import source occurrence: %w", err)
		}
	}
	for _, record := range stream.blobs {
		contentHash, checksum, data := decodeBlobBytes(record)
		if _, err := transaction.Exec(ctx, fmt.Sprintf(
			"INSERT INTO %s.snapshot_blob (content_hash, codec, codec_version,"+
				" uncompressed_size, compressed_size, checksum, data, created_at)"+
				" VALUES ($1, $2, $3, $4, $5, $6, $7, $8)",
			catalog.schema,
		),
			contentHash, record.Codec, record.CodecVersion,
			record.UncompressedSize, record.CompressedSize, checksum, data,
			record.CreatedAt,
		); err != nil {
			return fmt.Errorf("import snapshot blob: %w", err)
		}
	}
	for _, record := range stream.revisions {
		revisionHash, _ := hex.DecodeString(record.RevisionHash)
		snapshotHash, _ := hex.DecodeString(record.SnapshotHash)
		if _, err := transaction.Exec(ctx, fmt.Sprintf(
			"INSERT INTO %s.session_revision (revision_hash, session_key,"+
				" occurrence_id, harness, native_id, title, discovery_revision,"+
				" source_revision_kind, source_revision_value, snapshot_hash,"+
				" locator_kind, locator_root, locator_path, started_at,"+
				" updated_at, event_count, observed_at)"+
				" VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,"+
				" $13, $14, $15, $16, $17)",
			catalog.schema,
		),
			revisionHash, record.SessionKey, record.OccurrenceID, record.Harness,
			record.NativeID, record.Title, record.DiscoveryRevision,
			record.SourceRevisionKind, record.SourceRevisionValue, snapshotHash,
			record.LocatorKind, record.LocatorRoot, record.LocatorPath,
			record.StartedAt, record.UpdatedAt, record.EventCount,
			record.ObservedAt,
		); err != nil {
			return fmt.Errorf("import session revision: %w", err)
		}
	}
	for _, record := range stream.checkpoints {
		revisionHash, _ := hex.DecodeString(record.RevisionHash)
		snapshotHash, _ := hex.DecodeString(record.SnapshotHash)
		if _, err := transaction.Exec(ctx, fmt.Sprintf(
			"INSERT INTO %s.scan_checkpoint (occurrence_id, revision_hash,"+
				" discovery_revision, source_revision_value, snapshot_hash,"+
				" snapshot_size, source_size, record_count, file_identity,"+
				" tail_kind, change_kind, observed_at)"+
				" VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)",
			catalog.schema,
		),
			record.OccurrenceID, revisionHash, record.DiscoveryRevision,
			record.SourceRevisionValue, snapshotHash, record.SnapshotSize,
			record.SourceSize, record.RecordCount, record.FileIdentity,
			record.TailKind, record.ChangeKind, record.ObservedAt,
		); err != nil {
			return fmt.Errorf("import scan checkpoint: %w", err)
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("commit state import: %w", err)
	}
	return nil
}

// decodeBlobBytes runs after validateBlobRecord proved every encoding.
func decodeBlobBytes(record stateBlob) (contentHash, checksum, data []byte) {
	contentHash, _ = hex.DecodeString(record.ContentHash)
	checksum, _ = hex.DecodeString(record.Checksum)
	data, _ = base64.StdEncoding.DecodeString(record.Data)
	return contentHash, checksum, data
}

// requireInitialized keeps every state command behind the same typed failure
// the other catalog commands report.
func (catalog *Catalog) requireInitialized(ctx context.Context) error {
	_, err := catalog.Status(ctx)
	return err
}

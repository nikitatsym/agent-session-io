package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	sessionio "github.com/nikitatsym/agent-session-io"
	"github.com/nikitatsym/agent-session-io/internal/catalog"
	"github.com/nikitatsym/agent-session-io/internal/fileid"
	"github.com/nikitatsym/agent-session-io/internal/passage"
	"github.com/nikitatsym/agent-session-io/internal/readercache"
	"github.com/spf13/cobra"
)

const scanSchema = "sessionio.scan/v1"

// exitPartial reports an explicitly requested partial result.
const exitPartial = 4

type retentionCounts struct {
	SessionsRead    int64 `json:"sessions_read"`
	SessionsReused  int64 `json:"sessions_reused"`
	SessionsRebuilt int64 `json:"sessions_rebuilt"`
	// DerivedSessions and DerivedRows count what this scan actually built; a
	// scan that changes nothing leaves both at zero.
	DerivedSessions  int64 `json:"derived_sessions_written"`
	DerivedRows      int64 `json:"derived_rows_written"`
	DerivedReused    int64 `json:"derived_sessions_reused"`
	SnapshotsStored  int64 `json:"snapshots_stored"`
	SnapshotsReused  int64 `json:"snapshots_reused"`
	RevisionsStored  int64 `json:"session_revisions_stored"`
	RevisionsReused  int64 `json:"session_revisions_reused"`
	PendingTails     int64 `json:"pending_tails"`
	ObservedSources  int64 `json:"observed_sources"`
	ObservedOccurred int64 `json:"observed_occurrences"`
}

type scanRecord struct {
	Schema          string                  `json:"schema"`
	CatalogSchema   string                  `json:"catalog_schema"`
	Generation      int64                   `json:"catalog_generation"`
	State           string                  `json:"catalog_generation_state"`
	Sources         []string                `json:"sources"`
	Counts          catalog.ScanCounts      `json:"counts"`
	Retention       retentionCounts         `json:"retention"`
	Checkpoints     map[string]int64        `json:"checkpoints"`
	Tombstones      catalog.TombstoneCounts `json:"tombstones"`
	FailedSources   []catalog.SourceFailure `json:"failed_sources"`
	BuilderVersions map[string]string       `json:"builder_versions"`
	Reclaimed       int                     `json:"reclaimed_generations"`
}

func newScanCommand(
	configPath *string,
	newRegistry registryFactory,
) *cobra.Command {
	var formatValue string
	var partial bool
	cmd := &cobra.Command{
		Use:               "scan",
		Short:             "Reconcile sessions into a new catalog generation",
		Args:              invalidArgs(cobra.NoArgs),
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withCatalog(
				cmd,
				*configPath,
				formatValue,
				"scan",
				func(format outputFormat, opened *catalog.Catalog) error {
					record, err := runScan(cmd, opened, newRegistry, partial)
					if err != nil {
						return typedFailure(cmd.OutOrStdout(), format, err)
					}
					if err := writeScanRecord(cmd, format, record); err != nil {
						return err
					}
					if record.State == catalog.StatePartial {
						return &commandError{
							code:     exitPartial,
							err:      errors.New("scan published a partial generation"),
							reported: true,
						}
					}
					return nil
				},
			)
		},
	}
	addFormatFlag(
		cmd,
		&formatValue,
		string(formatHuman),
		"output format: human or json",
		"human",
		"json",
	)
	cmd.Flags().BoolVar(
		&partial,
		"partial",
		false,
		"publish a partial generation when a source cannot be read",
	)
	return cmd
}

func runScan(
	cmd *cobra.Command,
	opened *catalog.Catalog,
	newRegistry registryFactory,
	partial bool,
) (scanRecord, error) {
	ctx := cmd.Context()
	if _, err := opened.Status(ctx); err != nil {
		return scanRecord{}, err
	}
	registry, cache, err := openRegistry(newRegistry)
	if err != nil {
		return scanRecord{}, err
	}
	harnesses, err := selectHarnesses(registry, nil)
	if err != nil {
		return scanRecord{}, err
	}
	active, present, err := opened.ActiveGeneration(ctx)
	if err != nil {
		return scanRecord{}, err
	}
	var parent *catalog.GenerationID
	if present {
		parent = &active
	}
	generation, err := opened.BeginCandidate(ctx, parent)
	if err != nil {
		return scanRecord{}, err
	}
	builderKey := catalog.BuilderKey(builderVersions())
	run := &scanRun{
		ctx:        ctx,
		catalog:    opened,
		registry:   registry,
		cache:      cache,
		harnesses:  harnesses,
		generation: generation,
		parent:     parent,
		partial:    partial,
		now:        time.Now().UTC(),
		builderKey: builderKey,
		writer:     opened.NewDerivedWriter(builderKey),
		changes:    map[string]int64{},
	}
	record, err := run.fill()
	if err != nil {
		// The candidate never becomes visible; marking it failed makes it
		// reclaimable by the next successful scan.
		return scanRecord{}, errors.Join(err, opened.MarkFailed(ctx, generation))
	}
	record.CatalogSchema = opened.SchemaName()
	record.Reclaimed, err = opened.Reclaim(ctx)
	if err != nil {
		return scanRecord{}, err
	}
	return record, nil
}

type scanRun struct {
	ctx         context.Context
	catalog     *catalog.Catalog
	registry    *sessionio.Registry
	cache       *readercache.Store
	harnesses   []sessionio.Harness
	generation  catalog.GenerationID
	parent      *catalog.GenerationID
	partial     bool
	now         time.Time
	builderKey  string
	writer      *catalog.DerivedWriter
	retention   retentionCounts
	changes     map[string]int64
	failures    []scanFailure
	seenSources []string
	seenOccurs  []string
	sources     []string
}

func (run *scanRun) fill() (scanRecord, error) {
	sessions, err := run.observeSources()
	if err != nil {
		return scanRecord{}, err
	}
	for _, session := range sessions {
		err := run.retainSession(session)
		if err == nil {
			continue
		}
		if !run.partial {
			return scanRecord{}, err
		}
		run.failures = append(run.failures, scanFailure{
			occurrence: session.Occurrence,
			cause:      fmt.Errorf("read session source: %w", err),
		})
	}
	tombstones, err := run.tombstone()
	if err != nil {
		return scanRecord{}, err
	}
	run.retention.DerivedSessions = run.writer.SessionsWritten()
	run.retention.DerivedRows = run.writer.RowsWritten()
	counts, err := run.count()
	if err != nil {
		return scanRecord{}, err
	}
	facts := catalog.BuildFacts{
		Sources:         run.sources,
		BuilderVersions: builderVersions(),
		Counts:          counts,
	}
	if err := run.catalog.RecordBuild(run.ctx, run.generation, facts); err != nil {
		return scanRecord{}, err
	}
	if err := run.catalog.MaintainIndexes(
		run.ctx,
		run.generation,
		run.retention.DerivedRows > 0,
	); err != nil {
		return scanRecord{}, err
	}
	state, err := run.publish()
	if err != nil {
		return scanRecord{}, err
	}
	return scanRecord{
		Schema:          scanSchema,
		Generation:      int64(run.generation),
		State:           state,
		Sources:         run.sources,
		Counts:          counts,
		Retention:       run.retention,
		Checkpoints:     run.changes,
		Tombstones:      tombstones,
		FailedSources:   run.failedSources(),
		BuilderVersions: facts.BuilderVersions,
	}, nil
}

// count reports what this generation presents. A generation that wrote no row
// and presents exactly what its parent presented has the parent's counts by
// construction, so an unchanged refresh never pays for a corpus-wide aggregate.
func (run *scanRun) count() (catalog.ScanCounts, error) {
	if run.retention.DerivedRows == 0 && run.parent != nil {
		counts, ok, err := run.inheritedCounts(*run.parent)
		if err != nil || ok {
			return counts, err
		}
	}
	counts, err := run.catalog.GenerationCounts(run.ctx, run.generation)
	if err != nil {
		return catalog.ScanCounts{}, err
	}
	counts.ResolvedRelations, counts.UnresolvedRelations, err =
		run.catalog.ResolveRelations(run.ctx, run.generation)
	if err != nil {
		return catalog.ScanCounts{}, err
	}
	return counts, nil
}

func (run *scanRun) inheritedCounts(
	parent catalog.GenerationID,
) (catalog.ScanCounts, bool, error) {
	same, err := run.catalog.PresentsSameAs(run.ctx, run.generation, parent)
	if err != nil || !same {
		return catalog.ScanCounts{}, false, err
	}
	return run.catalog.RecordedCounts(run.ctx, parent)
}

// tombstone is skipped for a partial scan: a source that could not be read did
// not disappear, and recording it as gone would lose retained evidence.
func (run *scanRun) tombstone() (catalog.TombstoneCounts, error) {
	if len(run.failures) > 0 {
		return catalog.TombstoneCounts{}, nil
	}
	return run.catalog.Tombstone(
		run.ctx,
		run.seenSources,
		run.seenOccurs,
		run.now,
	)
}

func (run *scanRun) publish() (string, error) {
	if len(run.failures) == 0 {
		if err := run.catalog.Publish(run.ctx, run.generation); err != nil {
			return "", err
		}
		return catalog.StateComplete, nil
	}
	if err := run.catalog.PublishPartial(
		run.ctx,
		run.generation,
		run.failedSources(),
	); err != nil {
		return "", err
	}
	return catalog.StatePartial, nil
}

// scanFailure retains the actual cause of one absorbed source failure, so a
// partial generation reports why a source is missing rather than only that it is.
type scanFailure struct {
	occurrence sessionio.SourceOccurrence
	cause      error
}

// failedSources reduces the retained causes to one record per source.
func (run *scanRun) failedSources() []catalog.SourceFailure {
	failed := make([]catalog.SourceFailure, 0, len(run.failures))
	seen := make(map[string]struct{}, len(run.failures))
	for _, failure := range run.failures {
		sourceID := string(failure.occurrence.SourceID)
		if _, found := seen[sourceID]; found {
			continue
		}
		seen[sourceID] = struct{}{}
		failed = append(failed, catalog.SourceFailure{
			SourceID: sourceID,
			Harness:  string(failure.occurrence.Harness),
			Reason:   failure.cause.Error(),
		})
	}
	return failed
}

// observeSources retains every visible source and returns the sessions to scan.
func (run *scanRun) observeSources() ([]sessionio.SessionRef, error) {
	var sessions []sessionio.SessionRef
	for _, harness := range run.harnesses {
		harnessSources, harnessSessions, err := run.readHarness(harness)
		if err != nil && !run.partial {
			return nil, err
		}
		if err == nil {
			if err := run.observeHarness(harnessSources); err != nil {
				return nil, err
			}
			sessions = append(sessions, harnessSessions...)
			continue
		}
		run.failures = append(run.failures, scanFailure{
			occurrence: sessionio.SourceOccurrence{Harness: harness},
			cause:      fmt.Errorf("list harness %s: %w", harness, err),
		})
	}
	sort.Strings(run.sources)
	return sessions, nil
}

func (run *scanRun) observeHarness(sources []sessionio.Source) error {
	for _, source := range sources {
		locator, err := scanLocator(source.Locator)
		if err != nil {
			return err
		}
		if err := run.catalog.ObserveSource(run.ctx, catalog.RetainedSource{
			SourceID: string(source.ID),
			Harness:  string(source.Harness),
			Locator:  locator,
		}, run.now); err != nil {
			return err
		}
		run.sources = append(run.sources, string(source.ID))
		run.seenSources = append(run.seenSources, string(source.ID))
		run.retention.ObservedSources++
	}
	return nil
}

// readHarness lists one harness in a single failure domain, so a partial scan
// records one failure per harness instead of two.
func (run *scanRun) readHarness(
	harness sessionio.Harness,
) ([]sessionio.Source, []sessionio.SessionRef, error) {
	if _, found := run.registry.Adapter(harness); !found {
		return nil, nil, fmt.Errorf("registered adapter %q disappeared", harness)
	}
	selected := []sessionio.Harness{harness}
	sources, err := collectSources(run.ctx, run.registry, selected)
	if err != nil {
		return nil, nil, err
	}
	sessions, err := collectSessions(
		run.ctx,
		run.registry,
		selected,
		false,
		run.cache,
	)
	if err != nil {
		return nil, nil, err
	}
	return sources, sessions, nil
}

func (run *scanRun) retainSession(session sessionio.SessionRef) error {
	locator, err := scanLocator(session.Occurrence.Locator)
	if err != nil {
		return err
	}
	if err := run.catalog.ObserveOccurrence(
		run.ctx,
		catalog.RetainedOccurrence{
			OccurrenceID: string(session.Occurrence.ID),
			SourceID:     string(session.Occurrence.SourceID),
			Harness:      string(session.Occurrence.Harness),
			Locator:      locator,
		},
		run.now,
	); err != nil {
		return err
	}
	run.seenOccurs = append(run.seenOccurs, string(session.Occurrence.ID))
	run.retention.ObservedOccurred++
	previous, hasPrevious, err := run.catalog.LoadCheckpoint(
		run.ctx,
		string(session.Occurrence.ID),
	)
	if err != nil {
		return err
	}
	if run.reusable(session, previous, hasPrevious) {
		derived, found, err := run.catalog.FindDerivedSession(
			run.ctx,
			previous.RevisionHash,
			run.builderKey,
		)
		if err != nil {
			return err
		}
		if found {
			return run.confirmReuse(previous, derived)
		}
		// The occurrence is unchanged but its retained rows were built by a
		// superseded builder, so this session is rebuilt rather than reused.
		run.retention.SessionsRebuilt++
	}
	return run.readSession(session, locator, previous, hasPrevious)
}

// reusable holds when the adapter's discovery token is unchanged, so the
// retained revision still describes this occurrence.
func (run *scanRun) reusable(
	session sessionio.SessionRef,
	previous catalog.Checkpoint,
	hasPrevious bool,
) bool {
	return hasPrevious &&
		previous.DiscoveryRevision == string(session.DiscoveryRevision)
}

// confirmReuse presents an unchanged occurrence from its retained derived rows
// and refreshes its checkpoint sighting without reopening the transcript. It
// writes one membership row and nothing else.
func (run *scanRun) confirmReuse(
	previous catalog.Checkpoint,
	derived int64,
) error {
	run.retention.SessionsReused++
	run.retention.RevisionsReused++
	run.retention.SnapshotsReused++
	run.retention.DerivedReused++
	run.changes[catalog.ChangeUnchanged]++
	previous.ChangeKind = catalog.ChangeUnchanged
	if err := run.catalog.AddGenerationMember(
		run.ctx,
		run.generation,
		derived,
	); err != nil {
		return err
	}
	if previous.TailKind == catalog.TailPending {
		run.retention.PendingTails++
	}
	return run.catalog.PutCheckpoint(run.ctx, previous, run.now)
}

func (run *scanRun) readSession(
	session sessionio.SessionRef,
	locator catalog.Locator,
	previous catalog.Checkpoint,
	hasPrevious bool,
) error {
	adapter, found := run.registry.Adapter(session.Occurrence.Harness)
	if !found {
		return fmt.Errorf(
			"registered adapter %q disappeared",
			session.Occurrence.Harness,
		)
	}
	items, err := readItems(run.ctx, adapter, session)
	if err != nil {
		return err
	}
	run.retention.SessionsRead++
	observed := observeSnapshot(items, locator)
	blob, err := catalog.CompressSnapshot(observed.data)
	if err != nil {
		return err
	}
	identity, sourceSize, err := sourceState(locator)
	if err != nil {
		return err
	}
	if observed.covered < sourceSize {
		run.retention.PendingTails++
	}
	built := passage.Build(items)
	revision := catalog.SessionRevision{
		SessionKey:          string(session.ID),
		OccurrenceID:        string(session.Occurrence.ID),
		Harness:             string(session.Occurrence.Harness),
		NativeID:            session.NativeID,
		Title:               session.Title,
		DiscoveryRevision:   string(session.DiscoveryRevision),
		SourceRevisionKind:  string(observed.revision.Kind),
		SourceRevisionValue: observed.revision.Value,
		SnapshotHash:        blob.ContentHash,
		Locator:             locator,
		StartedAt:           session.StartedAt,
		UpdatedAt:           session.UpdatedAt,
		EventCount:          int64(len(built.Events)),
	}
	revision.RevisionHash = catalog.RevisionHash(revision)
	change, err := run.classify(observed, blob, previous, hasPrevious, identity)
	if err != nil {
		return err
	}
	reusedBlob, err := run.catalog.PutSnapshot(run.ctx, blob, run.now)
	if err != nil {
		return err
	}
	if reusedBlob {
		run.retention.SnapshotsReused++
	} else {
		run.retention.SnapshotsStored++
	}
	reusedRevision, err := run.catalog.PutSessionRevision(
		run.ctx,
		revision,
		run.now,
	)
	if err != nil {
		return err
	}
	if reusedRevision {
		run.retention.RevisionsReused++
	} else {
		run.retention.RevisionsStored++
	}
	derived, err := run.presentDerived(session, revision, built)
	if err != nil {
		return err
	}
	if err := run.catalog.AddGenerationMember(
		run.ctx,
		run.generation,
		derived,
	); err != nil {
		return err
	}
	run.changes[change]++
	return run.catalog.PutCheckpoint(run.ctx, catalog.Checkpoint{
		OccurrenceID:        string(session.Occurrence.ID),
		RevisionHash:        revision.RevisionHash,
		DiscoveryRevision:   string(session.DiscoveryRevision),
		SourceRevisionValue: observed.revision.Value,
		SnapshotHash:        blob.ContentHash,
		SnapshotSize:        blob.UncompressedSize,
		SourceSize:          sourceSize,
		RecordCount:         observed.records,
		FileIdentity:        identity,
		TailKind:            tailKind(observed.covered, sourceSize),
		ChangeKind:          change,
	}, run.now)
}

// presentDerived returns the derived session this generation should present.
// Rows are written only when this revision has none for the current builder
// versions, so a rewritten transcript that reverts to a retained revision
// writes nothing either.
func (run *scanRun) presentDerived(
	session sessionio.SessionRef,
	revision catalog.SessionRevision,
	built passage.Session,
) (int64, error) {
	derived, found, err := run.catalog.FindDerivedSession(
		run.ctx,
		revision.RevisionHash,
		run.builderKey,
	)
	if err != nil {
		return 0, err
	}
	if found {
		run.retention.DerivedReused++
		return derived, nil
	}
	retained, err := scanSession(session, revision, built)
	if err != nil {
		return 0, err
	}
	return run.writer.WriteSession(run.ctx, retained)
}

func tailKind(covered int64, sourceSize int64) string {
	if covered < sourceSize {
		return catalog.TailPending
	}
	return catalog.TailClean
}

// classify names the container change this observation represents. A prefix
// proof separates an append from a rewrite; file identity separates an
// in-place rewrite from an atomic replacement.
func (run *scanRun) classify(
	observed snapshotObservation,
	blob catalog.SnapshotBlob,
	previous catalog.Checkpoint,
	hasPrevious bool,
	identity string,
) (string, error) {
	if !hasPrevious {
		return catalog.ChangeInitial, nil
	}
	if bytesEqual(previous.SnapshotHash, blob.ContentHash) {
		return catalog.ChangeUnchanged, nil
	}
	if identity != fileid.Unavailable &&
		previous.FileIdentity != fileid.Unavailable &&
		identity != previous.FileIdentity {
		return catalog.ChangeReplaced, nil
	}
	size := blob.UncompressedSize
	if size > previous.SnapshotSize {
		prefix := sha256.Sum256(observed.data[:previous.SnapshotSize])
		if bytesEqual(prefix[:], previous.SnapshotHash) {
			return catalog.ChangeGrown, nil
		}
		return catalog.ChangeRewritten, nil
	}
	if size < previous.SnapshotSize {
		truncated, err := run.truncates(previous, blob, size)
		if err != nil {
			return "", err
		}
		if truncated {
			return catalog.ChangeTruncated, nil
		}
	}
	return catalog.ChangeRewritten, nil
}

func (run *scanRun) truncates(
	previous catalog.Checkpoint,
	blob catalog.SnapshotBlob,
	size int64,
) (bool, error) {
	stored, found, err := run.catalog.LoadSnapshot(
		run.ctx,
		previous.SnapshotHash,
	)
	if err != nil || !found {
		return false, err
	}
	if int64(len(stored)) < size {
		return false, nil
	}
	prefix := sha256.Sum256(stored[:size])
	return bytesEqual(prefix[:], blob.ContentHash), nil
}

func bytesEqual(left []byte, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// snapshotObservation is the byte-exact native snapshot of one canonical
// transcript, built from the single pass the reader already performed.
type snapshotObservation struct {
	data     []byte
	records  int64
	covered  int64
	revision sessionio.Revision
}

// observeSnapshot keeps only the canonical container's records, so the
// snapshot hash stays comparable with the file prefix of the previous scan.
func observeSnapshot(
	items []sessionio.ReadItem,
	locator catalog.Locator,
) snapshotObservation {
	var observed snapshotObservation
	for index := range items {
		file := items[index].Observation.Locator.File
		if file == nil || file.Path != locator.Path {
			continue
		}
		representation := items[index].Observation.Representation
		observed.data = append(observed.data, representation.Data...)
		observed.data = append(observed.data, representation.Framing...)
		observed.records++
		observed.revision = items[index].Observation.Revision
	}
	// JSONL records are contiguous from the first byte, so the retained bytes
	// are exactly the prefix that complete records cover.
	observed.covered = int64(len(observed.data))
	return observed
}

// sourceState reads the container facts the reader model does not carry: the
// live size, which reveals a pending final record, and the file identity,
// which separates an in-place rewrite from an atomic replacement.
func sourceState(
	locator catalog.Locator,
) (identity string, size int64, err error) {
	if locator.Kind != string(sessionio.LocatorKindFile) {
		return fileid.Unavailable, 0, nil
	}
	path := filepath.Join(locator.Root, filepath.FromSlash(locator.Path))
	info, err := os.Stat(path)
	if err != nil {
		return "", 0, fmt.Errorf("stat scanned source %s: %w", path, err)
	}
	identity, err = fileid.Token(path)
	if err != nil {
		return "", 0, err
	}
	return identity, info.Size(), nil
}

func builderVersions() map[string]string {
	return map[string]string{
		"passage":    passage.BuilderVersion,
		"projection": passage.ProjectionVersion,
	}
}

func scanSession(
	session sessionio.SessionRef,
	revision catalog.SessionRevision,
	built passage.Session,
) (catalog.ScanSession, error) {
	scan := catalog.ScanSession{
		Key:                 string(session.ID),
		Harness:             string(session.Occurrence.Harness),
		NativeID:            session.NativeID,
		Title:               session.Title,
		SourceID:            string(session.Occurrence.SourceID),
		OccurrenceID:        string(session.Occurrence.ID),
		DiscoveryRevision:   string(session.DiscoveryRevision),
		RevisionHash:        revision.RevisionHash,
		SourceRevisionKind:  revision.SourceRevisionKind,
		SourceRevisionValue: revision.SourceRevisionValue,
		Locator:             revision.Locator,
		StartedAt:           session.StartedAt,
		UpdatedAt:           session.UpdatedAt,
	}
	for _, event := range built.Events {
		scanned := catalog.ScanEvent{
			Key:         event.Key,
			NativeKey:   event.NativeKey,
			Kind:        event.Kind,
			Role:        event.Role,
			Observation: event.Observation,
			OccurredAt:  event.OccurredAt,
		}
		for _, evidence := range event.Evidence {
			locator, err := scanLocator(evidence.Locator)
			if err != nil {
				return catalog.ScanSession{}, err
			}
			scanned.Evidence = append(scanned.Evidence, catalog.ScanEvidence{
				Observation: evidence.Observation,
				Locator:     locator,
			})
		}
		scan.Events = append(scan.Events, scanned)
	}
	for _, built := range built.Passages {
		limitations := make([]catalog.ProjectionLimitation, 0, len(built.Limitations))
		for _, limitation := range built.Limitations {
			limitations = append(limitations, catalog.ProjectionLimitation{
				Kind:         limitation.Kind,
				RemovedBytes: limitation.RemovedBytes,
			})
		}
		scan.Passages = append(scan.Passages, catalog.ScanPassage{
			Kind:              string(built.Kind),
			BuilderVersion:    passage.BuilderVersion,
			ProjectionKind:    catalog.ProjectionKindLexical,
			ProjectionVersion: passage.ProjectionVersion,
			Events:            built.Events,
			Body:              built.Body,
			ContentHash:       built.ContentHash,
			OccurredAt:        built.OccurredAt,
			Limitations:       limitations,
			Part:              built.Part,
			Parts:             built.Parts,
			Facets: []catalog.FacetFilter{{
				Namespace: "source",
				Key:       "harness",
				Value:     string(session.Occurrence.Harness),
			}},
		})
	}
	scan.Relations = sessionRelations(session, built)
	return scan, nil
}

// sessionRelations keeps the adapter's per-record relations and adds the
// session-level native hints, whose targets scan resolves from the revisions
// retained in the same generation.
func sessionRelations(
	session sessionio.SessionRef,
	built passage.Session,
) []catalog.ScanRelation {
	relations := make([]catalog.ScanRelation, 0, len(built.Relations))
	for _, relation := range built.Relations {
		relations = append(relations, catalog.ScanRelation{
			Kind:        relation.Kind,
			Origin:      relation.Origin,
			FromKind:    relation.FromKind,
			FromRef:     relation.FromRef,
			ToKind:      relation.ToKind,
			ToRef:       relation.ToRef,
			Observation: relation.Observation,
		})
	}
	for _, hint := range session.Native.Relationships {
		relations = append(relations, catalog.ScanRelation{
			Kind:     nativeRelationKind(hint.Kind),
			Origin:   string(sessionio.RelationOriginNative),
			FromKind: string(sessionio.NodeKindSession),
			FromRef:  string(session.ID),
			ToKind:   catalog.ToKindSessionNative,
			ToRef:    hint.TargetNativeID,
		})
	}
	return relations
}

func nativeRelationKind(kind sessionio.NativeRelationshipKind) string {
	if kind == sessionio.NativeRelationshipKindForkParent {
		return string(sessionio.RelationKindBranchParent)
	}
	return string(kind)
}

// scanLocator keeps every locator variant addressable through the same
// retained columns; a database or opaque locator stores its table or scheme in
// the root column.
func scanLocator(
	locator sessionio.SourceLocator,
) (catalog.Locator, error) {
	switch {
	case locator.File != nil:
		built := catalog.Locator{
			Kind: string(locator.Kind),
			Root: locator.File.Root,
			Path: locator.File.Path,
		}
		record, err := ordinal(locator.File.Record, "record")
		if err != nil {
			return catalog.Locator{}, err
		}
		built.Record = record
		line, err := ordinal(locator.File.Line, "line")
		if err != nil {
			return catalog.Locator{}, err
		}
		built.Line = line
		if span := locator.File.ByteRange; span != nil {
			start, end := span.Start, span.End
			built.ByteStart = &start
			built.ByteEnd = &end
		}
		return built, nil
	case locator.Database != nil:
		return catalog.Locator{
			Kind: string(locator.Kind),
			Root: locator.Database.Table,
			Path: locator.Database.Path,
		}, nil
	case locator.Opaque != nil:
		return catalog.Locator{
			Kind: string(locator.Kind),
			Root: locator.Opaque.Scheme,
			Path: locator.Opaque.Value,
		}, nil
	default:
		return catalog.Locator{Kind: string(locator.Kind)}, nil
	}
}

func ordinal(value *uint64, name string) (*int64, error) {
	if value == nil {
		return nil, nil
	}
	if *value > math.MaxInt64 {
		return nil, fmt.Errorf(
			"evidence locator %s %d does not fit a catalog bigint",
			name,
			*value,
		)
	}
	converted := int64(*value)
	return &converted, nil
}

func writeScanRecord(
	cmd *cobra.Command,
	format outputFormat,
	record scanRecord,
) error {
	if format == formatJSON {
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(record); err != nil {
			return fmt.Errorf("write scan record: %w", err)
		}
		return nil
	}
	if _, err := fmt.Fprintf(
		cmd.OutOrStdout(),
		"catalog schema %q published generation %d (%s)\n"+
			"sources: %d\n"+
			"sessions: %d (read %d, reused %d, rebuilt %d)\n"+
			"derived: %d rows written, %d sessions built, %d sessions reused\n"+
			"events: %d\n"+
			"evidence: %d\n"+
			"relations: %d (resolved %d, unresolved %d)\n"+
			"passages: %d\n"+
			"projections: %d (%d with a limitation)\n"+
			"snapshots: %d stored, %d reused\n"+
			"checkpoints: %s\n"+
			"tombstones: %d sources, %d occurrences\n"+
			"reclaimed generations: %d\n",
		record.CatalogSchema,
		record.Generation,
		record.State,
		len(record.Sources),
		record.Counts.Sessions,
		record.Retention.SessionsRead,
		record.Retention.SessionsReused,
		record.Retention.SessionsRebuilt,
		record.Retention.DerivedRows,
		record.Retention.DerivedSessions,
		record.Retention.DerivedReused,
		record.Counts.Events,
		record.Counts.Evidence,
		record.Counts.Relations,
		record.Counts.ResolvedRelations,
		record.Counts.UnresolvedRelations,
		record.Counts.Passages,
		record.Counts.Projections,
		record.Counts.Limitations,
		record.Retention.SnapshotsStored,
		record.Retention.SnapshotsReused,
		formatChanges(record.Checkpoints),
		record.Tombstones.Sources,
		record.Tombstones.Occurrences,
		record.Reclaimed,
	); err != nil {
		return fmt.Errorf("write scan result: %w", err)
	}
	for _, failure := range record.FailedSources {
		if _, err := fmt.Fprintf(
			cmd.ErrOrStderr(),
			"partial generation: source %s (%s) failed: %s\n",
			failure.SourceID,
			failure.Harness,
			failure.Reason,
		); err != nil {
			return fmt.Errorf("write partial scan diagnostic: %w", err)
		}
	}
	return nil
}

func formatChanges(changes map[string]int64) string {
	if len(changes) == 0 {
		return "none"
	}
	kinds := make([]string, 0, len(changes))
	for kind := range changes {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	parts := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		parts = append(parts, fmt.Sprintf("%s %d", kind, changes[kind]))
	}
	return joinComma(parts)
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

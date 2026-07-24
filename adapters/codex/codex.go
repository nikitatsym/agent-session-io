// Package codex reads Codex rollout JSONL files.
package codex

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	sessionio "github.com/nikitatsym/agent-session-io"
	"github.com/nikitatsym/agent-session-io/internal/sourceio"
)

const (
	// DefaultMaxRecordBytes bounds one native record materialized by default.
	DefaultMaxRecordBytes  int64  = 64 << 20
	adapterVersion                = "1"
	minimumDecoderMemory   uint64 = 128 << 20
	unlimitedDecoderMemory uint64 = 1 << 30
)

// Config configures a Codex adapter. Records near the limit can need several
// times the limit in transient process memory while JSONL is indexed and read.
// zstd decoder memory is at least 128 MiB and grows with a finite record limit;
// unlimited records retain a 1 GiB decoder window bound.
type Config struct {
	Home           string
	MaxRecordBytes int64
}

// DefaultConfig returns deterministic configuration without filesystem access.
func DefaultConfig() Config {
	return Config{MaxRecordBytes: DefaultMaxRecordBytes}
}

// Adapter reads one configured Codex home.
type Adapter struct {
	home           string
	maxRecordBytes int64
	sourceID       sessionio.SourceID
}

// New constructs an adapter and resolves an empty home once.
func New(config Config) (*Adapter, error) {
	if config.MaxRecordBytes != sourceio.UnlimitedRecordBytes && config.MaxRecordBytes <= 0 {
		return nil, fmt.Errorf("codex: max record bytes must be positive or %d", sourceio.UnlimitedRecordBytes)
	}
	home := config.Home
	if home == "" {
		home = os.Getenv("CODEX_HOME")
		if home == "" {
			userHome, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("codex: resolve user home: %w", err)
			}
			home = filepath.Join(userHome, ".codex")
		}
	}
	absHome, err := filepath.Abs(home)
	if err != nil {
		return nil, fmt.Errorf("codex: resolve home %q: %w", home, err)
	}
	return &Adapter{
		home:           absHome,
		maxRecordBytes: config.MaxRecordBytes,
		sourceID:       sessionio.SourceID(derivedID("source", string(sessionio.HarnessCodex), absHome)),
	}, nil
}

// Descriptor declares the supported Codex rollout coverage.
func (adapter *Adapter) Descriptor() sessionio.AdapterDescriptor {
	return sessionio.AdapterDescriptor{
		Harness:      sessionio.HarnessCodex,
		Version:      adapterVersion,
		Capabilities: capabilities(),
	}
}

func capabilities() []sessionio.CapabilityStatus {
	values := []sessionio.Capability{
		sessionio.CapabilityDiscovery, sessionio.CapabilityMessages,
		sessionio.CapabilityRichContent, sessionio.CapabilityTools,
		sessionio.CapabilityReasoning, sessionio.CapabilityBranches,
		sessionio.CapabilityUsage, sessionio.CapabilityEnvironment,
		sessionio.CapabilityRepository, sessionio.CapabilityIncrementalReading,
	}
	statuses := make([]sessionio.CapabilityStatus, 0, len(values))
	for _, capability := range values {
		statuses = append(statuses, sessionio.CapabilityStatus{Capability: capability, Support: sessionio.SupportFull})
	}
	return statuses
}

// Sources reports the one configured canonical Codex source.
func (adapter *Adapter) Sources(ctx context.Context) (sessionio.Stream[sessionio.Source], error) {
	if adapter == nil {
		return nil, errors.New("codex: nil adapter")
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	discovery, err := adapter.discover()
	if err != nil {
		return nil, adapter.error("sources", "", nil, err)
	}
	source := adapter.source(discovery)
	emitted := false
	return sessionio.NewStream(func(ctx context.Context) (sessionio.Source, error) {
		if err := contextError(ctx); err != nil {
			return sessionio.Source{}, err
		}
		if emitted {
			return sessionio.Source{}, io.EOF
		}
		emitted = true
		return source, nil
	}, func() error { return nil })
}

// Sessions lists one reference per rollout occurrence with canonical metadata.
func (adapter *Adapter) Sessions(ctx context.Context, request sessionio.SessionRequest) (sessionio.Stream[sessionio.SessionRef], error) {
	if adapter == nil {
		return nil, errors.New("codex: nil adapter")
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if len(request.Sources) > 0 {
		found := false
		for _, sourceID := range request.Sources {
			if sourceID == adapter.sourceID {
				found = true
				break
			}
		}
		if !found {
			return emptySessionStream()
		}
	}
	discovery, err := adapter.discover()
	if err != nil {
		return nil, adapter.error("sessions", "", nil, err)
	}
	occurrences := discovery.occurrences
	refs := make([]sessionio.SessionRef, 0, len(occurrences))
	for _, occurrence := range occurrences {
		ref, err := adapter.readSessionRef(ctx, occurrence)
		if err != nil {
			return nil, err
		}
		if ref != nil {
			refs = append(refs, *ref)
		}
	}
	index := 0
	return sessionio.NewStream(func(ctx context.Context) (sessionio.SessionRef, error) {
		if err := contextError(ctx); err != nil {
			return sessionio.SessionRef{}, err
		}
		if index == len(refs) {
			return sessionio.SessionRef{}, io.EOF
		}
		ref := refs[index]
		index++
		return ref, nil
	}, func() error { return nil })
}

// Read streams every complete native record for a previously listed occurrence.
func (adapter *Adapter) Read(ctx context.Context, session sessionio.SessionRef) (sessionio.Stream[sessionio.ReadItem], error) {
	if adapter == nil {
		return nil, errors.New("codex: nil adapter")
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	occurrence, err := adapter.occurrenceFromSession(session)
	if err != nil {
		return nil, err
	}
	correlations := make(map[string]*toolCardinality)
	var header *observedHeader
	generation, err := adapter.openWithObserver(ctx, occurrence, string(session.ID), func(record sourceio.JSONLRecord) error {
		classifyToolCorrelation(record.Data, correlations)
		if record.Record == 1 {
			locator := adapter.recordLocator(occurrence, record)
			meta, kind, err := parseMetadata(record.Data)
			if err != nil {
				return &locatedError{locator: locator, err: err}
			}
			if kind != "session_meta" || meta.ID == "" {
				return &locatedError{locator: locator, err: errors.New("first complete record is not session metadata")}
			}
			native, err := nativeHeader(record.Data, locator)
			if err != nil {
				return &locatedError{locator: locator, err: err}
			}
			header = &observedHeader{
				meta:       meta,
				timestamp:  native.timestamp,
				diagnostic: native.diagnostic,
				data:       append([]byte(nil), record.Data...),
				framing:    append([]byte(nil), record.Framing...),
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if header == nil {
		return nil, adapter.error("read", string(session.ID), nil, errors.New("canonical metadata is unavailable"))
	}
	physicalSize, modTimeUnixNano := generation.PhysicalMetadata()
	fresh := adapter.sessionRef(
		occurrence,
		header.meta,
		header.timestamp,
		adapter.discoveryRevisionAt(
			occurrence,
			append(header.data, header.framing...),
			physicalSize,
			modTimeUnixNano,
		),
		header.diagnostic,
	)
	branchParent, err := adapter.branchParent(ctx, fresh)
	if err != nil {
		return nil, adapter.error("read", string(fresh.ID), nil, err)
	}
	state := &readState{
		adapter:           adapter,
		session:           fresh,
		occurrence:        occurrence,
		generation:        generation,
		discoveryRevision: string(fresh.DiscoveryRevision),
		toolCalls:         make(map[string][]toolEventEvidence),
		toolResults:       make(map[string][]toolEventEvidence),
		correlations:      correlations,
		branchParent:      branchParent,
	}
	return sessionio.NewStream(state.next, generation.Close)
}

func (adapter *Adapter) branchParent(ctx context.Context, session sessionio.SessionRef) (sessionio.SessionID, error) {
	var target string
	for _, hint := range session.Native.Relationships {
		if hint.Kind == sessionio.NativeRelationshipKindForkParent {
			target = hint.TargetNativeID
			break
		}
	}
	if target == "" {
		return "", nil
	}
	stream, err := adapter.Sessions(ctx, sessionio.SessionRequest{})
	if err != nil {
		return "", err
	}
	defer stream.Close()
	var found sessionio.SessionID
	for {
		candidate, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if candidate.NativeID != target {
			continue
		}
		if found != "" {
			return "", nil
		}
		found = candidate.ID
	}
	return found, nil
}

type observedHeader struct {
	meta       nativeMeta
	timestamp  *time.Time
	diagnostic *sessionio.Diagnostic
	data       []byte
	framing    []byte
}

func (adapter *Adapter) sessionRef(
	occurrence occurrence,
	meta nativeMeta,
	timestamp *time.Time,
	discoveryRevision sessionio.DiscoveryRevision,
	headerDiagnostic *sessionio.Diagnostic,
) sessionio.SessionRef {
	locator := adapter.baseLocator(occurrence)
	occurrenceID := sessionio.OccurrenceID(derivedID("occurrence", string(adapter.sourceID), adapter.home, occurrence.relative))
	ref := sessionio.SessionRef{ID: sessionio.SessionID(derivedID("session", string(occurrenceID), meta.ID)), NativeID: meta.ID, DiscoveryRevision: discoveryRevision, Occurrence: sessionio.SourceOccurrence{ID: occurrenceID, SourceID: adapter.sourceID, Harness: sessionio.HarnessCodex, Locator: sessionio.SourceLocator{Kind: sessionio.LocatorKindFile, File: &locator}}, StartedAt: timestamp, Native: meta.native()}
	if meta.SessionID == "" {
		ref.Diagnostics = append(ref.Diagnostics, diagnostic("codex_legacy_missing_session_id", "legacy Codex metadata has no session_id; using id", nil))
	}
	if headerDiagnostic != nil {
		ref.Diagnostics = append(ref.Diagnostics, *headerDiagnostic)
	}
	suffix := ".jsonl"
	if occurrence.compressed {
		suffix = ".jsonl.zst"
	}
	filenameID, valid := rolloutFilenameID(filepath.Base(occurrence.relative), suffix)
	if valid && filenameID != strings.ToLower(meta.ID) {
		sourceLocator := sessionio.SourceLocator{
			Kind: sessionio.LocatorKindFile,
			File: &locator,
		}
		ref.Diagnostics = append(ref.Diagnostics, diagnostic(
			"codex_filename_metadata_mismatch",
			fmt.Sprintf(
				"Codex rollout filename id %q differs from metadata id %q; metadata id wins",
				filenameID,
				meta.ID,
			),
			&sourceLocator,
		))
	}
	return ref
}

func (adapter *Adapter) readSessionRef(ctx context.Context, occurrence occurrence) (*sessionio.SessionRef, error) {
	if occurrence.compressed {
		return adapter.readCompressedSessionRef(ctx, occurrence)
	}
	path := filepath.Join(adapter.home, filepath.FromSlash(occurrence.relative))
	base := adapter.baseLocator(occurrence)
	sourceLocator := sessionio.SourceLocator{Kind: sessionio.LocatorKindFile, File: &base}
	file, err := os.Open(path)
	if err != nil {
		return nil, adapter.error("sessions", "", &sourceLocator, err)
	}
	defer file.Close()
	input := boundedHeaderReader(file, adapter.maxRecordBytes)
	data, readErr := bufio.NewReader(input).ReadBytes('\n')
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, adapter.error("sessions", "", &sourceLocator, fmt.Errorf("read metadata header: %w", readErr))
	}
	if len(data) == 0 && errors.Is(readErr, io.EOF) {
		return nil, nil
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	terminated := len(data) > 0 && data[len(data)-1] == '\n'
	var framing []byte
	if terminated {
		framing = []byte{'\n'}
		data = data[:len(data)-1]
		if len(data) > 0 && data[len(data)-1] == '\r' {
			data = data[:len(data)-1]
			framing = []byte{'\r', '\n'}
		}
	}
	locator := headerLocator(base, len(data))
	if int64(len(data)) > adapter.maxRecordBytes && adapter.maxRecordBytes != sourceio.UnlimitedRecordBytes {
		return nil, adapter.error("sessions", "", &locator, fmt.Errorf("record=1 line=1 limit=%d observed-at-least=%d", adapter.maxRecordBytes, len(data)))
	}
	if occurrence.active && !terminated {
		return nil, nil
	}
	meta, kind, err := parseMetadata(data)
	if err != nil {
		return nil, adapter.error("sessions", "", &locator, err)
	}
	if kind != "session_meta" {
		return nil, adapter.error("sessions", "", &locator, fmt.Errorf("first complete record is not session metadata"))
	}
	if meta.ID == "" {
		return nil, adapter.error("sessions", "", &locator, fmt.Errorf("session metadata id is empty"))
	}
	native, err := nativeHeader(data, locator)
	if err != nil {
		return nil, adapter.error("sessions", "", &locator, err)
	}
	header := append(append([]byte(nil), data...), framing...)
	discoveryRevision, err := adapter.discoveryRevision(occurrence, header)
	if err != nil {
		return nil, adapter.error("sessions", "", &locator, err)
	}
	ref := adapter.sessionRef(
		occurrence,
		meta,
		native.timestamp,
		discoveryRevision,
		native.diagnostic,
	)
	return &ref, nil
}

func headerLocator(base sessionio.FileLocator, dataLength int) sessionio.SourceLocator {
	record := uint64(1)
	line := uint64(1)
	base.Record = &record
	base.Line = &line
	base.ByteRange = &sessionio.ByteRange{Start: 0, End: int64(dataLength)}
	return sessionio.SourceLocator{Kind: sessionio.LocatorKindFile, File: &base}
}

func (adapter *Adapter) readCompressedSessionRef(ctx context.Context, occurrence occurrence) (*sessionio.SessionRef, error) {
	base := adapter.baseLocator(occurrence)
	path := filepath.Join(adapter.home, filepath.FromSlash(occurrence.relative))
	file, err := os.Open(path)
	if err != nil {
		locator := sessionio.SourceLocator{Kind: sessionio.LocatorKindFile, File: &base}
		return nil, adapter.error("sessions", "", &locator, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		locator := sessionio.SourceLocator{Kind: sessionio.LocatorKindFile, File: &base}
		return nil, adapter.error("sessions", "", &locator, err)
	}
	decoder, err := adapter.openZstdDecoder(contextBoundReader{ctx: ctx, reader: file})
	if err != nil {
		locator := sessionio.SourceLocator{Kind: sessionio.LocatorKindFile, File: &base}
		return nil, adapter.error("sessions", "", &locator, err)
	}
	defer decoder.Close()
	input := boundedHeaderReader(decoder, adapter.maxRecordBytes)
	raw, readErr := bufio.NewReader(input).ReadBytes('\n')
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		locator := sessionio.SourceLocator{Kind: sessionio.LocatorKindFile, File: &base}
		return nil, adapter.error("sessions", "", &locator, fmt.Errorf("read decoded metadata header: %w", readErr))
	}
	if len(raw) == 0 && errors.Is(readErr, io.EOF) {
		return nil, nil
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	terminated := raw[len(raw)-1] == '\n'
	data, framing := sourceio.SplitJSONLRecord(raw, terminated)
	locator := (sourceio.DecodedJSONLRecord{Record: 1, Line: 1}).SourceLocator(base)
	if adapter.maxRecordBytes != sourceio.UnlimitedRecordBytes && int64(len(data)) > adapter.maxRecordBytes {
		return nil, adapter.error("sessions", "", &locator, fmt.Errorf("record=1 line=1 limit=%d observed-at-least=%d", adapter.maxRecordBytes, len(data)))
	}
	meta, kind, err := parseMetadata(data)
	if err != nil {
		return nil, adapter.error("sessions", "", &locator, err)
	}
	if kind != "session_meta" || meta.ID == "" {
		return nil, adapter.error("sessions", "", &locator, errors.New("first complete record is not session metadata"))
	}
	native, err := nativeHeader(data, locator)
	if err != nil {
		return nil, adapter.error("sessions", "", &locator, err)
	}
	return ptr(adapter.sessionRef(occurrence, meta, native.timestamp, adapter.discoveryRevisionAt(occurrence, append(data, framing...), info.Size(), info.ModTime().UnixNano()), native.diagnostic)), nil
}

func (adapter *Adapter) baseLocator(occurrence occurrence) sessionio.FileLocator {
	return sessionio.FileLocator{Root: adapter.home, Path: occurrence.relative}
}

func (adapter *Adapter) recordLocator(occurrence occurrence, record sourceio.JSONLRecord) sessionio.SourceLocator {
	if occurrence.compressed {
		return (sourceio.DecodedJSONLRecord{Record: record.Record, Line: record.Line}).SourceLocator(adapter.baseLocator(occurrence))
	}
	return record.SourceLocator(adapter.baseLocator(occurrence))
}

type recordGeneration interface {
	Next(context.Context) (sourceio.JSONLRecord, error)
	Close() error
	Revision() sessionio.Revision
	PhysicalMetadata() (int64, int64)
}

func (adapter *Adapter) openWithObserver(
	ctx context.Context,
	occurrence occurrence,
	sessionID string,
	observer sourceio.RecordObserver,
) (recordGeneration, error) {
	base := adapter.baseLocator(occurrence)
	if occurrence.compressed {
		generation, err := sourceio.OpenDecodedJSONLGeneration(ctx, sourceio.DecodedFileSpec{OpenPath: filepath.Join(adapter.home, filepath.FromSlash(occurrence.relative)), Locator: base, Codec: "zstd", OpenDecoder: adapter.openZstdDecoder}, sourceio.DecodedOpenOptions{SizePolicy: sourceio.RecordSizePolicy{MaxBytes: adapter.maxRecordBytes}, ObserveRecord: func(record sourceio.DecodedJSONLRecord) error {
			return observer(sourceio.JSONLRecord{Record: record.Record, Line: record.Line, Data: record.Data, Framing: record.Framing})
		}})
		if err != nil {
			return nil, adapter.error("read", sessionID, sourceErrorLocator(err, base), err)
		}
		return decodedRecordGeneration{generation: generation}, nil
	}
	result, err := sourceio.OpenJSONLGeneration(ctx, sourceio.FileSpec{OpenPath: filepath.Join(adapter.home, filepath.FromSlash(occurrence.relative)), Locator: base}, sourceio.OpenOptions{TailMode: tailMode(occurrence.active), SizePolicy: sourceio.RecordSizePolicy{MaxBytes: adapter.maxRecordBytes}, ObserveRecord: observer})
	if err != nil {
		return nil, adapter.error("read", sessionID, sourceErrorLocator(err, base), err)
	}
	if result.Generation == nil {
		locator := sessionio.SourceLocator{Kind: sessionio.LocatorKindFile, File: &base}
		return nil, adapter.error("read", sessionID, &locator, errors.New("source disappeared"))
	}
	return result.Generation, nil
}

type decodedRecordGeneration struct {
	generation *sourceio.DecodedJSONLGeneration
}

func (generation decodedRecordGeneration) Next(ctx context.Context) (sourceio.JSONLRecord, error) {
	record, err := generation.generation.Next(ctx)
	return sourceio.JSONLRecord{Record: record.Record, Line: record.Line, Data: record.Data, Framing: record.Framing}, err
}

func (generation decodedRecordGeneration) Close() error {
	return generation.generation.Close()
}

func (generation decodedRecordGeneration) Revision() sessionio.Revision {
	return generation.generation.Revision()
}

func (generation decodedRecordGeneration) PhysicalMetadata() (int64, int64) {
	return generation.generation.PhysicalMetadata()
}

func (adapter *Adapter) openZstdDecoder(reader io.Reader) (io.ReadCloser, error) {
	decoder, err := zstd.NewReader(reader, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(decoderMemoryLimit(adapter.maxRecordBytes)))
	if err != nil {
		return nil, err
	}
	return decoder.IOReadCloser(), nil
}

func decoderMemoryLimit(maxRecordBytes int64) uint64 {
	if maxRecordBytes == sourceio.UnlimitedRecordBytes {
		return unlimitedDecoderMemory
	}
	const overhead uint64 = 64 << 20
	const maximum uint64 = 1 << 63
	if maxRecordBytes < 0 || uint64(maxRecordBytes) > maximum-overhead {
		return maximum
	}
	limit := uint64(maxRecordBytes) + overhead
	if limit < minimumDecoderMemory {
		return minimumDecoderMemory
	}
	return limit
}

func boundedHeaderReader(reader io.Reader, maxRecordBytes int64) io.Reader {
	const framingSlack int64 = 2
	const maxInt64 = int64(^uint64(0) >> 1)
	if maxRecordBytes == sourceio.UnlimitedRecordBytes ||
		maxRecordBytes > maxInt64-framingSlack {
		return reader
	}
	return io.LimitReader(reader, maxRecordBytes+framingSlack)
}

type contextBoundReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader contextBoundReader) Read(data []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	count, err := reader.reader.Read(data)
	if contextErr := reader.ctx.Err(); contextErr != nil {
		return count, contextErr
	}
	return count, err
}

type locatedError struct {
	locator sessionio.SourceLocator
	err     error
}

func (located *locatedError) Error() string {
	if located == nil {
		return "codex: located error"
	}
	return fmt.Sprint(located.err)
}

func (located *locatedError) Unwrap() error {
	if located == nil {
		return nil
	}
	return located.err
}

func sourceErrorLocator(err error, fallback sessionio.FileLocator) *sessionio.SourceLocator {
	var located *locatedError
	if errors.As(err, &located) {
		locator := located.locator
		return &locator
	}
	var tooLarge *sourceio.RecordTooLargeError
	if errors.As(err, &tooLarge) {
		locator := tooLarge.Locator
		return &locator
	}
	var malformed *sourceio.MalformedJSONLError
	if errors.As(err, &malformed) {
		locator := malformed.Locator
		return &locator
	}
	var changed *sourceio.ChangedSourceError
	if errors.As(err, &changed) {
		locator := changed.Locator
		return &locator
	}
	locator := sessionio.SourceLocator{Kind: sessionio.LocatorKindFile, File: &fallback}
	return &locator
}

func tailMode(active bool) sourceio.TailMode {
	if active {
		return sourceio.TailModeGrowing
	}
	return sourceio.TailModeFinal
}

func (adapter *Adapter) occurrenceFromSession(session sessionio.SessionRef) (occurrence, error) {
	locator := session.Occurrence.Locator
	if session.Occurrence.SourceID != adapter.sourceID || session.Occurrence.Harness != sessionio.HarnessCodex || session.Occurrence.Locator.File == nil || session.Occurrence.Locator.File.Root != adapter.home {
		return occurrence{}, adapter.error("read", string(session.ID), &locator, errors.New("session does not belong to this Codex home"))
	}
	relative := filepath.ToSlash(session.Occurrence.Locator.File.Path)
	discovery, err := adapter.discover()
	if err != nil {
		return occurrence{}, adapter.error("read", string(session.ID), &locator, err)
	}
	for _, candidate := range discovery.occurrences {
		if candidate.relative == relative {
			return candidate, nil
		}
	}
	return occurrence{}, adapter.error("read", string(session.ID), &locator, errors.New("session occurrence is not a readable rollout"))
}

func (adapter *Adapter) discoveryRevision(
	occurrence occurrence,
	header []byte,
) (sessionio.DiscoveryRevision, error) {
	info, err := os.Stat(filepath.Join(adapter.home, filepath.FromSlash(occurrence.relative)))
	if err != nil {
		return "", fmt.Errorf("stat discovery revision: %w", err)
	}
	return adapter.discoveryRevisionAt(occurrence, header, info.Size(), info.ModTime().UnixNano()), nil
}

func (adapter *Adapter) discoveryRevisionAt(
	occurrence occurrence,
	header []byte,
	size int64,
	modTimeUnixNano int64,
) sessionio.DiscoveryRevision {
	occurrenceID := sessionio.OccurrenceID(derivedID("occurrence", string(adapter.sourceID), adapter.home, occurrence.relative))
	parts := []string{string(occurrenceID)}
	if size != 0 || modTimeUnixNano != 0 {
		parts = append(parts, fmt.Sprintf("%d", size), fmt.Sprintf("%d", modTimeUnixNano))
	}
	parts = append(parts, string(header))
	return sessionio.DiscoveryRevision(derivedID("discovery", parts...))
}

func (adapter *Adapter) error(operation, sessionID string, locator *sessionio.SourceLocator, err error) error {
	return &sessionio.ReaderError{Operation: operation, Harness: sessionio.HarnessCodex, AdapterVersion: adapterVersion, SessionID: sessionio.SessionID(sessionID), Locator: locator, Err: err}
}

func derivedID(kind string, values ...string) string {
	hash := sha256.New()
	write := func(value string) {
		length := make([]byte, 4)
		binary.BigEndian.PutUint32(length, uint32(len(value)))
		_, _ = hash.Write(length)
		_, _ = hash.Write([]byte(value))
	}
	write("sessionio/id/v1")
	write(kind)
	for _, value := range values {
		write(value)
	}
	return kind + ":sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return errors.New("codex: context must not be nil")
	}
	return ctx.Err()
}
func ptr[T any](value T) *T { return &value }
func emptySessionStream() (sessionio.Stream[sessionio.SessionRef], error) {
	return sessionio.NewStream(func(context.Context) (sessionio.SessionRef, error) { return sessionio.SessionRef{}, io.EOF }, func() error { return nil })
}

func diagnostic(code, message string, locator *sessionio.SourceLocator) sessionio.Diagnostic {
	return sessionio.Diagnostic{Code: code, Severity: sessionio.DiagnosticSeverityWarning, Message: message, Locator: locator}
}

type nativeMeta struct {
	ID             string          `json:"id"`
	SessionID      string          `json:"session_id"`
	Fork           string          `json:"forked_from_id"`
	Control        string          `json:"parent_thread_id"`
	Nickname       string          `json:"agent_nickname"`
	Role           string          `json:"agent_role"`
	Path           string          `json:"agent_path"`
	CWD            string          `json:"cwd"`
	Provider       string          `json:"model_provider"`
	Model          string          `json:"model"`
	Effort         string          `json:"effort"`
	ApprovalPolicy string          `json:"approval_policy"`
	SandboxPolicy  json.RawMessage `json:"sandbox_policy"`
	Timezone       string          `json:"timezone"`
	CurrentDate    string          `json:"current_date"`
	Git            *gitMeta        `json:"git"`
	History        *historyMeta    `json:"history_base"`
	OwnStart       *uint64         `json:"subagent_history_start_ordinal"`
}

type nativeRecord struct {
	nativeMeta
	Type             string          `json:"type"`
	Timestamp        string          `json:"timestamp"`
	Payload          json.RawMessage `json:"payload"`
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	Summary          json.RawMessage `json:"summary"`
	EncryptedContent *string         `json:"encrypted_content"`
	Name             string          `json:"name"`
	CallID           string          `json:"call_id"`
	Arguments        json.RawMessage `json:"arguments"`
	Input            json.RawMessage `json:"input"`
	Action           json.RawMessage `json:"action"`
	Output           json.RawMessage `json:"output"`
	Message          *string         `json:"message"`
	Text             *string         `json:"text"`
	Images           []string        `json:"images"`
	LocalImages      []string        `json:"local_images"`
	Status           string          `json:"status"`
	ExitCode         *int            `json:"exit_code"`
}

type historyMeta struct {
	ThreadID   string  `json:"thread_id"`
	EndOrdinal *uint64 `json:"end_ordinal_exclusive"`
	EndByte    *uint64 `json:"end_byte_offset"`
}

type gitMeta struct {
	Root       string `json:"root"`
	Remote     string `json:"repository_url"`
	Branch     string `json:"branch"`
	CommitHash string `json:"commit_hash"`
}

func (meta nativeMeta) native() sessionio.NativeSessionMetadata {
	result := sessionio.NativeSessionMetadata{}
	if meta.SessionID != "" {
		result.Identities = append(result.Identities, sessionio.NativeIdentity{Kind: sessionio.NativeIdentityKindSession, Value: meta.SessionID})
	}
	if meta.Fork != "" {
		result.Relationships = append(result.Relationships, sessionio.NativeRelationshipHint{Kind: sessionio.NativeRelationshipKindForkParent, TargetNativeID: meta.Fork})
	}
	if meta.Control != "" {
		result.Relationships = append(result.Relationships, sessionio.NativeRelationshipHint{Kind: sessionio.NativeRelationshipKindControlParent, TargetNativeID: meta.Control})
	}
	if meta.Nickname != "" || meta.Role != "" || meta.Path != "" {
		result.Agent = &sessionio.NativeAgentMetadata{Nickname: meta.Nickname, Role: meta.Role, Path: meta.Path}
	}
	if meta.History != nil {
		result.History = &sessionio.NativeHistoryMetadata{BaseNativeID: meta.History.ThreadID, EndOrdinalExclusive: meta.History.EndOrdinal, EndByteOffset: meta.History.EndByte, OwnStartOrdinal: meta.OwnStart}
	} else if meta.OwnStart != nil {
		result.History = &sessionio.NativeHistoryMetadata{OwnStartOrdinal: meta.OwnStart}
	}
	return result
}

func parseMetadata(data []byte) (nativeMeta, string, error) {
	var outer nativeRecord
	if err := json.Unmarshal(data, &outer); err != nil {
		return nativeMeta{}, "", err
	}
	raw := data
	if len(outer.Payload) > 0 {
		raw = outer.Payload
	}
	var meta nativeMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nativeMeta{}, "", err
	}
	if len(outer.Payload) == 0 {
		meta = outer.nativeMeta
	}
	if outer.Type == "" && meta.ID != "" {
		outer.Type = "session_meta"
	}
	return meta, outer.Type, nil
}

type readState struct {
	adapter           *Adapter
	session           sessionio.SessionRef
	occurrence        occurrence
	generation        recordGeneration
	discoveryRevision string
	toolCalls         map[string][]toolEventEvidence
	toolResults       map[string][]toolEventEvidence
	correlations      map[string]*toolCardinality
	branchParent      sessionio.SessionID
}

type toolCardinality struct {
	calls   int
	results int
}

type toolEventEvidence struct {
	event       sessionio.EventID
	observation sessionio.ObservationID
	locator     sessionio.SourceLocator
}

func (state *readState) next(ctx context.Context) (sessionio.ReadItem, error) {
	record, err := state.generation.Next(ctx)
	if errors.Is(err, io.EOF) {
		return sessionio.ReadItem{}, io.EOF
	}
	if err != nil {
		base := state.adapter.baseLocator(state.occurrence)
		return sessionio.ReadItem{}, state.adapter.error("read", string(state.session.ID), sourceErrorLocator(err, base), err)
	}
	locator := state.adapter.recordLocator(state.occurrence, record)
	header, err := nativeHeader(record.Data, locator)
	if err != nil {
		return sessionio.ReadItem{}, state.adapter.error("read", string(state.session.ID), &locator, err)
	}
	representation := record.NativeRepresentation()
	if state.occurrence.compressed {
		representation = (sourceio.DecodedJSONLRecord{Record: record.Record, Line: record.Line, Data: record.Data, Framing: record.Framing}).NativeRepresentation("zstd")
	}
	item := sessionio.ReadItem{Session: state.session, Observation: sessionio.NativeObservation{ID: sessionio.ObservationID(derivedID("observation", string(state.session.ID), fmt.Sprintf("%d", record.Record), digest(record.Data, record.Framing))), NativeKind: header.kind, Timestamp: header.timestamp, Locator: locator, Revision: state.generation.Revision(), Representation: representation}}
	event, diagnostic, err := state.adapter.normalize(item.Observation, record.Data)
	if err != nil {
		return sessionio.ReadItem{}, state.adapter.error("read", string(state.session.ID), &locator, err)
	}
	if diagnostic != nil {
		item.Diagnostics = append(item.Diagnostics, *diagnostic)
	}
	if header.diagnostic != nil {
		item.Diagnostics = append(item.Diagnostics, *header.diagnostic)
	}
	item.Events = append(item.Events, event)
	if record.Record == 1 && state.branchParent != "" {
		item.Relations = append(item.Relations, sessionio.Relation{ID: sessionio.RelationID(derivedID("relation", string(state.session.Occurrence.ID), string(sessionio.RelationKindBranchParent), string(sessionio.NodeKindSession), string(state.session.ID), string(sessionio.NodeKindSession), string(state.branchParent), string(item.Observation.ID))), Kind: sessionio.RelationKindBranchParent, From: sessionio.NodeRef{Kind: sessionio.NodeKindSession, ID: string(state.session.ID)}, To: sessionio.NodeRef{Kind: sessionio.NodeKindSession, ID: string(state.branchParent)}, Origin: sessionio.RelationOriginNative, Evidence: []sessionio.EvidenceRef{{Observation: item.Observation.ID, Locator: item.Observation.Locator}}})
	}
	if event.ToolCall != nil {
		callID := event.ToolCall.CallID
		call := toolEventEvidence{event: event.ID, observation: item.Observation.ID, locator: item.Observation.Locator}
		state.toolCalls[callID] = append(state.toolCalls[callID], call)
		if state.uniqueToolPair(callID) && len(state.toolResults[callID]) == 1 {
			item.Relations = append(item.Relations, state.toolPairRelation(call, state.toolResults[callID][0]))
		}
	}
	if event.ToolResult != nil {
		callID := event.ToolResult.CallID
		result := toolEventEvidence{event: event.ID, observation: item.Observation.ID, locator: item.Observation.Locator}
		state.toolResults[callID] = append(state.toolResults[callID], result)
		if state.uniqueToolPair(callID) && len(state.toolCalls[callID]) == 1 {
			item.Relations = append(item.Relations, state.toolPairRelation(state.toolCalls[callID][0], result))
		}
	}
	return item, nil
}

func (state *readState) uniqueToolPair(callID string) bool {
	cardinality := state.correlations[callID]
	return cardinality != nil && cardinality.calls == 1 && cardinality.results == 1
}

func (state *readState) toolPairRelation(call, result toolEventEvidence) sessionio.Relation {
	return sessionio.Relation{
		ID: sessionio.RelationID(derivedID(
			"relation",
			string(state.session.Occurrence.ID),
			string(sessionio.RelationKindToolPair),
			string(sessionio.NodeKindEvent),
			string(call.event),
			string(sessionio.NodeKindEvent),
			string(result.event),
			string(call.observation),
			string(result.observation),
		)),
		Kind:   sessionio.RelationKindToolPair,
		From:   sessionio.NodeRef{Kind: sessionio.NodeKindEvent, ID: string(call.event)},
		To:     sessionio.NodeRef{Kind: sessionio.NodeKindEvent, ID: string(result.event)},
		Origin: sessionio.RelationOriginDeterministic,
		Evidence: []sessionio.EvidenceRef{
			{Observation: call.observation, Locator: call.locator},
			{Observation: result.observation, Locator: result.locator},
		},
	}
}

func digest(parts ...[]byte) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = hash.Write(part)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func classifyToolCorrelation(data []byte, values map[string]*toolCardinality) {
	var outer nativeRecord
	if json.Unmarshal(data, &outer) != nil {
		return
	}
	classification := ""
	switch outer.Type {
	case "response_item":
		var inner nativeRecord
		if json.Unmarshal(outer.Payload, &inner) != nil {
			return
		}
		outer.CallID = inner.CallID
		switch inner.Type {
		case "function_call", "custom_tool_call", "local_shell_call":
			classification = "call"
		case "function_call_output", "custom_tool_call_output", "local_shell_call_output":
			classification = "result"
		}
	case "event_msg":
		var inner nativeRecord
		if json.Unmarshal(outer.Payload, &inner) != nil {
			return
		}
		outer.CallID = inner.CallID
		switch inner.Type {
		case "exec_command_begin", "patch_apply_begin", "mcp_tool_call_begin":
			classification = "call"
		case "exec_command_end", "patch_apply_end", "mcp_tool_call_end":
			classification = "result"
		}
	default:
		switch outer.Type {
		case "function_call", "custom_tool_call", "local_shell_call":
			classification = "call"
		case "function_call_output", "custom_tool_call_output", "local_shell_call_output":
			classification = "result"
		}
	}
	if outer.CallID == "" || classification == "" {
		return
	}
	value := values[outer.CallID]
	if value == nil {
		value = &toolCardinality{}
		values[outer.CallID] = value
	}
	switch classification {
	case "call":
		value.calls++
	case "result":
		value.results++
	}
}

type nativeHeaderResult struct {
	kind       string
	timestamp  *time.Time
	diagnostic *sessionio.Diagnostic
	cause      error
}

func (result nativeHeaderResult) Error() string {
	return fmt.Sprint(result.cause)
}

func (result nativeHeaderResult) Unwrap() error {
	return result.cause
}

func nativeHeader(data []byte, locator sessionio.SourceLocator) (nativeHeaderResult, error) {
	var header nativeRecord
	if err := json.Unmarshal(data, &header); err != nil {
		return nativeHeaderResult{}, err
	}
	if header.Type == "" && header.ID != "" {
		header.Type = "session_meta"
	}
	if header.Timestamp == "" {
		return nativeHeaderResult{kind: header.Type}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, header.Timestamp)
	if err != nil {
		cause := fmt.Errorf("invalid Codex record timestamp: %w", err)
		return nativeHeaderResult{
			kind: header.Type,
			diagnostic: &sessionio.Diagnostic{
				Code:     "codex_invalid_timestamp",
				Severity: sessionio.DiagnosticSeverityWarning,
				Message:  cause.Error(),
				Locator:  &locator,
				Cause:    cause,
			},
			cause: err,
		}, nil
	}
	return nativeHeaderResult{kind: header.Type, timestamp: &parsed}, nil
}

func (adapter *Adapter) normalize(observation sessionio.NativeObservation, data []byte) (sessionio.Event, *sessionio.Diagnostic, error) {
	var outer nativeRecord
	if err := json.Unmarshal(data, &outer); err != nil {
		return sessionio.Event{}, nil, err
	}
	evidence := []sessionio.EvidenceRef{{Observation: observation.ID, Locator: observation.Locator}}
	event := func(kind sessionio.EventKind) sessionio.Event {
		return sessionio.Event{ID: sessionio.EventID(derivedID("event", string(observation.ID), "0", string(kind))), Kind: kind, Timestamp: observation.Timestamp, Evidence: evidence}
	}
	typeName := outer.Type
	if typeName == "" && outer.ID != "" {
		typeName = "session_meta"
	}
	envelopeType := typeName
	projectionRaw := json.RawMessage(data)
	if typeName == "response_item" || typeName == "event_msg" {
		if len(outer.Payload) == 0 || string(outer.Payload) == "null" {
			return sessionio.Event{}, nil, fmt.Errorf("%s payload is required", typeName)
		}
		var inner nativeRecord
		if err := json.Unmarshal(outer.Payload, &inner); err != nil {
			return sessionio.Event{}, nil, fmt.Errorf("%s payload: %w", typeName, err)
		}
		if inner.Type == "" {
			return sessionio.Event{}, nil, fmt.Errorf("%s payload type is required", typeName)
		}
		typeName = inner.Type
		projectionRaw = cloneRaw(outer.Payload)
		outer = inner
	}
	if typeName == "" {
		return sessionio.Event{}, nil, errors.New("record type is required")
	}
	switch typeName {
	case "session_meta", "turn_context":
		meta, _, err := parseMetadata(data)
		if err != nil {
			return sessionio.Event{}, nil, err
		}
		facts, err := metadataFacts(typeName, meta)
		if err != nil {
			return sessionio.Event{}, nil, err
		}
		if len(facts) == 0 {
			item := event(sessionio.EventKindUnknown)
			item.Unknown = &sessionio.UnknownEvent{NativeType: typeName}
			return item, nil, nil
		}
		item := event(sessionio.EventKindFacts)
		item.Facts = &sessionio.FactEvent{Facts: facts}
		return item, nil, nil
	case "message":
		if outer.Role == "" {
			return sessionio.Event{}, nil, errors.New("message role is required")
		}
		item := event(sessionio.EventKindMessage)
		next := 0
		content, err := decodeContentBlocks(item.ID, outer.Content, "message", &next, true, false)
		if err != nil {
			return sessionio.Event{}, nil, fmt.Errorf("message content: %w", err)
		}
		item.Message = &sessionio.MessageEvent{Role: role(outer.Role), Content: content}
		return item, nil, nil
	case "user_message", "agent_message":
		item := event(sessionio.EventKindMessage)
		next := 0
		var content []sessionio.ContentBlock
		var err error
		if envelopeType == "event_msg" || outer.Message != nil {
			content, err = eventMessageContent(item.ID, outer, &next)
		} else {
			content, err = decodeContentBlocks(item.ID, outer.Content, "agent_message", &next, false, false)
		}
		if err != nil {
			return sessionio.Event{}, nil, fmt.Errorf("%s content: %w", typeName, err)
		}
		messageRole := sessionio.MessageRoleAssistant
		if typeName == "user_message" {
			messageRole = sessionio.MessageRoleUser
		}
		item.Message = &sessionio.MessageEvent{Role: messageRole, Content: content}
		return item, nil, nil
	case "reasoning":
		item := event(sessionio.EventKindReasoning)
		next := 0
		content, err := decodeContentBlocks(item.ID, outer.Content, "reasoning", &next, true, envelopeType == "response_item")
		if err != nil {
			return sessionio.Event{}, nil, fmt.Errorf("reasoning content: %w", err)
		}
		if outer.EncryptedContent != nil {
			content = append(content, encryptedBlock(item.ID, next, "encrypted_reasoning", *outer.EncryptedContent))
			next++
		}
		if envelopeType == "response_item" && len(outer.Summary) == 0 {
			return sessionio.Event{}, nil, errors.New("reasoning summary is required")
		}
		summary, err := decodeContentBlocks(item.ID, outer.Summary, "reasoning_summary", &next, false, envelopeType != "response_item")
		if err != nil {
			return sessionio.Event{}, nil, fmt.Errorf("reasoning summary: %w", err)
		}
		item.Reasoning = &sessionio.ReasoningEvent{Content: content, Summary: summary}
		return item, nil, nil
	case "agent_reasoning":
		if outer.Text == nil {
			return sessionio.Event{}, nil, errors.New("agent_reasoning text is required")
		}
		item := event(sessionio.EventKindReasoning)
		item.Reasoning = &sessionio.ReasoningEvent{Content: []sessionio.ContentBlock{textBlock(item.ID, 0, *outer.Text)}}
		return item, nil, nil
	case "function_call":
		if outer.CallID == "" || outer.Name == "" {
			return sessionio.Event{}, nil, errors.New("function_call requires call_id and name")
		}
		input, err := decodedStringPayload(outer.Arguments, "function_call arguments")
		if err != nil {
			return sessionio.Event{}, nil, err
		}
		if !json.Valid(input.Data) {
			return sessionio.Event{}, nil, errors.New("function_call arguments contain invalid JSON")
		}
		input.MediaType = "application/json"
		item := event(sessionio.EventKindToolCall)
		item.ToolCall = &sessionio.ToolCallEvent{CallID: outer.CallID, Name: outer.Name, Input: input}
		return item, nil, nil
	case "custom_tool_call":
		if outer.CallID == "" || outer.Name == "" {
			return sessionio.Event{}, nil, errors.New("custom_tool_call requires call_id and name")
		}
		input, err := decodedStringPayload(outer.Input, "custom_tool_call input")
		if err != nil {
			return sessionio.Event{}, nil, err
		}
		item := event(sessionio.EventKindToolCall)
		item.ToolCall = &sessionio.ToolCallEvent{CallID: outer.CallID, Name: outer.Name, Input: input}
		return item, nil, nil
	case "local_shell_call":
		callID := outer.CallID
		if callID == "" {
			callID = outer.ID
		}
		if callID == "" {
			return sessionio.Event{}, nil, errors.New("local_shell_call requires call_id or id")
		}
		input, err := objectPayload(outer.Action, "local_shell_call action")
		if err != nil {
			return sessionio.Event{}, nil, err
		}
		item := event(sessionio.EventKindToolCall)
		item.ToolCall = &sessionio.ToolCallEvent{CallID: callID, Name: "local_shell", Input: input}
		return item, nil, nil
	case "function_call_output", "custom_tool_call_output", "local_shell_call_output":
		if outer.CallID == "" {
			return sessionio.Event{}, nil, fmt.Errorf("%s requires call_id", typeName)
		}
		output, err := decodedOutputPayload(outer.Output, typeName+" output")
		if err != nil {
			return sessionio.Event{}, nil, err
		}
		item := event(sessionio.EventKindToolResult)
		item.ToolResult = &sessionio.ToolResultEvent{CallID: outer.CallID, Status: sessionio.ToolResultStatusSuccess, Output: output}
		return item, nil, nil
	case "exec_command_begin", "patch_apply_begin", "mcp_tool_call_begin":
		if outer.CallID == "" {
			return sessionio.Event{}, nil, fmt.Errorf("%s requires call_id", typeName)
		}
		item := event(sessionio.EventKindToolCall)
		item.ToolCall = &sessionio.ToolCallEvent{
			CallID: outer.CallID,
			Name:   strings.TrimSuffix(typeName, "_begin"),
			Input:  sessionio.Payload{MediaType: "application/json", Data: cloneRaw(projectionRaw)},
		}
		return item, nil, nil
	case "exec_command_end", "patch_apply_end", "mcp_tool_call_end":
		if outer.CallID == "" {
			return sessionio.Event{}, nil, fmt.Errorf("%s requires call_id", typeName)
		}
		item := event(sessionio.EventKindToolResult)
		item.ToolResult = &sessionio.ToolResultEvent{
			CallID: outer.CallID,
			Status: operationalStatus(outer),
			Output: sessionio.Payload{MediaType: "application/json", Data: cloneRaw(projectionRaw)},
		}
		return item, nil, nil
	case "token_count":
		var token struct {
			Info struct {
				Total struct {
					Input           *int64 `json:"input_tokens"`
					Output          *int64 `json:"output_tokens"`
					Reasoning       *int64 `json:"reasoning_tokens"`
					ReasoningOutput *int64 `json:"reasoning_output_tokens"`
					CacheRead       *int64 `json:"cached_input_tokens"`
					Total           *int64 `json:"total_tokens"`
				} `json:"total_token_usage"`
			} `json:"info"`
		}
		if err := json.Unmarshal(projectionRaw, &token); err != nil {
			return sessionio.Event{}, nil, err
		}
		reasoningTokens := token.Info.Total.ReasoningOutput
		if reasoningTokens == nil {
			reasoningTokens = token.Info.Total.Reasoning
		}
		usage := sessionio.UsageEvent{InputTokens: token.Info.Total.Input, OutputTokens: token.Info.Total.Output, ReasoningTokens: reasoningTokens, CacheReadTokens: token.Info.Total.CacheRead, TotalTokens: token.Info.Total.Total}
		if usage.InputTokens == nil && usage.OutputTokens == nil && usage.ReasoningTokens == nil && usage.CacheReadTokens == nil && usage.TotalTokens == nil {
			return sessionio.Event{}, nil, errors.New("token_count total_token_usage has no supported counters")
		}
		item := event(sessionio.EventKindUsage)
		item.Usage = &usage
		return item, nil, nil
	case "compacted", "compaction", "context_compacted", "task_started", "task_complete",
		"turn_started", "turn_complete", "turn_aborted", "entered_review_mode", "exited_review_mode":
		item := event(sessionio.EventKindMarker)
		item.Marker = &sessionio.MarkerEvent{Name: typeName}
		return item, nil, nil
	default:
		item := event(sessionio.EventKindUnknown)
		item.Unknown = &sessionio.UnknownEvent{NativeType: typeName}
		locator := observation.Locator
		return item, &sessionio.Diagnostic{
			Code:     "codex_unknown_record_kind",
			Severity: sessionio.DiagnosticSeverityWarning,
			Message:  fmt.Sprintf("Codex record kind %q has no normalized projection", typeName),
			Locator:  &locator,
		}, nil
	}
}

func metadataFacts(typeName string, meta nativeMeta) ([]sessionio.Fact, error) {
	facts := make([]sessionio.Fact, 0, 12)
	appendFact := func(kind sessionio.FactKind, value string) {
		if value != "" {
			facts = append(facts, sessionio.Fact{Kind: kind, Value: value})
		}
	}
	if typeName == "session_meta" {
		appendFact(sessionio.FactKindLaunchDirectory, meta.CWD)
	} else {
		appendFact(sessionio.FactKindWorkingDirectory, meta.CWD)
	}
	appendFact(sessionio.FactKindModel, meta.Model)
	appendFact(sessionio.FactKindProvider, meta.Provider)
	appendFact(sessionio.FactKindEffort, meta.Effort)
	appendFact(sessionio.FactKindApprovalPolicy, meta.ApprovalPolicy)
	sandboxPolicy, err := factValue(meta.SandboxPolicy)
	if err != nil {
		return nil, fmt.Errorf("sandbox_policy: %w", err)
	}
	appendFact(sessionio.FactKindSandboxPolicy, sandboxPolicy)
	appendFact(sessionio.FactKindTimezone, meta.Timezone)
	appendFact(sessionio.FactKindCurrentDate, meta.CurrentDate)
	if meta.Git != nil {
		appendFact(sessionio.FactKindGitRoot, meta.Git.Root)
		appendFact(sessionio.FactKindGitRemote, meta.Git.Remote)
		appendFact(sessionio.FactKindGitBranch, meta.Git.Branch)
		appendFact(sessionio.FactKindGitCommit, meta.Git.CommitHash)
	}
	return facts, nil
}

func factValue(raw json.RawMessage) (string, error) {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	if !json.Valid(raw) {
		return "", errors.New("invalid JSON")
	}
	return value, nil
}

func eventMessageContent(eventID sessionio.EventID, record nativeRecord, next *int) ([]sessionio.ContentBlock, error) {
	if record.Message == nil {
		return nil, errors.New("message is required")
	}
	content := []sessionio.ContentBlock{textBlock(eventID, *next, *record.Message)}
	*next++
	for _, reference := range append(append([]string(nil), record.Images...), record.LocalImages...) {
		block, err := mediaBlock(eventID, *next, "image/*", reference)
		if err != nil {
			return nil, err
		}
		content = append(content, block)
		*next++
	}
	return content, nil
}

func decodeContentBlocks(
	eventID sessionio.EventID,
	raw json.RawMessage,
	contextName string,
	next *int,
	allowString bool,
	allowNull bool,
) ([]sessionio.ContentBlock, error) {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		if allowNull {
			return nil, nil
		}
		return nil, errors.New("is required")
	}
	if allowString {
		if strings.HasPrefix(value, `"`) {
			var text string
			if err := json.Unmarshal(raw, &text); err != nil {
				return nil, fmt.Errorf("decode text content: %w", err)
			}
			block := textBlock(eventID, *next, text)
			*next++
			return []sessionio.ContentBlock{block}, nil
		}
	}
	var nativeBlocks []json.RawMessage
	if err := json.Unmarshal(raw, &nativeBlocks); err != nil {
		return nil, fmt.Errorf("content must be a string or array of content blocks: %w", err)
	}
	blocks := make([]sessionio.ContentBlock, 0, len(nativeBlocks))
	for _, nativeBlock := range nativeBlocks {
		block, err := decodeContentBlock(eventID, *next, contextName, nativeBlock)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, block)
		*next++
	}
	return blocks, nil
}

func decodeContentBlock(
	eventID sessionio.EventID,
	index int,
	contextName string,
	raw json.RawMessage,
) (sessionio.ContentBlock, error) {
	var native struct {
		Type             string  `json:"type"`
		Text             *string `json:"text"`
		ImageURL         *string `json:"image_url"`
		AudioURL         *string `json:"audio_url"`
		EncryptedContent *string `json:"encrypted_content"`
	}
	if err := json.Unmarshal(raw, &native); err != nil {
		return sessionio.ContentBlock{}, err
	}
	if native.Type == "" {
		return sessionio.ContentBlock{}, errors.New("content block type is required")
	}
	allowed := func(expectedContext string) bool {
		return contextName == expectedContext
	}
	switch native.Type {
	case "input_text", "output_text":
		return requiredTextBlock(eventID, index, native.Type, contextName, native.Text, allowed("message") || allowed("agent_message"))
	case "input_image":
		if !allowed("message") {
			return sessionio.ContentBlock{}, fmt.Errorf("content type %q is invalid in %s", native.Type, contextName)
		}
		if native.ImageURL == nil {
			return sessionio.ContentBlock{}, errors.New("input_image image_url is required")
		}
		return mediaBlock(eventID, index, "image/*", *native.ImageURL)
	case "input_audio":
		if !allowed("message") {
			return sessionio.ContentBlock{}, fmt.Errorf("content type %q is invalid in %s", native.Type, contextName)
		}
		if native.AudioURL == nil {
			return sessionio.ContentBlock{}, errors.New("input_audio audio_url is required")
		}
		return mediaBlock(eventID, index, "audio/*", *native.AudioURL)
	case "encrypted_content":
		if !allowed("agent_message") {
			return sessionio.ContentBlock{}, fmt.Errorf("content type %q is invalid in %s", native.Type, contextName)
		}
		if native.EncryptedContent == nil {
			return sessionio.ContentBlock{}, errors.New("encrypted_content value is required")
		}
		return encryptedBlock(eventID, index, native.Type, *native.EncryptedContent), nil
	case "reasoning_text", "text":
		return requiredTextBlock(eventID, index, native.Type, contextName, native.Text, allowed("reasoning"))
	case "summary_text":
		return requiredTextBlock(eventID, index, native.Type, contextName, native.Text, allowed("reasoning_summary"))
	default:
		return opaqueBlock(eventID, index, native.Type, sessionio.ContentAvailabilityAvailable, "application/json", raw), nil
	}
}

func requiredTextBlock(
	eventID sessionio.EventID,
	index int,
	nativeType string,
	contextName string,
	text *string,
	allowed bool,
) (sessionio.ContentBlock, error) {
	if !allowed {
		return sessionio.ContentBlock{}, fmt.Errorf("content type %q is invalid in %s", nativeType, contextName)
	}
	if text == nil {
		return sessionio.ContentBlock{}, fmt.Errorf("%s text is required", nativeType)
	}
	return textBlock(eventID, index, *text), nil
}

func decodedStringPayload(raw json.RawMessage, field string) (sessionio.Payload, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return sessionio.Payload{}, fmt.Errorf("%s is required", field)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return sessionio.Payload{}, fmt.Errorf("%s must be a string: %w", field, err)
	}
	return sessionio.Payload{MediaType: "text/plain; charset=utf-8", Data: []byte(value)}, nil
}

func objectPayload(raw json.RawMessage, field string) (sessionio.Payload, error) {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return sessionio.Payload{}, fmt.Errorf("%s is required", field)
	}
	if !json.Valid(raw) || !strings.HasPrefix(value, "{") {
		return sessionio.Payload{}, fmt.Errorf("%s must be a JSON object", field)
	}
	return sessionio.Payload{MediaType: "application/json", Data: cloneRaw(raw)}, nil
}

func decodedOutputPayload(raw json.RawMessage, field string) (sessionio.Payload, error) {
	payload, err := decodedStringPayload(raw, field)
	if err == nil {
		return payload, nil
	}
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" || !json.Valid(raw) || !strings.HasPrefix(value, "[") {
		return sessionio.Payload{}, fmt.Errorf("%s must be a string or JSON array", field)
	}
	return sessionio.Payload{MediaType: "application/json", Data: cloneRaw(raw)}, nil
}

func operationalStatus(record nativeRecord) sessionio.ToolResultStatus {
	if record.ExitCode != nil {
		if *record.ExitCode == 0 {
			return sessionio.ToolResultStatusSuccess
		}
		return sessionio.ToolResultStatusError
	}
	switch record.Status {
	case "completed", "success":
		return sessionio.ToolResultStatusSuccess
	case "failed", "error":
		return sessionio.ToolResultStatusError
	case "in_progress", "running":
		return sessionio.ToolResultStatusRunning
	case "pending", "incomplete":
		return sessionio.ToolResultStatusPending
	default:
		return sessionio.ToolResultStatusUnknown
	}
}

func cloneRaw(value json.RawMessage) []byte {
	if len(value) == 0 {
		return []byte("null")
	}
	return append([]byte(nil), value...)
}
func role(value string) sessionio.MessageRole {
	switch value {
	case "user":
		return sessionio.MessageRoleUser
	case "assistant", "agent":
		return sessionio.MessageRoleAssistant
	case "developer":
		return sessionio.MessageRoleDeveloper
	case "system":
		return sessionio.MessageRoleSystem
	case "tool":
		return sessionio.MessageRoleTool
	default:
		return sessionio.MessageRoleUnknown
	}
}
func textBlock(eventID sessionio.EventID, index int, text string) sessionio.ContentBlock {
	return sessionio.ContentBlock{ID: sessionio.ContentID(derivedID("content", string(eventID), fmt.Sprintf("%d", index), string(sessionio.ContentKindText))), Kind: sessionio.ContentKindText, Availability: sessionio.ContentAvailabilityAvailable, Text: &sessionio.TextContent{Text: text}}
}

func mediaBlock(
	eventID sessionio.EventID,
	index int,
	fallbackMediaType string,
	reference string,
) (sessionio.ContentBlock, error) {
	mediaType, data, inline, err := decodeDataURI(reference, fallbackMediaType)
	if err != nil {
		return sessionio.ContentBlock{}, err
	}
	availability := sessionio.ContentAvailabilityExternal
	media := &sessionio.MediaContent{MediaType: mediaType, Reference: reference}
	if inline {
		availability = sessionio.ContentAvailabilityAvailable
		media.Data = data
		media.Reference = ""
	}
	return sessionio.ContentBlock{
		ID:           sessionio.ContentID(derivedID("content", string(eventID), fmt.Sprintf("%d", index), string(sessionio.ContentKindMedia))),
		Kind:         sessionio.ContentKindMedia,
		Availability: availability,
		Media:        media,
	}, nil
}

func decodeDataURI(reference, fallbackMediaType string) (string, []byte, bool, error) {
	if !strings.HasPrefix(reference, "data:") {
		return fallbackMediaType, nil, false, nil
	}
	comma := strings.IndexByte(reference, ',')
	if comma < 0 {
		return "", nil, false, errors.New("data URI has no payload separator")
	}
	metadata := strings.TrimPrefix(reference[:comma], "data:")
	encoded := reference[comma+1:]
	parts := strings.Split(metadata, ";")
	mediaType := fallbackMediaType
	if parts[0] != "" {
		mediaType = parts[0]
	}
	if parts[len(parts)-1] == "base64" {
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return "", nil, false, fmt.Errorf("decode base64 data URI: %w", err)
		}
		return mediaType, data, true, nil
	}
	decoded, err := url.PathUnescape(encoded)
	if err != nil {
		return "", nil, false, fmt.Errorf("decode data URI: %w", err)
	}
	return mediaType, []byte(decoded), true, nil
}

func encryptedBlock(eventID sessionio.EventID, index int, nativeType, value string) sessionio.ContentBlock {
	return opaqueBlock(
		eventID,
		index,
		nativeType,
		sessionio.ContentAvailabilityEncrypted,
		"text/plain; charset=utf-8",
		[]byte(value),
	)
}

func opaqueBlock(
	eventID sessionio.EventID,
	index int,
	nativeType string,
	availability sessionio.ContentAvailability,
	mediaType string,
	data []byte,
) sessionio.ContentBlock {
	return sessionio.ContentBlock{
		ID:           sessionio.ContentID(derivedID("content", string(eventID), fmt.Sprintf("%d", index), string(sessionio.ContentKindOpaque))),
		Kind:         sessionio.ContentKindOpaque,
		Availability: availability,
		Opaque:       &sessionio.OpaqueContent{NativeType: nativeType, MediaType: mediaType, Data: append([]byte(nil), data...)},
	}
}

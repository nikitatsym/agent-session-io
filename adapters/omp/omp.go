// Package omp reads OMP session trees without invoking the mutating native loader.
package omp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	sessionio "github.com/nikitatsym/agent-session-io"
	"github.com/nikitatsym/agent-session-io/internal/fileid"
	"github.com/nikitatsym/agent-session-io/internal/sourceio"
)

const (
	DefaultMaxRecordBytes int64 = 64 << 20
	adapterVersion              = "1"
)

// Config selects the OMP data directory containing sessions and blobs.
type Config struct {
	AgentDir       string
	MaxRecordBytes int64
	Cache          sessionio.ListingCacheSource
}

func DefaultConfig() Config { return Config{MaxRecordBytes: DefaultMaxRecordBytes} }

type Adapter struct {
	root           string
	maxRecordBytes int64
	sourceID       sessionio.SourceID
	cache          sessionio.ListingCache
	mu             sync.RWMutex
	refs           []sessionio.SessionRef
}

func New(config Config) (*Adapter, error) {
	if config.MaxRecordBytes != sourceio.UnlimitedRecordBytes && config.MaxRecordBytes <= 0 {
		return nil, fmt.Errorf("omp: max record bytes must be positive or %d", sourceio.UnlimitedRecordBytes)
	}
	root := config.AgentDir
	if root == "" {
		var err error
		root, err = defaultRoot()
		if err != nil {
			return nil, err
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("omp: resolve agent directory: %w", err)
	}
	adapter := &Adapter{root: root, maxRecordBytes: config.MaxRecordBytes, sourceID: sessionio.SourceID(sourceio.DerivedID("source", "omp", root))}
	if config.Cache != nil {
		adapter.cache, _ = config.Cache.ListingCache(string(adapter.sourceID))
	}
	return adapter, nil
}

func defaultRoot() (string, error) {
	profile := os.Getenv("PI_PROFILE")
	if profile == "" {
		if override := os.Getenv("PI_CODING_AGENT_DIR"); override != "" {
			return override, nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("omp: resolve user home: %w", err)
	}
	config := os.Getenv("PI_CONFIG_DIR")
	if config == "" {
		config = ".omp"
	}
	root := filepath.Join(home, config)
	if filepath.IsAbs(config) {
		root = config
	}
	if profile != "" {
		root = filepath.Join(root, "profiles", profile)
	}
	if data := os.Getenv("XDG_DATA_HOME"); data != "" {
		candidate := filepath.Join(data, "omp")
		if profile != "" {
			candidate = filepath.Join(candidate, "profiles", profile)
		}
		info, err := os.Stat(candidate)
		if err == nil && info.IsDir() {
			return candidate, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("omp: inspect data directory: %w", err)
		}
	}
	return filepath.Join(root, "agent"), nil
}

func (adapter *Adapter) Descriptor() sessionio.AdapterDescriptor {
	return sessionio.AdapterDescriptor{Harness: sessionio.HarnessOMP, Version: adapterVersion, Capabilities: capabilities()}
}

func capabilities() []sessionio.CapabilityStatus {
	result := []sessionio.CapabilityStatus{}
	for _, capability := range []sessionio.Capability{sessionio.CapabilityMessages, sessionio.CapabilityRichContent, sessionio.CapabilityTools, sessionio.CapabilityReasoning, sessionio.CapabilityUsage, sessionio.CapabilityEnvironment, sessionio.CapabilityIncrementalReading} {
		result = append(result, sessionio.CapabilityStatus{Capability: capability, Support: sessionio.SupportFull})
	}
	return append(result,
		sessionio.CapabilityStatus{Capability: sessionio.CapabilityDiscovery, Support: sessionio.SupportPartial, Detail: "all live project buckets and nested transcripts are read; native archives and recovery backups are not imported"},
		sessionio.CapabilityStatus{Capability: sessionio.CapabilityBranches, Support: sessionio.SupportPartial, Detail: "persisted entry parents and reload leaf are available; transient in-memory branch selection and ambiguous opaque parentSession targets are unavailable"},
		sessionio.CapabilityStatus{Capability: sessionio.CapabilityRepository, Support: sessionio.SupportUnavailable, Detail: "OMP session headers expose workspace directories, not authoritative repository identity"},
	)
}

func (adapter *Adapter) locator(relative string) sessionio.SourceLocator {
	return sessionio.SourceLocator{Kind: sessionio.LocatorKindFile, File: &sessionio.FileLocator{Root: adapter.root, Path: relative}}
}

func (adapter *Adapter) discover(ctx context.Context) ([]string, []sessionio.Diagnostic, bool, error) {
	if err := adapter.validateContext(ctx); err != nil {
		return nil, nil, false, err
	}
	root := filepath.Join(adapter.root, "sessions")
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, adapter.readerError("discover", "", adapter.locator("sessions"), err)
	}
	if !info.IsDir() {
		return nil, nil, true, adapter.readerError("discover", "", adapter.locator("sessions"), errors.New("OMP sessions root is not a directory"))
	}
	var paths []string
	var diagnostics []sessionio.Diagnostic
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(adapter.root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.Type()&os.ModeSymlink != 0 {
			diagnostics = append(diagnostics, diagnostic("omp_symlink_skipped", "symlinked source entry is not followed", adapter.locator(relative), nil))
			return nil
		}
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".jsonl") {
			paths = append(paths, relative)
		}
		return nil
	})
	sort.Strings(paths)
	if err != nil {
		return nil, nil, true, adapter.readerError("discover", "", adapter.locator("sessions"), err)
	}
	return paths, diagnostics, true, nil
}

func (adapter *Adapter) Sources(ctx context.Context) (sessionio.Stream[sessionio.Source], error) {
	_, diagnostics, exists, err := adapter.discover(ctx)
	if err != nil {
		return nil, adapter.readerError("sources", "", adapter.locator("sessions"), err)
	}
	status := sessionio.SourceStatusAvailable
	if !exists {
		status = sessionio.SourceStatusMissing
	}
	return values([]sessionio.Source{{ID: adapter.sourceID, Harness: sessionio.HarnessOMP, Kind: sessionio.SourceKindCanonical, Status: status, Locator: adapter.locator("."), Capabilities: capabilities(), Diagnostics: diagnostics}})
}

func (adapter *Adapter) Sessions(ctx context.Context, request sessionio.SessionRequest) (sessionio.Stream[sessionio.SessionRef], error) {
	paths, _, _, err := adapter.discover(ctx)
	if err != nil {
		return nil, err
	}
	if len(request.Sources) != 0 {
		found := false
		for _, id := range request.Sources {
			if id == adapter.sourceID {
				found = true
			}
		}
		if !found {
			return values([]sessionio.SessionRef{})
		}
	}
	refs := make([]sessionio.SessionRef, 0, len(paths))
	for _, relative := range paths {
		stamp, stampErr := adapter.listingStamp(relative)
		if stampErr != nil {
			return nil, adapter.readerError("sessions", "", adapter.locator(relative), stampErr)
		}
		if adapter.cache != nil {
			if ref, found := adapter.cache.Lookup(relative, stamp); found {
				refs = append(refs, ref)
				continue
			}
		}
		state, err := adapter.open(ctx, relative)
		if err != nil {
			return nil, err
		}
		if state == nil {
			continue
		}
		ref := state.session
		if err := state.generation.Close(); err != nil {
			return nil, err
		}
		if adapter.cache != nil && stamp == strings.TrimPrefix(string(ref.DiscoveryRevision), "omp:") {
			adapter.cache.Retain(relative, stamp, ref)
		}
		refs = append(refs, ref)
	}
	resolveRelationships(refs)
	adapter.mu.Lock()
	adapter.refs = refs
	adapter.mu.Unlock()
	return values(refs)
}

// Blob and artifact availability participates in freshness even when JSONL bytes do not change.
func (adapter *Adapter) listingStamp(relative string) (string, error) {
	stamp, err := fileid.Stamp(filepath.Join(adapter.root, filepath.FromSlash(relative)))
	if err != nil {
		return "", err
	}
	for _, root := range append([]string{"blobs"}, artifactDirectories(relative)...) {
		err := filepath.WalkDir(filepath.Join(adapter.root, filepath.FromSlash(root)), func(path string, entry os.DirEntry, walkErr error) error {
			if errors.Is(walkErr, os.ErrNotExist) {
				stamp += "\x00" + root + ":absent"
				return nil
			}
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() && path != filepath.Join(adapter.root, filepath.FromSlash(root)) {
				return filepath.SkipDir
			}
			part, err := fileid.Stamp(path)
			if err != nil {
				return err
			}
			stamp += "\x00" + path + ":" + part
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	return digest([]byte(stamp)), nil
}

type entry struct {
	Type          string          `json:"type"`
	ID            string          `json:"id"`
	ParentID      json.RawMessage `json:"parentId"`
	Timestamp     json.RawMessage `json:"timestamp"`
	Version       *int            `json:"version"`
	Title         string          `json:"title"`
	CWD           string          `json:"cwd"`
	ParentSession string          `json:"parentSession"`
	Message       json.RawMessage `json:"message"`
	Content       json.RawMessage `json:"content"`
	Model         string          `json:"model"`
	ThinkingLevel string          `json:"thinkingLevel"`
	Summary       string          `json:"summary"`
	CustomType    string          `json:"customType"`
}

type recordMeta struct {
	id      sessionio.ObservationID
	locator sessionio.SourceLocator
}

type readState struct {
	adapter          *Adapter
	session          sessionio.SessionRef
	generation       *sourceio.JSONLGeneration
	version          int
	headOrdinal      uint64
	lastOrdinal      uint64
	parents          map[string][]recordMeta
	tools            map[string]*toolIndex
	external         map[string]*externalPayload
	externalOrder    []string
	externalPosition int
	previous         *recordMeta
	branchTarget     sessionio.SessionID
}

func (adapter *Adapter) open(ctx context.Context, relative string) (*readState, error) {
	base := *adapter.locator(relative).File
	stamp, err := adapter.listingStamp(relative)
	if err != nil {
		return nil, adapter.readerError("read", "", adapter.locator(relative), err)
	}
	state := &readState{adapter: adapter, version: 1, parents: map[string][]recordMeta{}, tools: map[string]*toolIndex{}, external: map[string]*externalPayload{}}
	var header *entry
	var slot *entry
	var created, last *time.Time
	var diagnostics []sessionio.Diagnostic
	var title string
	result, err := sourceio.OpenJSONLGeneration(ctx, sourceio.FileSpec{OpenPath: filepath.Join(adapter.root, filepath.FromSlash(relative)), Locator: base}, sourceio.OpenOptions{TailMode: sourceio.TailModeGrowing, SizePolicy: sourceio.RecordSizePolicy{MaxBytes: adapter.maxRecordBytes}, ObserveRecord: func(record sourceio.JSONLRecord) error {
		locator := record.SourceLocator(base)
		value, err := parseEntry(record.Data)
		if err != nil {
			return &locatedError{locator: locator, err: err}
		}
		if header == nil {
			if record.Record == 1 && value.Type == "title" {
				var titleSlot struct {
					V         int     `json:"v"`
					UpdatedAt string  `json:"updatedAt"`
					Pad       *string `json:"pad"`
				}
				if err := json.Unmarshal(record.Data, &titleSlot); err != nil {
					return &locatedError{locator: locator, err: err}
				}
				if titleSlot.V != 1 || titleSlot.Pad == nil {
					return &locatedError{locator: locator, err: errors.New("invalid OMP title slot")}
				}
				_, dates := sourceTimestamp(json.RawMessage(strconv.Quote(titleSlot.UpdatedAt)), false, "title.updatedAt", locator)
				diagnostics = append(diagnostics, dates...)
				slot = &value
				return nil
			}
			if value.Type != "session" || value.ID == "" {
				return &locatedError{locator: locator, err: errors.New("OMP logical first record must be a session header with id")}
			}
			header = &value
			state.headOrdinal = record.Record
			if value.Version != nil {
				state.version = *value.Version
			}
			if state.version < 1 || state.version > 3 {
				return &locatedError{locator: locator, err: fmt.Errorf("unsupported OMP session version %d", state.version)}
			}
			occurrence := sessionio.OccurrenceID(sourceio.DerivedID("occurrence", string(adapter.sourceID), relative))
			state.session = sessionio.SessionRef{ID: sessionio.SessionID(sourceio.DerivedID("session", string(occurrence), value.ID)), NativeID: value.ID, Occurrence: sessionio.SourceOccurrence{ID: occurrence, SourceID: adapter.sourceID, Harness: sessionio.HarnessOMP, Locator: adapter.locator(relative)}, Native: sessionio.NativeSessionMetadata{Identities: []sessionio.NativeIdentity{{Kind: sessionio.NativeIdentityKindSession, Value: value.ID}}}}
			var dates []sessionio.Diagnostic
			created, dates = sourceTimestamp(value.Timestamp, false, "session.timestamp", locator)
			diagnostics = append(diagnostics, dates...)
			title = value.Title
			return nil
		}
		if value.Type == "session" || value.Type == "title" {
			return &locatedError{locator: locator, err: errors.New("unexpected interior OMP header or title slot")}
		}
		if state.version >= 2 && value.ID == "" {
			diagnostics = append(diagnostics, diagnostic("omp_missing_entry_id", "native entry id is missing; topology cannot be resolved", locator, nil))
		}
		id := state.observationID(record)
		if value.ID != "" {
			state.parents[value.ID] = append(state.parents[value.ID], recordMeta{id: id, locator: locator})
		}
		state.lastOrdinal = record.Record
		_, dates := sourceTimestamp(value.Timestamp, false, "entry.timestamp", locator)
		diagnostics = append(diagnostics, dates...)
		if value.Type == "title_change" && slot == nil {
			title = value.Title
		}
		if value.Type == "message" {
			var message nativeMessage
			if err := json.Unmarshal(value.Message, &message); err != nil {
				return &locatedError{locator: locator, err: err}
			}
			if message.Role == "" {
				return &locatedError{locator: locator, err: errors.New("OMP message role is required")}
			}
			timestamp, dates := sourceTimestamp(message.Timestamp, true, "message.timestamp", locator)
			diagnostics = append(diagnostics, dates...)
			if (message.Role == "user" || message.Role == "assistant") && timestamp != nil && (last == nil || timestamp.After(*last)) {
				last = timestamp
			}
			if err := state.indexTools(message); err != nil {
				return &locatedError{locator: locator, err: err}
			}
		}
		if err := state.indexExternal(record.Data, relative); err != nil {
			return &locatedError{locator: locator, err: err}
		}
		return nil
	}})
	if err != nil {
		return nil, adapter.readerError("read", state.session.ID, errorLocator(err, base), err)
	}
	if result.Generation == nil {
		return nil, nil
	}
	state.generation = result.Generation
	if header == nil {
		if err := result.Generation.Close(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if slot != nil {
		title = slot.Title
	}
	state.session.Title, state.session.CreatedAt, state.session.LastMessageAt, state.session.Diagnostics = title, created, last, diagnostics
	state.session.DiscoveryRevision = sessionio.DiscoveryRevision("omp:" + stamp)
	if header.ParentSession != "" {
		state.session.Native.Relationships = append(state.session.Native.Relationships, sessionio.NativeRelationshipHint{Kind: sessionio.NativeRelationshipKindForkParent, TargetNativeID: header.ParentSession})
	}
	if strings.Count(relative, "/") > 2 {
		state.session.Native.Agent = &sessionio.NativeAgentMetadata{Nickname: strings.TrimSuffix(filepath.Base(relative), ".jsonl"), Path: relative}
		parent := strings.TrimSuffix(filepath.ToSlash(filepath.Dir(relative)), ".jsonl") + ".jsonl"
		state.session.Native.Relationships = append(state.session.Native.Relationships, sessionio.NativeRelationshipHint{Kind: sessionio.NativeRelationshipKindControlParent, TargetNativeID: parent})
	}
	return state, nil
}

func (adapter *Adapter) Read(ctx context.Context, session sessionio.SessionRef) (sessionio.Stream[sessionio.ReadItem], error) {
	paths, _, _, err := adapter.discover(ctx)
	if err != nil {
		return nil, err
	}
	locator := session.Occurrence.Locator
	if locator.File == nil || locator.File.Root != adapter.root || session.Occurrence.SourceID != adapter.sourceID || session.Occurrence.Harness != sessionio.HarnessOMP {
		return nil, adapter.readerError("read", session.ID, locator, errors.New("session does not belong to this OMP source"))
	}
	index := sort.SearchStrings(paths, locator.File.Path)
	if index == len(paths) || paths[index] != locator.File.Path {
		return nil, adapter.readerError("read", session.ID, locator, errors.New("session occurrence is not a discovered OMP transcript"))
	}
	state, err := adapter.open(ctx, locator.File.Path)
	if err != nil {
		return nil, err
	}
	if state == nil {
		return nil, adapter.readerError("read", session.ID, locator, errors.New("session header is unavailable"))
	}
	if state.session.ID != session.ID {
		return nil, errors.Join(adapter.readerError("read", session.ID, locator, errors.New("native session identity changed")), state.generation.Close())
	}
	adapter.mu.RLock()
	refs := adapter.refs
	adapter.mu.RUnlock()
	if refs == nil {
		refsStream, err := adapter.Sessions(ctx, sessionio.SessionRequest{})
		if err != nil {
			return nil, errors.Join(err, state.generation.Close())
		}
		if err := refsStream.Close(); err != nil {
			return nil, errors.Join(err, state.generation.Close())
		}
		adapter.mu.RLock()
		refs = adapter.refs
		adapter.mu.RUnlock()
	}
	for hintIndex := range state.session.Native.Relationships {
		hint := &state.session.Native.Relationships[hintIndex]
		for _, ref := range refs {
			file := ref.Occurrence.Locator.File
			target := hint.TargetNativeID
			if !filepath.IsAbs(target) {
				target = filepath.Join(adapter.root, filepath.FromSlash(target))
			}
			if filepath.Clean(target) == filepath.Clean(filepath.Join(file.Root, filepath.FromSlash(file.Path))) {
				hint.TargetNativeID = ref.NativeID
			}
		}
	}
	for _, hint := range state.session.Native.Relationships {
		if hint.Kind != sessionio.NativeRelationshipKindForkParent {
			continue
		}
		var targets []sessionio.SessionID
		for _, ref := range refs {
			if ref.NativeID == hint.TargetNativeID && ref.ID != state.session.ID {
				targets = append(targets, ref.ID)
			}
		}
		if len(targets) == 1 {
			state.branchTarget = targets[0]
		}
	}
	return sessionio.NewStream(state.next, state.generation.Close)
}

func parseEntry(data []byte) (entry, error) {
	var value entry
	if err := json.Unmarshal(data, &value); err != nil {
		return value, err
	}
	if value.Type == "" {
		return value, errors.New("OMP entry type is required")
	}
	return value, nil
}

func sourceTimestamp(raw json.RawMessage, milliseconds bool, field string, locator sessionio.SourceLocator) (*time.Time, []sessionio.Diagnostic) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	timestamp, err := parseTimestamp(raw, milliseconds)
	if err != nil {
		return nil, []sessionio.Diagnostic{{Code: "omp_invalid_timestamp", Severity: sessionio.DiagnosticSeverityWarning, Message: "invalid " + field + ": " + err.Error(), Locator: &locator, Cause: err}}
	}
	return &timestamp, nil
}

func parseTimestamp(raw json.RawMessage, milliseconds bool) (time.Time, error) {
	if milliseconds {
		if raw[0] == '"' {
			return time.Time{}, errors.New("message timestamp must be numeric milliseconds")
		}
		var number json.Number
		if err := json.Unmarshal(raw, &number); err != nil {
			return time.Time{}, err
		}
		value, err := number.Int64()
		if err != nil {
			return time.Time{}, err
		}
		timestamp := time.UnixMilli(value).UTC()
		if timestamp.Year() < 1 || timestamp.Year() > 9999 {
			return time.Time{}, errors.New("timestamp outside representable RFC3339 range")
		}
		return timestamp, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return time.Time{}, err
	}
	timestamp, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return time.Time{}, err
	}
	return timestamp, nil
}

func (state *readState) observationID(record sourceio.JSONLRecord) sessionio.ObservationID {
	return sessionio.ObservationID(sourceio.DerivedID("observation", string(state.session.ID), "jsonl", strconv.FormatUint(record.Record, 10), digest(record.Data, record.Framing)))
}

func (adapter *Adapter) validateContext(ctx context.Context) error {
	if adapter == nil {
		return errors.New("omp: nil adapter")
	}
	if ctx == nil {
		return errors.New("omp: context must not be nil")
	}
	return ctx.Err()
}
func (adapter *Adapter) readerError(operation string, id sessionio.SessionID, locator sessionio.SourceLocator, err error) error {
	return &sessionio.ReaderError{Operation: operation, Harness: sessionio.HarnessOMP, AdapterVersion: adapterVersion, SessionID: id, Locator: &locator, Err: err}
}

type locatedError struct {
	locator sessionio.SourceLocator
	err     error
}

func (err *locatedError) Error() string { return err.err.Error() }
func (err *locatedError) Unwrap() error { return err.err }
func errorLocator(err error, base sessionio.FileLocator) sessionio.SourceLocator {
	var located *locatedError
	var malformed *sourceio.MalformedJSONLError
	var oversized *sourceio.RecordTooLargeError
	var changed *sourceio.ChangedSourceError
	switch {
	case errors.As(err, &located):
		return located.locator
	case errors.As(err, &malformed):
		return malformed.Locator
	case errors.As(err, &oversized):
		return oversized.Locator
	case errors.As(err, &changed):
		return changed.Locator
	default:
		return sessionio.SourceLocator{Kind: sessionio.LocatorKindFile, File: &base}
	}
}
func diagnostic(code, message string, locator sessionio.SourceLocator, cause error) sessionio.Diagnostic {
	return sessionio.Diagnostic{Code: code, Severity: sessionio.DiagnosticSeverityWarning, Message: message, Locator: &locator, Cause: cause}
}
func values[T any](items []T) (sessionio.Stream[T], error) {
	index := 0
	return sessionio.NewStream(func(ctx context.Context) (T, error) {
		var zero T
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		if index == len(items) {
			return zero, io.EOF
		}
		value := items[index]
		index++
		return value, nil
	}, func() error { return nil })
}
func digest(parts ...[]byte) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = hash.Write(part)
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

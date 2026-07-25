// Package claude reads Claude Code transcript JSONL files.
package claude

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	sessionio "github.com/nikitatsym/agent-session-io"
	"github.com/nikitatsym/agent-session-io/internal/sourceio"
)

const (
	DefaultMaxRecordBytes int64 = 64 << 20
	adapterVersion              = "1"
)

// Config configures a Claude Code adapter.
type Config struct {
	ConfigDir      string
	MaxRecordBytes int64
}

// DefaultConfig returns deterministic configuration without filesystem access.
func DefaultConfig() Config { return Config{MaxRecordBytes: DefaultMaxRecordBytes} }

// Adapter reads one configured Claude Code directory.
type Adapter struct {
	configDir      string
	maxRecordBytes int64
	sourceID       sessionio.SourceID
}

// New constructs an adapter and resolves an empty config directory once.
func New(config Config) (*Adapter, error) {
	if config.MaxRecordBytes != sourceio.UnlimitedRecordBytes && config.MaxRecordBytes <= 0 {
		return nil, fmt.Errorf("claude: max record bytes must be positive or %d", sourceio.UnlimitedRecordBytes)
	}
	dir := config.ConfigDir
	if dir == "" {
		dir = os.Getenv("CLAUDE_CONFIG_DIR")
		if dir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("claude: resolve user home: %w", err)
			}
			dir = filepath.Join(home, ".claude")
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("claude: resolve config directory %q: %w", dir, err)
	}
	return &Adapter{configDir: abs, maxRecordBytes: config.MaxRecordBytes, sourceID: sessionio.SourceID(derivedID("source", string(sessionio.HarnessClaude), abs))}, nil
}

// Descriptor declares the pinned Claude Code transcript coverage.
func (adapter *Adapter) Descriptor() sessionio.AdapterDescriptor {
	return sessionio.AdapterDescriptor{Harness: sessionio.HarnessClaude, Version: adapterVersion, Capabilities: capabilities()}
}

func capabilities() []sessionio.CapabilityStatus {
	return []sessionio.CapabilityStatus{
		{Capability: sessionio.CapabilityDiscovery, Support: sessionio.SupportFull},
		{Capability: sessionio.CapabilityMessages, Support: sessionio.SupportFull},
		{Capability: sessionio.CapabilityRichContent, Support: sessionio.SupportFull},
		{Capability: sessionio.CapabilityTools, Support: sessionio.SupportPartial, Detail: "external persisted payloads are not imported and transcript payloads may be truncated"},
		{Capability: sessionio.CapabilityReasoning, Support: sessionio.SupportFull},
		{Capability: sessionio.CapabilityBranches, Support: sessionio.SupportFull},
		{Capability: sessionio.CapabilityUsage, Support: sessionio.SupportFull},
		{Capability: sessionio.CapabilityEnvironment, Support: sessionio.SupportFull},
		{Capability: sessionio.CapabilityRepository, Support: sessionio.SupportPartial, Detail: "Claude persists working directory and branch but not complete repository identity"},
		{Capability: sessionio.CapabilityIncrementalReading, Support: sessionio.SupportFull},
	}
}

type occurrence struct {
	relative    string
	primaryID   string
	agentID     string
	sidecarPath string
	workflow    bool
}

type discovery struct {
	occurrences []occurrence
	journals    []string
	diagnostics []sessionio.Diagnostic
	projects    bool
}

func (adapter *Adapter) sourceLocator(path string) sessionio.SourceLocator {
	return sessionio.SourceLocator{Kind: sessionio.LocatorKindFile, File: &sessionio.FileLocator{Root: adapter.configDir, Path: filepath.ToSlash(path)}}
}

func (adapter *Adapter) discover() (discovery, error) {
	result := discovery{}
	projectsPath := filepath.Join(adapter.configDir, "projects")
	info, err := os.Lstat(projectsPath)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return discovery{}, fmt.Errorf("stat Claude projects root: %w", err)
	}
	result.projects = true
	if info.Mode()&os.ModeSymlink != 0 {
		result.diagnostics = append(result.diagnostics, adapter.symlinkDiagnostic("projects"))
		return result, nil
	}
	if !info.IsDir() {
		return discovery{}, errors.New("Claude projects root is not a directory")
	}
	projects, err := os.ReadDir(projectsPath)
	if err != nil {
		return discovery{}, fmt.Errorf("read Claude projects root: %w", err)
	}
	for _, project := range projects {
		projectRel := filepath.ToSlash(filepath.Join("projects", project.Name()))
		if project.Type()&os.ModeSymlink != 0 {
			result.diagnostics = append(result.diagnostics, adapter.symlinkDiagnostic(projectRel))
			continue
		}
		if !project.IsDir() {
			continue
		}
		if err := adapter.scanProject(&result, filepath.Join(projectsPath, project.Name()), projectRel); err != nil {
			return discovery{}, err
		}
	}
	sort.Slice(result.occurrences, func(i, j int) bool { return result.occurrences[i].relative < result.occurrences[j].relative })
	sort.Strings(result.journals)
	return result, nil
}

func (adapter *Adapter) scanProject(result *discovery, projectPath, projectRel string) error {
	entries, err := os.ReadDir(projectPath)
	if err != nil {
		return fmt.Errorf("read Claude project %q: %w", projectPath, err)
	}
	for _, entry := range entries {
		relative := filepath.ToSlash(filepath.Join(projectRel, entry.Name()))
		if entry.Type()&os.ModeSymlink != 0 {
			if strings.HasSuffix(entry.Name(), ".jsonl") || validUUID(entry.Name()) {
				result.diagnostics = append(result.diagnostics, adapter.symlinkDiagnostic(relative))
			}
			continue
		}
		if entry.Type().IsRegular() {
			id, ok := primaryFilenameID(entry.Name())
			if !ok {
				continue
			}
			result.occurrences = append(result.occurrences, occurrence{relative: relative, primaryID: id})
			continue
		}
		if !entry.IsDir() || !validUUID(entry.Name()) {
			continue
		}
		if err := adapter.scanSubagents(result, filepath.Join(projectPath, entry.Name(), "subagents"), filepath.ToSlash(filepath.Join(relative, "subagents")), entry.Name(), false); err != nil {
			return err
		}
	}
	return nil
}

func (adapter *Adapter) scanSubagents(result *discovery, dir, relative, primaryID string, workflow bool) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat Claude subagents directory %q: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		result.diagnostics = append(result.diagnostics, adapter.symlinkDiagnostic(relative))
		return nil
	}
	if !info.IsDir() {
		return fmt.Errorf("Claude subagents path %q is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read Claude subagents directory %q: %w", dir, err)
	}
	for _, entry := range entries {
		rel := filepath.ToSlash(filepath.Join(relative, entry.Name()))
		if entry.Type()&os.ModeSymlink != 0 {
			result.diagnostics = append(result.diagnostics, adapter.symlinkDiagnostic(rel))
			continue
		}
		if !workflow && entry.IsDir() && entry.Name() == "workflows" {
			workflows, err := os.ReadDir(filepath.Join(dir, entry.Name()))
			if err != nil {
				return fmt.Errorf("read Claude workflows directory: %w", err)
			}
			for _, item := range workflows {
				itemRel := filepath.ToSlash(filepath.Join(rel, item.Name()))
				if item.Type()&os.ModeSymlink != 0 {
					result.diagnostics = append(result.diagnostics, adapter.symlinkDiagnostic(itemRel))
					continue
				}
				if !item.IsDir() || item.Name() == "" {
					continue
				}
				if err := adapter.scanSubagents(result, filepath.Join(dir, entry.Name(), item.Name()), itemRel, primaryID, true); err != nil {
					return err
				}
			}
			continue
		}
		if entry.Name() == "journal.jsonl" && workflow && entry.Type().IsRegular() {
			result.journals = append(result.journals, rel)
			continue
		}
		if !entry.Type().IsRegular() {
			continue
		}
		agentID, ok := subagentFilenameID(entry.Name())
		if !ok {
			continue
		}
		metaRelative := strings.TrimSuffix(rel, ".jsonl") + ".meta.json"
		result.occurrences = append(result.occurrences, occurrence{relative: rel, primaryID: primaryID, agentID: agentID, sidecarPath: metaRelative, workflow: workflow})
	}
	return nil
}

func primaryFilenameID(name string) (string, bool) {
	value, ok := strings.CutSuffix(name, ".jsonl")
	return value, ok && validUUID(value)
}

func subagentFilenameID(name string) (string, bool) {
	value, ok := strings.CutPrefix(name, "agent-")
	if !ok {
		return "", false
	}
	value, ok = strings.CutSuffix(value, ".jsonl")
	return value, ok && value != ""
}

func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}

func (adapter *Adapter) symlinkDiagnostic(relative string) sessionio.Diagnostic {
	locator := adapter.sourceLocator(relative)
	return sessionio.Diagnostic{Code: "claude_symlink_skipped", Severity: sessionio.DiagnosticSeverityWarning, Message: "symlinked Claude entry skipped: " + relative, Locator: &locator}
}

func (adapter *Adapter) Sources(ctx context.Context) (sessionio.Stream[sessionio.Source], error) {
	if err := adapter.validateContext(ctx); err != nil {
		return nil, err
	}
	discovery, err := adapter.discover()
	if err != nil {
		return nil, adapter.error("sources", "", nil, err)
	}
	sources, err := adapter.sources(discovery)
	if err != nil {
		return nil, adapter.error("sources", "", nil, err)
	}
	return streamValues(sources)
}

func (adapter *Adapter) sources(discovery discovery) ([]sessionio.Source, error) {
	status := sessionio.SourceStatusAvailable
	if !discovery.projects {
		status = sessionio.SourceStatusMissing
	}
	diagnostics := append([]sessionio.Diagnostic(nil), discovery.diagnostics...)
	if !discovery.projects {
		diagnostics = append(diagnostics, sessionio.Diagnostic{Code: "claude_projects_missing", Severity: sessionio.DiagnosticSeverityWarning, Message: "Claude projects root is absent: projects"})
	}
	result := []sessionio.Source{{ID: adapter.sourceID, Harness: sessionio.HarnessClaude, Kind: sessionio.SourceKindCanonical, Status: status, Locator: adapter.sourceLocator("."), Capabilities: capabilities(), Diagnostics: diagnostics}}
	for _, relative := range []string{"history.jsonl", "file-history", "sessions", "shell-snapshots", "session-env", "tasks"} {
		source, err := adapter.auxiliarySource(relative)
		if err != nil {
			return nil, err
		}
		result = append(result, source)
	}
	for _, relative := range discovery.journals {
		source, err := adapter.auxiliarySource(relative)
		if err != nil {
			return nil, err
		}
		result = append(result, source)
	}
	return result, nil
}

func (adapter *Adapter) auxiliarySource(relative string) (sessionio.Source, error) {
	status := sessionio.SourceStatusDisabled
	if _, err := os.Lstat(filepath.Join(adapter.configDir, filepath.FromSlash(relative))); errors.Is(err, os.ErrNotExist) {
		status = sessionio.SourceStatusMissing
	} else if err != nil {
		return sessionio.Source{}, fmt.Errorf("stat Claude auxiliary source %q: %w", relative, err)
	}
	locator := adapter.sourceLocator(relative)
	return sessionio.Source{ID: sessionio.SourceID(derivedID("source", string(sessionio.HarnessClaude), adapter.configDir, relative)), Harness: sessionio.HarnessClaude, Kind: sessionio.SourceKindAuxiliary, Status: status, Locator: locator, Capabilities: []sessionio.CapabilityStatus{{Capability: sessionio.CapabilityDiscovery, Support: sessionio.SupportFull}}, Diagnostics: []sessionio.Diagnostic{{Code: "claude_auxiliary_excluded", Severity: sessionio.DiagnosticSeverityInfo, Message: "Claude auxiliary source is intentionally outside canonical transcript import", Locator: &locator}}}, nil
}

func (adapter *Adapter) Sessions(ctx context.Context, request sessionio.SessionRequest) (sessionio.Stream[sessionio.SessionRef], error) {
	if err := adapter.validateContext(ctx); err != nil {
		return nil, err
	}
	if !sourceSelected(request.Sources, adapter.sourceID) {
		return streamValues([]sessionio.SessionRef{})
	}
	discovery, err := adapter.discover()
	if err != nil {
		return nil, adapter.error("sessions", "", nil, err)
	}
	refs := make([]sessionio.SessionRef, 0, len(discovery.occurrences))
	for _, occurrence := range discovery.occurrences {
		snapshot, err := adapter.readSessionRef(ctx, occurrence)
		if err != nil {
			return nil, err
		}
		if snapshot != nil {
			refs = append(refs, snapshot.ref)
		}
	}
	return streamValues(refs)
}

type recordHeader struct {
	Type        string `json:"type"`
	UUID        string `json:"uuid"`
	ParentUUID  string `json:"parentUuid"`
	SessionID   string `json:"sessionId"`
	AgentID     string `json:"agentId"`
	Timestamp   string `json:"timestamp"`
	CWD         string `json:"cwd"`
	GitBranch   string `json:"gitBranch"`
	IsSidechain *bool  `json:"isSidechain"`
	ForkedFrom  struct {
		SessionID   string `json:"sessionId"`
		MessageUUID string `json:"messageUuid"`
	} `json:"forkedFrom"`
}

type sidecar struct {
	AgentType     string          `json:"agentType"`
	Model         string          `json:"model"`
	ParentAgentID string          `json:"parentAgentId"`
	ToolUseID     string          `json:"toolUseId"`
	Workflow      json.RawMessage `json:"workflow"`
	Worktree      json.RawMessage `json:"worktree"`
}

type sessionSnapshot struct {
	ref          sessionio.SessionRef
	sidecar      sidecar
	sidecarBytes []byte
	sidecarInfo  os.FileInfo
}

func (adapter *Adapter) readSessionRef(ctx context.Context, occurrence occurrence) (*sessionSnapshot, error) {
	path := filepath.Join(adapter.configDir, filepath.FromSlash(occurrence.relative))
	base := adapter.baseLocator(occurrence)
	result, err := sourceio.OpenJSONLGeneration(ctx, sourceio.FileSpec{OpenPath: path, Locator: base}, sourceio.OpenOptions{TailMode: sourceio.TailModeGrowing, SizePolicy: sourceio.RecordSizePolicy{MaxBytes: adapter.maxRecordBytes}})
	if err != nil {
		return nil, adapter.error("sessions", "", sourceErrorLocator(err, base), err)
	}
	if result.Generation == nil {
		return nil, nil
	}
	defer result.Generation.Close()
	var first sourceio.JSONLRecord
	first, err = result.Generation.Next(ctx)
	if errors.Is(err, io.EOF) {
		return nil, nil
	}
	if err != nil {
		return nil, adapter.error("sessions", "", sourceErrorLocator(err, base), err)
	}
	header, timestamp, diagnostic, err := parseHeader(first.Data, adapter.recordLocator(occurrence, first))
	if err != nil {
		locator := adapter.recordLocator(occurrence, first)
		return nil, adapter.error("sessions", "", &locator, err)
	}
	if !occurrence.matchesIdentity(header) {
		locator := adapter.recordLocator(occurrence, first)
		return nil, adapter.error("sessions", "", &locator, errors.New("first transcript record identity does not match filename"))
	}
	meta, sidecarBytes, sidecarInfo, err := adapter.readSidecar(occurrence)
	if err != nil {
		locator := adapter.sourceLocator(occurrence.sidecarPath)
		return nil, adapter.error("sessions", "", &locator, err)
	}
	title, titleEvidence, updated, relationships, err := adapter.indexTranscript(ctx, occurrence, meta)
	if err != nil {
		return nil, err
	}
	nativeID := occurrence.nativeID()
	native := sessionio.NativeSessionMetadata{Identities: []sessionio.NativeIdentity{{Kind: sessionio.NativeIdentityKindSession, Value: nativeID}}, Relationships: relationships}
	if occurrence.agentID != "" {
		native.Identities[0].Value = occurrence.primaryID
		native.Agent = &sessionio.NativeAgentMetadata{Role: meta.AgentType, Path: occurrence.relative}
	}
	occurrenceID := adapter.occurrenceID(occurrence)
	info, err := os.Stat(path)
	if err != nil {
		locator := adapter.sourceLocator(occurrence.relative)
		return nil, adapter.error("sessions", "", &locator, err)
	}
	discoveryRevision := adapter.discoveryRevision(occurrence, info, append(first.Data, first.Framing...), sidecarInfo, sidecarBytes, titleEvidence)
	ref := sessionio.SessionRef{ID: sessionio.SessionID(derivedID("session", string(occurrenceID), nativeID)), NativeID: nativeID, Title: title, DiscoveryRevision: discoveryRevision, Native: native, Occurrence: sessionio.SourceOccurrence{ID: occurrenceID, SourceID: adapter.sourceID, Harness: sessionio.HarnessClaude, Locator: adapter.sourceLocator(occurrence.relative)}, StartedAt: timestamp, UpdatedAt: updated}
	if diagnostic != nil {
		ref.Diagnostics = append(ref.Diagnostics, *diagnostic)
	}
	return &sessionSnapshot{ref: ref, sidecar: meta, sidecarBytes: sidecarBytes, sidecarInfo: sidecarInfo}, nil
}

func (occurrence occurrence) nativeID() string {
	if occurrence.agentID != "" {
		return occurrence.agentID
	}
	return occurrence.primaryID
}
func (occurrence occurrence) matchesIdentity(header recordHeader) bool {
	if occurrence.agentID == "" {
		return header.SessionID == "" || header.SessionID == occurrence.primaryID
	}
	return (header.AgentID == "" || header.AgentID == occurrence.agentID) && (header.SessionID == "" || header.SessionID == occurrence.primaryID)
}

func (adapter *Adapter) indexTranscript(ctx context.Context, occurrence occurrence, meta sidecar) (string, []byte, *time.Time, []sessionio.NativeRelationshipHint, error) {
	path := filepath.Join(adapter.configDir, filepath.FromSlash(occurrence.relative))
	base := adapter.baseLocator(occurrence)
	var customTitle string
	var customTitleEvidence []byte
	var aiTitle string
	var aiTitleEvidence []byte
	var updated *time.Time
	forkParents := []string{}
	seenForkParents := map[string]struct{}{}
	relationships := []sessionio.NativeRelationshipHint{}
	result, err := sourceio.OpenJSONLGeneration(ctx, sourceio.FileSpec{OpenPath: path, Locator: base}, sourceio.OpenOptions{TailMode: sourceio.TailModeGrowing, SizePolicy: sourceio.RecordSizePolicy{MaxBytes: adapter.maxRecordBytes}, ObserveRecord: func(record sourceio.JSONLRecord) error {
		header, timestamp, _, err := parseHeader(record.Data, adapter.recordLocator(occurrence, record))
		if err != nil {
			return &locatedError{locator: adapter.recordLocator(occurrence, record), err: err}
		}
		if !occurrence.matchesIdentity(header) {
			return &locatedError{locator: adapter.recordLocator(occurrence, record), err: errors.New("transcript record identity does not match filename")}
		}
		if parent := header.ForkedFrom.SessionID; parent != "" {
			if _, found := seenForkParents[parent]; !found {
				seenForkParents[parent] = struct{}{}
				forkParents = append(forkParents, parent)
			}
		}
		if timestamp != nil {
			updated = timestamp
		}
		var value struct {
			Type        string          `json:"type"`
			AITitle     string          `json:"aiTitle"`
			CustomTitle string          `json:"customTitle"`
			Message     json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(record.Data, &value); err != nil {
			return err
		}
		if value.Type == "custom-title" && value.CustomTitle != "" {
			customTitle = value.CustomTitle
			customTitleEvidence = append(append(customTitleEvidence[:0], record.Data...), record.Framing...)
		}
		if value.Type == "ai-title" && value.AITitle != "" {
			aiTitle = value.AITitle
			aiTitleEvidence = append(append(aiTitleEvidence[:0], record.Data...), record.Framing...)
		}
		return nil
	}})
	if err != nil {
		return "", nil, nil, nil, adapter.error("sessions", "", sourceErrorLocator(err, base), err)
	}
	if result.Generation != nil {
		_ = result.Generation.Close()
	}
	if occurrence.agentID != "" {
		target := meta.ParentAgentID
		if target == "" {
			target = occurrence.primaryID
		}
		relationships = append(relationships, sessionio.NativeRelationshipHint{Kind: sessionio.NativeRelationshipKindControlParent, TargetNativeID: target})
	}
	for _, parent := range forkParents {
		relationships = append(relationships, sessionio.NativeRelationshipHint{Kind: sessionio.NativeRelationshipKindForkParent, TargetNativeID: parent})
	}
	title := aiTitle
	titleEvidence := aiTitleEvidence
	if customTitle != "" {
		title = customTitle
		titleEvidence = customTitleEvidence
	}
	return title, titleEvidence, updated, relationships, nil
}

func parseHeader(data []byte, locator sessionio.SourceLocator) (recordHeader, *time.Time, *sessionio.Diagnostic, error) {
	var header recordHeader
	if err := json.Unmarshal(data, &header); err != nil {
		return header, nil, nil, err
	}
	if header.Type == "" {
		return header, nil, nil, errors.New("Claude record type is required")
	}
	if header.Timestamp == "" {
		return header, nil, nil, nil
	}
	// parse-skip: invalid source timestamp is intentionally nonfatal
	timestamp, err := time.Parse(time.RFC3339Nano, header.Timestamp)
	// no-report: parse failure is retained in the emitted diagnostic
	if err != nil {
		return header, nil, invalidTimestampDiagnostic(locator, err), nil
	}
	return header, &timestamp, nil, nil
}

func invalidTimestampDiagnostic(locator sessionio.SourceLocator, cause error) *sessionio.Diagnostic {
	return &sessionio.Diagnostic{Code: "claude_invalid_timestamp", Severity: sessionio.DiagnosticSeverityWarning, Message: "invalid Claude timestamp: " + cause.Error(), Locator: &locator, Cause: cause}
}

func (adapter *Adapter) readSidecar(occurrence occurrence) (sidecar, []byte, os.FileInfo, error) {
	if occurrence.sidecarPath == "" {
		return sidecar{}, nil, nil, nil
	}
	path := filepath.Join(adapter.configDir, filepath.FromSlash(occurrence.sidecarPath))
	linkInfo, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return sidecar{}, nil, nil, nil
	}
	if err != nil {
		return sidecar{}, nil, nil, fmt.Errorf("stat sidecar %q: %w", occurrence.sidecarPath, err)
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 {
		return sidecar{}, nil, nil, errors.New("agent metadata sidecar is a symlink")
	}
	file, err := os.Open(path)
	if err != nil {
		return sidecar{}, nil, nil, fmt.Errorf("open sidecar %q: %w", occurrence.sidecarPath, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return sidecar{}, nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return sidecar{}, nil, nil, errors.New("agent metadata sidecar is not a regular file")
	}
	if adapter.maxRecordBytes != sourceio.UnlimitedRecordBytes && info.Size() > adapter.maxRecordBytes {
		return sidecar{}, nil, nil, fmt.Errorf("sidecar exceeds configured limit %d", adapter.maxRecordBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, info.Size()))
	if err != nil {
		return sidecar{}, nil, nil, err
	}
	if int64(len(data)) != info.Size() {
		return sidecar{}, nil, nil, errors.New("agent metadata sidecar changed while reading")
	}
	var value sidecar
	if err := json.Unmarshal(data, &value); err != nil {
		return sidecar{}, nil, nil, fmt.Errorf("parse agent metadata sidecar: %w", err)
	}
	after, err := file.Stat()
	if err != nil {
		return sidecar{}, nil, nil, err
	}
	current, err := os.Stat(path)
	if err != nil {
		return sidecar{}, nil, nil, err
	}
	if !os.SameFile(info, current) || after.Size() != info.Size() || after.ModTime() != info.ModTime() || current.Size() != info.Size() || current.ModTime() != info.ModTime() {
		return sidecar{}, nil, nil, errors.New("agent metadata sidecar changed while reading")
	}
	return value, data, info, nil
}

func (adapter *Adapter) occurrenceID(occurrence occurrence) sessionio.OccurrenceID {
	return sessionio.OccurrenceID(derivedID("occurrence", string(adapter.sourceID), adapter.configDir, occurrence.relative))
}
func (adapter *Adapter) baseLocator(occurrence occurrence) sessionio.FileLocator {
	return sessionio.FileLocator{Root: adapter.configDir, Path: occurrence.relative}
}
func (adapter *Adapter) recordLocator(occurrence occurrence, record sourceio.JSONLRecord) sessionio.SourceLocator {
	return record.SourceLocator(adapter.baseLocator(occurrence))
}

func (adapter *Adapter) discoveryRevision(occurrence occurrence, transcript os.FileInfo, header []byte, meta os.FileInfo, metaBytes []byte, titleEvidence []byte) sessionio.DiscoveryRevision {
	parts := []string{string(adapter.occurrenceID(occurrence)), fmt.Sprintf("%d", transcript.Size()), fmt.Sprintf("%d", transcript.ModTime().UnixNano()), string(header), string(titleEvidence)}
	if meta != nil {
		parts = append(parts, fmt.Sprintf("%d", meta.Size()), fmt.Sprintf("%d", meta.ModTime().UnixNano()), string(metaBytes))
	}
	return sessionio.DiscoveryRevision(derivedID("discovery", parts...))
}

func (adapter *Adapter) Read(ctx context.Context, session sessionio.SessionRef) (sessionio.Stream[sessionio.ReadItem], error) {
	if err := adapter.validateContext(ctx); err != nil {
		return nil, err
	}
	occurrence, err := adapter.occurrenceFromSession(session)
	if err != nil {
		return nil, err
	}
	snapshot, err := adapter.readSessionRef(ctx, occurrence)
	if err != nil {
		return nil, err
	}
	if snapshot == nil {
		locator := session.Occurrence.Locator
		return nil, adapter.error("read", string(session.ID), &locator, errors.New("canonical transcript identity is unavailable"))
	}
	session = snapshot.ref
	path := filepath.Join(adapter.configDir, filepath.FromSlash(occurrence.relative))
	base := adapter.baseLocator(occurrence)
	correlations := make(map[string]*toolCardinality)
	observations := make(map[string][]sessionio.ObservationID)
	result, err := sourceio.OpenJSONLGeneration(ctx, sourceio.FileSpec{OpenPath: path, Locator: base}, sourceio.OpenOptions{TailMode: sourceio.TailModeGrowing, SizePolicy: sourceio.RecordSizePolicy{MaxBytes: adapter.maxRecordBytes}, ObserveRecord: func(record sourceio.JSONLRecord) error {
		locator := record.SourceLocator(base)
		header, _, _, err := parseHeader(record.Data, locator)
		if err != nil {
			return &locatedError{locator: locator, err: err}
		}
		if !occurrence.matchesIdentity(header) {
			return &locatedError{locator: locator, err: errors.New("transcript record identity does not match filename")}
		}
		if header.UUID != "" {
			id := sessionio.ObservationID(derivedID("observation", string(session.ID), "jsonl", fmt.Sprintf("%d", record.Record), digest(record.Data, record.Framing)))
			observations[header.UUID] = append(observations[header.UUID], id)
		}
		if err := classifyTools(record.Data, correlations); err != nil {
			return &locatedError{locator: locator, err: err}
		}
		return nil
	}})
	if err != nil {
		return nil, adapter.error("read", string(session.ID), sourceErrorLocator(err, base), err)
	}
	if result.Generation == nil {
		locator := adapter.sourceLocator(occurrence.relative)
		return nil, adapter.error("read", string(session.ID), &locator, errors.New("transcript disappeared"))
	}
	state := &readState{adapter: adapter, session: session, occurrence: occurrence, generation: result.Generation, sidecar: snapshot.sidecar, sidecarBytes: snapshot.sidecarBytes, sidecarInfo: snapshot.sidecarInfo, observations: observations, calls: map[string][]toolEvidence{}, results: map[string][]toolEvidence{}, correlations: correlations}
	return sessionio.NewStream(state.next, result.Generation.Close)
}

func (adapter *Adapter) occurrenceFromSession(session sessionio.SessionRef) (occurrence, error) {
	locator := session.Occurrence.Locator
	if session.Occurrence.SourceID != adapter.sourceID || session.Occurrence.Harness != sessionio.HarnessClaude || locator.File == nil || locator.File.Root != adapter.configDir {
		return occurrence{}, adapter.error("read", string(session.ID), &locator, errors.New("session does not belong to this Claude config directory"))
	}
	discovery, err := adapter.discover()
	if err != nil {
		return occurrence{}, adapter.error("read", string(session.ID), &locator, err)
	}
	target := filepath.ToSlash(locator.File.Path)
	index := sort.Search(len(discovery.occurrences), func(index int) bool {
		return discovery.occurrences[index].relative >= target
	})
	if index < len(discovery.occurrences) && discovery.occurrences[index].relative == target {
		return discovery.occurrences[index], nil
	}
	return occurrence{}, adapter.error("read", string(session.ID), &locator, errors.New("session occurrence is not a readable Claude transcript"))
}

func (adapter *Adapter) forkTargets(ctx context.Context, source occurrence, messageUUID string) ([]sessionio.ObservationID, error) {
	discovery, err := adapter.discover()
	if err != nil {
		return nil, err
	}
	project := projectKey(source.relative)
	var targets []sessionio.ObservationID
	for _, candidate := range discovery.occurrences {
		if projectKey(candidate.relative) != project {
			continue
		}
		path := filepath.Join(adapter.configDir, filepath.FromSlash(candidate.relative))
		base := adapter.baseLocator(candidate)
		result, err := sourceio.OpenJSONLGeneration(ctx, sourceio.FileSpec{OpenPath: path, Locator: base}, sourceio.OpenOptions{TailMode: sourceio.TailModeGrowing, SizePolicy: sourceio.RecordSizePolicy{MaxBytes: adapter.maxRecordBytes}})
		if err != nil {
			return nil, locatedSourceError(err, base)
		}
		if result.Generation == nil {
			return nil, &locatedError{locator: sessionio.SourceLocator{Kind: sessionio.LocatorKindFile, File: &base}, err: errors.New("fork target candidate disappeared")}
		}
		candidateSessionID := sessionio.SessionID(derivedID("session", string(adapter.occurrenceID(candidate)), candidate.nativeID()))
		for {
			record, err := result.Generation.Next(ctx)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				_ = result.Generation.Close()
				return nil, locatedSourceError(err, base)
			}
			locator := record.SourceLocator(base)
			header, _, _, err := parseHeader(record.Data, locator)
			if err != nil {
				_ = result.Generation.Close()
				return nil, &locatedError{locator: locator, err: err}
			}
			if !candidate.matchesIdentity(header) {
				_ = result.Generation.Close()
				return nil, &locatedError{locator: locator, err: errors.New("fork target transcript identity does not match filename")}
			}
			if header.UUID == messageUUID {
				targets = append(targets, sessionio.ObservationID(derivedID("observation", string(candidateSessionID), "jsonl", fmt.Sprintf("%d", record.Record), digest(record.Data, record.Framing))))
			}
		}
		if err := result.Generation.Close(); err != nil {
			return nil, &locatedError{locator: sessionio.SourceLocator{Kind: sessionio.LocatorKindFile, File: &base}, err: err}
		}
	}
	return targets, nil
}

func projectKey(relative string) string {
	parts := strings.Split(relative, "/")
	if len(parts) >= 2 && parts[0] == "projects" {
		return parts[1]
	}
	return ""
}

type toolEvidence struct {
	event       sessionio.EventID
	observation sessionio.ObservationID
	locator     sessionio.SourceLocator
}

type toolCardinality struct {
	calls   int
	results int
}

func classifyTools(data []byte, values map[string]*toolCardinality) error {
	var record struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return err
	}
	if record.Type != "user" && record.Type != "assistant" {
		return nil
	}
	var message struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(record.Message, &message); err != nil {
		return err
	}
	var blocks []json.RawMessage
	if len(message.Content) == 0 || json.Unmarshal(message.Content, &blocks) != nil {
		return nil
	}
	for _, raw := range blocks {
		var block struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			ToolUseID string `json:"tool_use_id"`
		}
		if err := json.Unmarshal(raw, &block); err != nil {
			return err
		}
		id := ""
		isCall := false
		switch block.Type {
		case "tool_use":
			id, isCall = block.ID, true
		case "tool_result":
			id = block.ToolUseID
		default:
			continue
		}
		if id == "" {
			continue
		}
		value := values[id]
		if value == nil {
			value = &toolCardinality{}
			values[id] = value
		}
		if isCall {
			value.calls++
		} else {
			value.results++
		}
	}
	return nil
}

type readState struct {
	adapter        *Adapter
	session        sessionio.SessionRef
	occurrence     occurrence
	generation     *sourceio.JSONLGeneration
	sidecar        sidecar
	sidecarBytes   []byte
	sidecarInfo    os.FileInfo
	emittedSidecar bool
	observations   map[string][]sessionio.ObservationID
	calls          map[string][]toolEvidence
	results        map[string][]toolEvidence
	correlations   map[string]*toolCardinality
}

func (state *readState) next(ctx context.Context) (sessionio.ReadItem, error) {
	if !state.emittedSidecar && state.sidecarBytes != nil {
		path := filepath.Join(state.adapter.configDir, filepath.FromSlash(state.occurrence.sidecarPath))
		linkInfo, err := os.Lstat(path)
		if err != nil {
			return sessionio.ReadItem{}, state.sidecarError(err)
		}
		if linkInfo.Mode()&os.ModeSymlink != 0 || !linkInfo.Mode().IsRegular() {
			return sessionio.ReadItem{}, state.sidecarChangedError()
		}
		currentInfo, err := os.Stat(path)
		if err != nil {
			return sessionio.ReadItem{}, state.sidecarError(err)
		}
		if !os.SameFile(state.sidecarInfo, currentInfo) || currentInfo.Size() != state.sidecarInfo.Size() || currentInfo.ModTime() != state.sidecarInfo.ModTime() {
			return sessionio.ReadItem{}, state.sidecarChangedError()
		}
		state.emittedSidecar = true
		return state.sidecarItem(), nil
	}
	record, err := state.generation.Next(ctx)
	if errors.Is(err, io.EOF) {
		return sessionio.ReadItem{}, io.EOF
	}
	if err != nil {
		return sessionio.ReadItem{}, state.adapter.error("read", string(state.session.ID), sourceErrorLocator(err, state.adapter.baseLocator(state.occurrence)), err)
	}
	locator := state.adapter.recordLocator(state.occurrence, record)
	header, timestamp, timestampDiagnostic, err := parseHeader(record.Data, locator)
	if err != nil {
		return sessionio.ReadItem{}, state.adapter.error("read", string(state.session.ID), &locator, err)
	}
	if !state.occurrence.matchesIdentity(header) {
		return sessionio.ReadItem{}, state.adapter.error("read", string(state.session.ID), &locator, errors.New("transcript record identity does not match filename"))
	}
	observationID := sessionio.ObservationID(derivedID("observation", string(state.session.ID), "jsonl", fmt.Sprintf("%d", record.Record), digest(record.Data, record.Framing)))
	item := sessionio.ReadItem{Session: state.session, Observation: sessionio.NativeObservation{ID: observationID, NativeKind: header.Type, Timestamp: timestamp, Locator: locator, Revision: state.generation.Revision(), Representation: record.NativeRepresentation()}}
	if timestampDiagnostic != nil {
		item.Diagnostics = append(item.Diagnostics, *timestampDiagnostic)
	}
	events, limitations, diagnostics, err := state.normalize(item.Observation, record.Data, header)
	if err != nil {
		return sessionio.ReadItem{}, state.adapter.error("read", string(state.session.ID), &locator, err)
	}
	item.Events, item.Diagnostics, item.Observation.Limitations = events, append(item.Diagnostics, diagnostics...), limitations
	if header.ParentUUID != "" {
		if parents := state.observations[header.ParentUUID]; len(parents) == 1 && parents[0] != observationID {
			parent := parents[0]
			item.Relations = append(item.Relations, state.relation(sessionio.RelationKindReplyTo, sessionio.NodeRef{Kind: sessionio.NodeKindObservation, ID: string(observationID)}, sessionio.NodeRef{Kind: sessionio.NodeKindObservation, ID: string(parent)}, sessionio.RelationOriginNative, []toolEvidence{{observation: observationID, locator: locator}}))
		} else {
			item.Diagnostics = append(item.Diagnostics, state.diagnostic("claude_parent_unresolved", "Claude parentUuid did not resolve uniquely", locator))
		}
	}
	if header.ForkedFrom.MessageUUID != "" {
		targets, err := state.adapter.forkTargets(ctx, state.occurrence, header.ForkedFrom.MessageUUID)
		if err != nil {
			return sessionio.ReadItem{}, state.adapter.error("read", string(state.session.ID), sourceErrorLocator(err, state.adapter.baseLocator(state.occurrence)), err)
		}
		if len(targets) == 1 {
			item.Relations = append(item.Relations, state.relation(sessionio.RelationKindBranchParent, sessionio.NodeRef{Kind: sessionio.NodeKindSession, ID: string(state.session.ID)}, sessionio.NodeRef{Kind: sessionio.NodeKindObservation, ID: string(targets[0])}, sessionio.RelationOriginNative, []toolEvidence{{observation: observationID, locator: locator}}))
		} else {
			item.Diagnostics = append(item.Diagnostics, state.diagnostic("claude_fork_target_unresolved", "Claude forkedFrom messageUuid did not resolve uniquely", locator))
		}
	}
	for _, event := range events {
		if event.ToolCall != nil {
			state.calls[event.ToolCall.CallID] = append(state.calls[event.ToolCall.CallID], toolEvidence{event: event.ID, observation: observationID, locator: locator})
			if !state.uniqueToolPair(event.ToolCall.CallID) {
				item.Diagnostics = append(item.Diagnostics, state.diagnostic("claude_tool_pair_unresolved", "Claude tool call has missing, dangling, or duplicate pair candidates", locator))
			}
		}
		if event.ToolResult != nil {
			state.results[event.ToolResult.CallID] = append(state.results[event.ToolResult.CallID], toolEvidence{event: event.ID, observation: observationID, locator: locator})
			if !state.uniqueToolPair(event.ToolResult.CallID) {
				item.Diagnostics = append(item.Diagnostics, state.diagnostic("claude_tool_pair_unresolved", "Claude tool result has missing, dangling, or duplicate pair candidates", locator))
			}
		}
	}
	callIDs := make([]string, 0, len(state.calls))
	for id := range state.calls {
		callIDs = append(callIDs, id)
	}
	sort.Strings(callIDs)
	for _, id := range callIDs {
		calls := state.calls[id]
		results := state.results[id]
		if state.uniqueToolPair(id) && len(calls) == 1 && len(results) == 1 && (calls[0].observation == observationID || results[0].observation == observationID) {
			item.Relations = append(item.Relations, state.toolPair(calls[0], results[0]))
		}
	}
	return item, nil
}

func (state *readState) sidecarError(err error) error {
	locator := state.adapter.sourceLocator(state.occurrence.sidecarPath)
	return state.adapter.error("read", string(state.session.ID), &locator, err)
}

func (state *readState) sidecarChangedError() error {
	return state.sidecarError(errors.New("agent metadata sidecar changed before emission"))
}

func (state *readState) sidecarItem() sessionio.ReadItem {
	locator := state.adapter.sourceLocator(state.occurrence.sidecarPath)
	digestValue := digest(state.sidecarBytes)
	observationID := sessionio.ObservationID(derivedID("observation", string(state.session.ID), "meta", state.occurrence.sidecarPath, digestValue))
	item := sessionio.ReadItem{Session: state.session, Observation: sessionio.NativeObservation{ID: observationID, NativeKind: "agent_metadata", Locator: locator, Revision: sessionio.Revision{Kind: sessionio.RevisionKindFileSnapshot, Value: "sha256:" + digestValue}, Representation: sessionio.NativeRepresentation{Capture: sessionio.CaptureKindByteExact, MediaType: "application/json", Data: append([]byte(nil), state.sidecarBytes...)}}}
	facts := []sessionio.Fact{}
	if state.sidecar.Model != "" {
		facts = append(facts, sessionio.Fact{Kind: sessionio.FactKindModel, Value: state.sidecar.Model})
	}
	if len(facts) != 0 {
		item.Events = append(item.Events, newEvent(item.Observation, 0, sessionio.EventKindFacts, &sessionio.FactEvent{Facts: facts}, nil, nil, nil, nil, nil, nil, nil))
	}
	return item
}

func (state *readState) normalize(observation sessionio.NativeObservation, data []byte, header recordHeader) ([]sessionio.Event, []sessionio.SourceLimitation, []sessionio.Diagnostic, error) {
	var record struct {
		Type                  string          `json:"type"`
		Message               json.RawMessage `json:"message"`
		Content               json.RawMessage `json:"content"`
		Subtype               string          `json:"subtype"`
		IsCompactSummary      bool            `json:"isCompactSummary"`
		PersistedOutputPath   string          `json:"persistedOutputPath"`
		Attachment            json.RawMessage `json:"attachment"`
		HookAdditionalContext json.RawMessage `json:"hookAdditionalContext"`
		ToolUseResult         json.RawMessage `json:"toolUseResult"`
		Model                 string          `json:"model"`
		CWD                   string          `json:"cwd"`
		GitBranch             string          `json:"gitBranch"`
		Usage                 json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, nil, nil, err
	}
	events := []sessionio.Event{}
	limitations := []sessionio.SourceLimitation{}
	diagnostics := []sessionio.Diagnostic{}
	if record.Type == "" {
		return nil, nil, nil, errors.New("Claude record type is required")
	}
	appendEvent := func(kind sessionio.EventKind, message *sessionio.MessageEvent, reasoning *sessionio.ReasoningEvent, call *sessionio.ToolCallEvent, result *sessionio.ToolResultEvent, usage *sessionio.UsageEvent, facts *sessionio.FactEvent, marker *sessionio.MarkerEvent, unknown *sessionio.UnknownEvent) sessionio.Event {
		item := newEvent(observation, len(events), kind, facts, message, reasoning, call, result, usage, marker, unknown)
		events = append(events, item)
		return item
	}
	appendOperational := func(name string, contents []json.RawMessage) error {
		operational, err := systemOperationalEvents(observation, len(events), name, contents)
		if err != nil {
			return err
		}
		events = append(events, operational...)
		return nil
	}
	operationalContents := func() []json.RawMessage {
		contents := []json.RawMessage{}
		for _, value := range []json.RawMessage{record.Content, record.Attachment, record.HookAdditionalContext} {
			if len(value) != 0 && string(value) != "null" {
				contents = append(contents, value)
			}
		}
		return contents
	}
	if record.Type == "user" || record.Type == "assistant" {
		role := sessionio.MessageRoleUser
		if record.Type == "assistant" {
			role = sessionio.MessageRoleAssistant
		}
		if record.IsCompactSummary {
			role = sessionio.MessageRoleSystem
		}
		content := record.Message
		var message struct {
			Content json.RawMessage `json:"content"`
			Usage   json.RawMessage `json:"usage"`
			Model   string          `json:"model"`
			Role    string          `json:"role"`
		}
		if len(record.Message) == 0 || string(record.Message) == "null" {
			return nil, nil, nil, errors.New("message is required")
		}
		if err := json.Unmarshal(record.Message, &message); err != nil {
			return nil, nil, nil, fmt.Errorf("message: %w", err)
		}
		expectedRole := string(role)
		if record.IsCompactSummary {
			expectedRole = "user"
		}
		if message.Role != expectedRole || len(message.Content) == 0 {
			return nil, nil, nil, errors.New("message role or content is invalid")
		}
		content = message.Content
		if record.Model == "" {
			record.Model = message.Model
		}
		if len(content) == 0 || string(content) == "null" {
			content = record.Content
		}
		blocks, thinking, calls, results, err := decodeMessageContent(observation, len(events), content)
		if err != nil {
			return nil, nil, nil, err
		}
		appendEvent(sessionio.EventKindMessage, &sessionio.MessageEvent{Role: role, Content: blocks}, nil, nil, nil, nil, nil, nil, nil)
		for _, value := range thinking {
			appendEvent(sessionio.EventKindReasoning, nil, &sessionio.ReasoningEvent{Content: []sessionio.ContentBlock{value}}, nil, nil, nil, nil, nil, nil)
		}
		for _, value := range calls {
			appendEvent(sessionio.EventKindToolCall, nil, nil, &value, nil, nil, nil, nil, nil)
		}
		for _, value := range results {
			appendEvent(sessionio.EventKindToolResult, nil, nil, nil, &value, nil, nil, nil, nil)
		}
		if record.Type == "assistant" {
			usageRaw := record.Usage
			if len(message.Usage) != 0 {
				usageRaw = message.Usage
			}
			if len(usageRaw) == 0 {
				var wrapper struct {
					Usage json.RawMessage `json:"usage"`
					Model string          `json:"model"`
				}
				if json.Unmarshal(content, &wrapper) == nil {
					usageRaw = wrapper.Usage
				}
			}
			if len(usageRaw) > 0 && string(usageRaw) != "null" {
				usage, supported, err := decodeUsage(usageRaw)
				if err != nil {
					return nil, nil, nil, err
				}
				if supported {
					appendEvent(sessionio.EventKindUsage, nil, nil, nil, nil, &usage, nil, nil, nil)
				}
			}
		}
		if record.IsCompactSummary {
			appendEvent(sessionio.EventKindMarker, nil, nil, nil, nil, nil, nil, &sessionio.MarkerEvent{Name: "compaction"}, nil)
		}
		external, err := state.externalLimitations(content)
		if err != nil {
			return nil, nil, nil, err
		}
		limitations = append(limitations, external...)
		external, err = state.externalLimitations(record.ToolUseResult)
		if err != nil {
			return nil, nil, nil, err
		}
		limitations = append(limitations, external...)
	} else if record.Type == "system" {
		if record.Subtype == "" {
			return nil, nil, nil, errors.New("system subtype is required")
		}
		if record.Subtype == "compact_boundary" {
			if err := appendOperational("compaction", operationalContents()); err != nil {
				return nil, nil, nil, err
			}
		} else if knownSystemSubtype(record.Subtype) {
			if err := appendOperational(record.Subtype, operationalContents()); err != nil {
				return nil, nil, nil, err
			}
		} else {
			for _, content := range operationalContents() {
				blocks, err := systemContent(observation, len(events), content)
				if err != nil {
					return nil, nil, nil, err
				}
				appendEvent(sessionio.EventKindMessage, &sessionio.MessageEvent{Role: sessionio.MessageRoleSystem, Content: blocks}, nil, nil, nil, nil, nil, nil, nil)
			}
			appendEvent(sessionio.EventKindUnknown, nil, nil, nil, nil, nil, nil, nil, &sessionio.UnknownEvent{NativeType: "system:" + record.Subtype})
			diagnostics = append(diagnostics, state.diagnostic("claude_unknown_system_subtype", "Claude system subtype has no normalized projection", observation.Locator))
		}
	} else if knownOperationalType(record.Type) {
		if err := appendOperational(record.Type, operationalContents()); err != nil {
			return nil, nil, nil, err
		}
	} else {
		appendEvent(sessionio.EventKindUnknown, nil, nil, nil, nil, nil, nil, nil, &sessionio.UnknownEvent{NativeType: record.Type})
		diagnostics = append(diagnostics, state.diagnostic("claude_unknown_record_type", "Claude record type has no normalized projection", observation.Locator))
	}
	if header.IsSidechain != nil {
		appendEvent(sessionio.EventKindMarker, nil, nil, nil, nil, nil, nil, &sessionio.MarkerEvent{Name: "sidechain", State: fmt.Sprintf("%t", *header.IsSidechain)}, nil)
	}
	facts := headerFacts(header)
	if record.Model != "" {
		facts = append(facts, sessionio.Fact{Kind: sessionio.FactKindModel, Value: record.Model})
	}
	if len(facts) > 0 {
		appendEvent(sessionio.EventKindFacts, nil, nil, nil, nil, nil, &sessionio.FactEvent{Facts: facts}, nil, nil)
	}
	if record.PersistedOutputPath != "" {
		limitation, err := state.externalLimitation(record.PersistedOutputPath)
		if err != nil {
			return nil, nil, nil, err
		}
		limitations = append(limitations, limitation)
	}
	return events, limitations, diagnostics, nil
}

func (state *readState) externalLimitations(content json.RawMessage) ([]sessionio.SourceLimitation, error) {
	if len(content) == 0 || string(content) == "null" {
		return nil, nil
	}
	var blocks []struct {
		PersistedOutputPath string `json:"persistedOutputPath"`
	}
	if content[0] == '{' {
		var single struct {
			PersistedOutputPath string `json:"persistedOutputPath"`
		}
		if err := json.Unmarshal(content, &single); err != nil {
			return nil, fmt.Errorf("parse tool-result external payload reference: %w", err)
		}
		if single.PersistedOutputPath == "" {
			return nil, nil
		}
		limitation, err := state.externalLimitation(single.PersistedOutputPath)
		if err != nil {
			return nil, err
		}
		return []sessionio.SourceLimitation{limitation}, nil
	}
	if content[0] != '[' {
		return nil, nil
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil, fmt.Errorf("parse tool-result external payload references: %w", err)
	}
	limitations := make([]sessionio.SourceLimitation, 0, len(blocks))
	for _, block := range blocks {
		if block.PersistedOutputPath != "" {
			limitation, err := state.externalLimitation(block.PersistedOutputPath)
			if err != nil {
				return nil, err
			}
			limitations = append(limitations, limitation)
		}
	}
	return limitations, nil
}

func (state *readState) externalLimitation(reference string) (sessionio.SourceLimitation, error) {
	path := filepath.FromSlash(reference)
	if !filepath.IsAbs(path) {
		path = filepath.Join(state.adapter.configDir, path)
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return sessionio.SourceLimitation{Kind: sessionio.LimitationKindMissingExternalPayload, Detail: reference}, nil
	}
	if err != nil {
		return sessionio.SourceLimitation{}, fmt.Errorf("stat external payload %q: %w", reference, err)
	}
	if !info.Mode().IsRegular() {
		return sessionio.SourceLimitation{}, fmt.Errorf("external payload %q is not a regular file", reference)
	}
	return sessionio.SourceLimitation{Kind: sessionio.LimitationKindExternalPayload, Detail: reference}, nil
}

func knownSystemSubtype(value string) bool {
	switch value {
	case "local_command", "stop_hook_summary", "turn_duration", "hook_summary", "away_summary":
		return true
	}
	return false
}
func knownOperationalType(value string) bool {
	switch value {
	case "attachment", "queue-operation", "file-history-delta", "file-history-snapshot", "ai-title", "custom-title", "last-prompt", "mode":
		return true
	}
	return false
}

func headerFacts(header recordHeader) []sessionio.Fact {
	facts := make([]sessionio.Fact, 0, 2)
	if header.CWD != "" {
		facts = append(facts, sessionio.Fact{Kind: sessionio.FactKindWorkingDirectory, Value: header.CWD})
	}
	if header.GitBranch != "" {
		facts = append(facts, sessionio.Fact{Kind: sessionio.FactKindGitBranch, Value: header.GitBranch})
	}
	return facts
}

func systemOperationalEvents(observation sessionio.NativeObservation, index int, name string, contents []json.RawMessage) ([]sessionio.Event, error) {
	events := make([]sessionio.Event, 0, len(contents)+1)
	for _, content := range contents {
		blocks, err := systemContent(observation, index+len(events), content)
		if err != nil {
			return nil, err
		}
		events = append(events, newEvent(observation, index+len(events), sessionio.EventKindMessage, nil, &sessionio.MessageEvent{Role: sessionio.MessageRoleSystem, Content: blocks}, nil, nil, nil, nil, nil, nil))
	}
	events = append(events, newEvent(observation, index+len(events), sessionio.EventKindMarker, nil, nil, nil, nil, nil, nil, &sessionio.MarkerEvent{Name: name}, nil))
	return events, nil
}

func decodeMessageContent(observation sessionio.NativeObservation, eventIndex int, raw json.RawMessage) ([]sessionio.ContentBlock, []sessionio.ContentBlock, []sessionio.ToolCallEvent, []sessionio.ToolResultEvent, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil, nil, nil, errors.New("message content is required")
	}
	eventID := sessionio.EventID(derivedID("event", string(observation.ID), fmt.Sprintf("%d", eventIndex), string(sessionio.EventKindMessage)))
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []sessionio.ContentBlock{textBlock(eventID, 0, text)}, nil, nil, nil, nil
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("message content must be a string or array: %w", err)
	}
	blocks := []sessionio.ContentBlock{}
	thinking := []sessionio.ContentBlock{}
	calls := []sessionio.ToolCallEvent{}
	results := []sessionio.ToolResultEvent{}
	for index, value := range values {
		var block struct {
			Type      string          `json:"type"`
			Text      *string         `json:"text"`
			Thinking  *string         `json:"thinking"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			IsError   bool            `json:"is_error"`
			Content   json.RawMessage `json:"content"`
			Source    struct {
				Type      string `json:"type"`
				MediaType string `json:"media_type"`
				Data      string `json:"data"`
				URL       string `json:"url"`
			} `json:"source"`
		}
		if err := json.Unmarshal(value, &block); err != nil {
			return nil, nil, nil, nil, err
		}
		if block.Type == "" {
			return nil, nil, nil, nil, errors.New("content block type is required")
		}
		switch block.Type {
		case "text":
			if block.Text == nil {
				return nil, nil, nil, nil, errors.New("text block text is required")
			}
			blocks = append(blocks, textBlock(eventID, index, *block.Text))
		case "thinking":
			if block.Thinking == nil {
				return nil, nil, nil, nil, errors.New("thinking block thinking is required")
			}
			reasoningIndex := eventIndex + 1 + len(thinking)
			reasoningID := sessionio.EventID(derivedID("event", string(observation.ID), fmt.Sprintf("%d", reasoningIndex), string(sessionio.EventKindReasoning)))
			thinking = append(thinking, textBlock(reasoningID, 0, *block.Thinking))
		case "image":
			media, err := imageBlock(eventID, index, block.Source)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			blocks = append(blocks, media)
		case "tool_use":
			if block.ID == "" || block.Name == "" || len(block.Input) == 0 || !json.Valid(block.Input) {
				return nil, nil, nil, nil, errors.New("tool_use requires id, name, and JSON input")
			}
			blocks = append(blocks, opaqueBlock(eventID, index, "tool_use", value))
			calls = append(calls, sessionio.ToolCallEvent{CallID: block.ID, Name: block.Name, Input: sessionio.Payload{MediaType: "application/json", Data: append([]byte(nil), block.Input...)}})
		case "tool_result":
			if block.ToolUseID == "" {
				return nil, nil, nil, nil, errors.New("tool_result requires tool_use_id")
			}
			blocks = append(blocks, opaqueBlock(eventID, index, "tool_result", value))
			payload, err := toolOutput(block.Content)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			status := sessionio.ToolResultStatusSuccess
			if block.IsError {
				status = sessionio.ToolResultStatusError
			}
			results = append(results, sessionio.ToolResultEvent{CallID: block.ToolUseID, Status: status, Output: payload})
		default:
			blocks = append(blocks, opaqueBlock(eventID, index, block.Type, value))
		}
	}
	return blocks, thinking, calls, results, nil
}

func imageBlock(eventID sessionio.EventID, index int, source struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}) (sessionio.ContentBlock, error) {
	if source.MediaType == "" {
		return sessionio.ContentBlock{}, errors.New("image source media_type is required")
	}
	block := sessionio.ContentBlock{ID: contentID(eventID, index, sessionio.ContentKindMedia), Kind: sessionio.ContentKindMedia, Availability: sessionio.ContentAvailabilityAvailable, Media: &sessionio.MediaContent{MediaType: source.MediaType}}
	switch source.Type {
	case "base64":
		data, err := base64.StdEncoding.DecodeString(source.Data)
		if err != nil {
			return sessionio.ContentBlock{}, fmt.Errorf("decode image base64: %w", err)
		}
		block.Media.Data = data
	case "url":
		if source.URL == "" {
			return sessionio.ContentBlock{}, errors.New("image URL is required")
		}
		block.Media.Reference = source.URL
		block.Availability = sessionio.ContentAvailabilityExternal
	default:
		return sessionio.ContentBlock{}, errors.New("unsupported image source type")
	}
	return block, nil
}
func toolOutput(raw json.RawMessage) (sessionio.Payload, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return sessionio.Payload{}, errors.New("tool_result content is required")
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return sessionio.Payload{MediaType: "text/plain; charset=utf-8", Data: []byte(text)}, nil
	}
	if !json.Valid(raw) {
		return sessionio.Payload{}, errors.New("tool_result content is invalid JSON")
	}
	return sessionio.Payload{MediaType: "application/json", Data: append([]byte(nil), raw...)}, nil
}
func systemContent(observation sessionio.NativeObservation, eventIndex int, raw json.RawMessage) ([]sessionio.ContentBlock, error) {
	eventID := sessionio.EventID(derivedID("event", string(observation.ID), fmt.Sprintf("%d", eventIndex), string(sessionio.EventKindMessage)))
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []sessionio.ContentBlock{textBlock(eventID, 0, text)}, nil
	}
	if !json.Valid(raw) {
		return nil, errors.New("system content is invalid JSON")
	}
	return []sessionio.ContentBlock{opaqueBlock(eventID, 0, "system_content", raw)}, nil
}
func decodeUsage(raw json.RawMessage) (sessionio.UsageEvent, bool, error) {
	var value struct {
		Input  *int64 `json:"input_tokens"`
		Output *int64 `json:"output_tokens"`
		Read   *int64 `json:"cache_read_input_tokens"`
		Write  *int64 `json:"cache_creation_input_tokens"`
		Total  *int64 `json:"total_tokens"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return sessionio.UsageEvent{}, false, err
	}
	usage := sessionio.UsageEvent{InputTokens: value.Input, OutputTokens: value.Output, CacheReadTokens: value.Read, CacheWriteTokens: value.Write, TotalTokens: value.Total}
	for _, counter := range []*int64{usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheWriteTokens, usage.TotalTokens} {
		if counter != nil && *counter < 0 {
			return sessionio.UsageEvent{}, false, errors.New("assistant usage counter is negative")
		}
	}
	if usage.InputTokens == nil && usage.OutputTokens == nil && usage.CacheReadTokens == nil && usage.CacheWriteTokens == nil && usage.TotalTokens == nil {
		return sessionio.UsageEvent{}, false, nil
	}
	return usage, true, nil
}

func newEvent(observation sessionio.NativeObservation, index int, kind sessionio.EventKind, facts *sessionio.FactEvent, message *sessionio.MessageEvent, reasoning *sessionio.ReasoningEvent, call *sessionio.ToolCallEvent, result *sessionio.ToolResultEvent, usage *sessionio.UsageEvent, marker *sessionio.MarkerEvent, unknown *sessionio.UnknownEvent) sessionio.Event {
	return sessionio.Event{ID: sessionio.EventID(derivedID("event", string(observation.ID), fmt.Sprintf("%d", index), string(kind))), Kind: kind, Timestamp: observation.Timestamp, Evidence: []sessionio.EvidenceRef{{Observation: observation.ID, Locator: observation.Locator}}, Facts: facts, Message: message, Reasoning: reasoning, ToolCall: call, ToolResult: result, Usage: usage, Marker: marker, Unknown: unknown}
}
func textBlock(eventID sessionio.EventID, index int, text string) sessionio.ContentBlock {
	return sessionio.ContentBlock{ID: contentID(eventID, index, sessionio.ContentKindText), Kind: sessionio.ContentKindText, Availability: sessionio.ContentAvailabilityAvailable, Text: &sessionio.TextContent{Text: text}}
}
func opaqueBlock(eventID sessionio.EventID, index int, nativeType string, data json.RawMessage) sessionio.ContentBlock {
	return sessionio.ContentBlock{ID: contentID(eventID, index, sessionio.ContentKindOpaque), Kind: sessionio.ContentKindOpaque, Availability: sessionio.ContentAvailabilityAvailable, Opaque: &sessionio.OpaqueContent{NativeType: nativeType, MediaType: "application/json", Data: append([]byte(nil), data...)}}
}
func contentID(eventID sessionio.EventID, index int, kind sessionio.ContentKind) sessionio.ContentID {
	return sessionio.ContentID(derivedID("content", string(eventID), fmt.Sprintf("%d", index), string(kind)))
}

func (state *readState) relation(kind sessionio.RelationKind, from, to sessionio.NodeRef, origin sessionio.RelationOrigin, evidence []toolEvidence) sessionio.Relation {
	inputs := []string{"relation", string(state.session.Occurrence.ID), string(kind), string(from.Kind), from.ID, string(to.Kind), to.ID}
	references := make([]sessionio.EvidenceRef, 0, len(evidence))
	for _, item := range evidence {
		inputs = append(inputs, string(item.observation))
		references = append(references, sessionio.EvidenceRef{Observation: item.observation, Locator: item.locator})
	}
	return sessionio.Relation{ID: sessionio.RelationID(derivedID(inputs[0], inputs[1:]...)), Kind: kind, From: from, To: to, Origin: origin, Evidence: references}
}
func (state *readState) toolPair(call, result toolEvidence) sessionio.Relation {
	return state.relation(sessionio.RelationKindToolPair, sessionio.NodeRef{Kind: sessionio.NodeKindEvent, ID: string(call.event)}, sessionio.NodeRef{Kind: sessionio.NodeKindEvent, ID: string(result.event)}, sessionio.RelationOriginDeterministic, []toolEvidence{call, result})
}

func (state *readState) uniqueToolPair(id string) bool {
	value := state.correlations[id]
	return value != nil && value.calls == 1 && value.results == 1
}
func (state *readState) diagnostic(code, message string, locator sessionio.SourceLocator) sessionio.Diagnostic {
	return sessionio.Diagnostic{Code: code, Severity: sessionio.DiagnosticSeverityWarning, Message: message, Locator: &locator}
}

type locatedError struct {
	locator sessionio.SourceLocator
	err     error
}

func (value *locatedError) Error() string { return value.err.Error() }
func (value *locatedError) Unwrap() error { return value.err }
func sourceErrorLocator(err error, fallback sessionio.FileLocator) *sessionio.SourceLocator {
	locator := sessionio.SourceLocator{Kind: sessionio.LocatorKindFile, File: &fallback}
	var located *locatedError
	if errors.As(err, &located) {
		locator = located.locator
	} else {
		var malformed *sourceio.MalformedJSONLError
		var oversized *sourceio.RecordTooLargeError
		var changed *sourceio.ChangedSourceError
		switch {
		case errors.As(err, &malformed):
			locator = malformed.Locator
		case errors.As(err, &oversized):
			locator = oversized.Locator
		case errors.As(err, &changed):
			locator = changed.Locator
		}
	}
	return &locator
}
func locatedSourceError(err error, fallback sessionio.FileLocator) error {
	return &locatedError{locator: *sourceErrorLocator(err, fallback), err: err}
}
func (adapter *Adapter) error(operation, sessionID string, locator *sessionio.SourceLocator, err error) error {
	return &sessionio.ReaderError{Operation: operation, Harness: sessionio.HarnessClaude, AdapterVersion: adapterVersion, SessionID: sessionio.SessionID(sessionID), Locator: locator, Err: err}
}
func (adapter *Adapter) validateContext(ctx context.Context) error {
	if adapter == nil {
		return errors.New("claude: nil adapter")
	}
	if ctx == nil {
		return errors.New("claude: context must not be nil")
	}
	return ctx.Err()
}
func sourceSelected(sources []sessionio.SourceID, expected sessionio.SourceID) bool {
	if len(sources) == 0 {
		return true
	}
	for index := range sources {
		if sources[index] == expected {
			return true
		}
	}
	return false
}
func streamValues[T any](values []T) (sessionio.Stream[T], error) {
	position := 0
	return sessionio.NewStream(func(context.Context) (T, error) {
		var zero T
		if position >= len(values) {
			return zero, io.EOF
		}
		value := values[position]
		position++
		return value, nil
	}, func() error { return nil })
}
func digest(parts ...[]byte) string {
	hash := sha256.New()
	for index := range parts {
		_, _ = hash.Write(parts[index])
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}
func derivedID(kind string, values ...string) string {
	hash := sha256.New()
	framed := append([]string{"sessionio/id/v1", kind}, values...)
	var length [4]byte
	for _, value := range framed {
		binary.BigEndian.PutUint32(length[:], uint32(len(value)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(value))
	}
	return fmt.Sprintf("%s:sha256:%x", kind, hash.Sum(nil))
}

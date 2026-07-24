package sessionio

import "time"

// Harness identifies a coding-agent harness.
type Harness string

const (
	HarnessCodex    Harness = "codex"
	HarnessClaude   Harness = "claude"
	HarnessOMP      Harness = "omp"
	HarnessOpenCode Harness = "opencode"
)

// SourceID is an opaque discovered-source identifier.
type SourceID string

// OccurrenceID is an opaque source-occurrence identifier.
type OccurrenceID string

// SessionID is an opaque occurrence-scoped session identifier.
type SessionID string

// ObservationID is an opaque source-native observation identifier.
type ObservationID string

// EventID is an opaque normalized-event identifier.
type EventID string

// ContentID is an opaque content-block identifier.
type ContentID string

// RelationID is an opaque structural-relation identifier.
type RelationID string

// DiscoveryRevision is an opaque, non-authoritative discovery change token.
type DiscoveryRevision string

// Capability identifies a reader feature exposed by a source or adapter.
type Capability string

const (
	CapabilityDiscovery          Capability = "discovery"
	CapabilityMessages           Capability = "messages"
	CapabilityRichContent        Capability = "rich_content"
	CapabilityTools              Capability = "tools"
	CapabilityReasoning          Capability = "reasoning"
	CapabilityBranches           Capability = "branches"
	CapabilityUsage              Capability = "usage"
	CapabilityEnvironment        Capability = "environment"
	CapabilityRepository         Capability = "repository"
	CapabilityIncrementalReading Capability = "incremental_reading"
)

// SupportLevel describes how completely a capability is implemented.
type SupportLevel string

const (
	SupportFull         SupportLevel = "full"
	SupportPartial      SupportLevel = "partial"
	SupportExperimental SupportLevel = "experimental"
	SupportUnavailable  SupportLevel = "unavailable"
)

// AdapterDescriptor declares an adapter's identity and capabilities.
type AdapterDescriptor struct {
	Harness      Harness            `json:"harness"`
	Version      string             `json:"version"`
	Capabilities []CapabilityStatus `json:"capabilities"`
}

// CapabilityStatus describes one capability.
type CapabilityStatus struct {
	Capability Capability   `json:"capability"`
	Support    SupportLevel `json:"support"`
	Detail     string       `json:"detail,omitempty"`
}

// SourceKind distinguishes canonical transcripts from auxiliary stores.
type SourceKind string

const (
	SourceKindCanonical SourceKind = "canonical"
	SourceKindAuxiliary SourceKind = "auxiliary"
)

// SourceStatus describes whether a discovered source can be read.
type SourceStatus string

const (
	SourceStatusAvailable   SourceStatus = "available"
	SourceStatusMissing     SourceStatus = "missing"
	SourceStatusDisabled    SourceStatus = "disabled"
	SourceStatusUnsupported SourceStatus = "unsupported"
)

// Source describes one discovered native source.
type Source struct {
	ID           SourceID           `json:"id"`
	Harness      Harness            `json:"harness"`
	Kind         SourceKind         `json:"kind"`
	Status       SourceStatus       `json:"status"`
	Locator      SourceLocator      `json:"locator"`
	Capabilities []CapabilityStatus `json:"capabilities,omitempty"`
	Diagnostics  []Diagnostic       `json:"diagnostics,omitempty"`
}

// SourceOccurrence identifies one observed instance of a source.
type SourceOccurrence struct {
	ID       OccurrenceID  `json:"id"`
	SourceID SourceID      `json:"source_id"`
	Harness  Harness       `json:"harness"`
	Locator  SourceLocator `json:"locator"`
}

// SessionRef identifies one native session within one source occurrence.
type SessionRef struct {
	ID                SessionID             `json:"id"`
	NativeID          string                `json:"native_id"`
	DiscoveryRevision DiscoveryRevision     `json:"discovery_revision"`
	Native            NativeSessionMetadata `json:"native"`
	Occurrence        SourceOccurrence      `json:"occurrence"`
	StartedAt         *time.Time            `json:"started_at,omitempty"`
	UpdatedAt         *time.Time            `json:"updated_at,omitempty"`
	Diagnostics       []Diagnostic          `json:"diagnostics,omitempty"`
}

// NativeIdentityKind identifies one native session identity.
type NativeIdentityKind string

const NativeIdentityKindSession NativeIdentityKind = "session"

// NativeRelationshipKind identifies a native session relationship hint.
type NativeRelationshipKind string

const (
	NativeRelationshipKindForkParent    NativeRelationshipKind = "fork_parent"
	NativeRelationshipKindControlParent NativeRelationshipKind = "control_parent"
)

// NativeSessionMetadata preserves native identities and session topology.
type NativeSessionMetadata struct {
	Identities    []NativeIdentity         `json:"identities,omitempty"`
	Relationships []NativeRelationshipHint `json:"relationships,omitempty"`
	Agent         *NativeAgentMetadata     `json:"agent,omitempty"`
	History       *NativeHistoryMetadata   `json:"history,omitempty"`
}

type NativeIdentity struct {
	Kind  NativeIdentityKind `json:"kind"`
	Value string             `json:"value"`
}

type NativeRelationshipHint struct {
	Kind           NativeRelationshipKind `json:"kind"`
	TargetNativeID string                 `json:"target_native_id"`
}

type NativeAgentMetadata struct {
	Nickname string `json:"nickname,omitempty"`
	Role     string `json:"role,omitempty"`
	Path     string `json:"path,omitempty"`
}

type NativeHistoryMetadata struct {
	BaseNativeID        string  `json:"base_native_id,omitempty"`
	EndOrdinalExclusive *uint64 `json:"end_ordinal_exclusive,omitempty"`
	EndByteOffset       *uint64 `json:"end_byte_offset,omitempty"`
	OwnStartOrdinal     *uint64 `json:"own_start_ordinal,omitempty"`
}

// SessionRequest filters session discovery by source.
type SessionRequest struct {
	Sources []SourceID
}

// LocatorKind selects the active SourceLocator variant.
type LocatorKind string

const (
	LocatorKindFile     LocatorKind = "file"
	LocatorKindDatabase LocatorKind = "database"
	LocatorKindOpaque   LocatorKind = "opaque"
)

// SourceLocator locates a source-native unit.
type SourceLocator struct {
	Kind     LocatorKind      `json:"kind"`
	File     *FileLocator     `json:"file,omitempty"`
	Database *DatabaseLocator `json:"database,omitempty"`
	Opaque   *OpaqueLocator   `json:"opaque,omitempty"`
}

// FileLocator identifies a record or byte range within a root-relative file.
type FileLocator struct {
	Root      string     `json:"root"`
	Path      string     `json:"path"`
	Record    *uint64    `json:"record,omitempty"`
	Line      *uint64    `json:"line,omitempty"`
	ByteRange *ByteRange `json:"byte_range,omitempty"`
}

// ByteRange is a half-open source byte range.
type ByteRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// DatabaseLocator identifies a row or event within a database.
type DatabaseLocator struct {
	Path  string        `json:"path"`
	Table string        `json:"table"`
	Keys  []DatabaseKey `json:"keys,omitempty"`
}

// DatabaseKey is one native database key component.
type DatabaseKey struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// OpaqueLocator identifies a unit through an adapter-defined scheme.
type OpaqueLocator struct {
	Scheme string `json:"scheme"`
	Value  string `json:"value"`
}

// RevisionKind identifies the source revision mechanism.
type RevisionKind string

const (
	RevisionKindFileSnapshot        RevisionKind = "file_snapshot"
	RevisionKindDatabaseTransaction RevisionKind = "database_transaction"
	RevisionKindEventSequence       RevisionKind = "event_sequence"
	RevisionKindOpaque              RevisionKind = "opaque"
)

// Revision identifies the immutable source state used for a read.
type Revision struct {
	Kind  RevisionKind `json:"kind"`
	Value string       `json:"value"`
}

// CaptureKind distinguishes exact bytes from a logical structured snapshot.
type CaptureKind string

const (
	CaptureKindByteExact          CaptureKind = "byte_exact"
	CaptureKindStructuredSnapshot CaptureKind = "structured_snapshot"
	CaptureKindDecodedStream      CaptureKind = "decoded_stream"
)

// NativeRepresentation preserves a source-native observation.
type NativeRepresentation struct {
	Capture   CaptureKind `json:"capture"`
	Codec     string      `json:"codec,omitempty"`
	MediaType string      `json:"media_type"`
	Data      []byte      `json:"data"`
	Framing   []byte      `json:"framing,omitempty"`
}

// LimitationKind identifies missing fidelity in the upstream source.
type LimitationKind string

const (
	LimitationKindUpstreamTruncation     LimitationKind = "upstream_truncation"
	LimitationKindExternalPayload        LimitationKind = "external_payload"
	LimitationKindMissingExternalPayload LimitationKind = "missing_external_payload"
	LimitationKindMutableMaterialization LimitationKind = "mutable_materialization"
)

// SourceLimitation records a native-source fidelity limitation.
type SourceLimitation struct {
	Kind   LimitationKind `json:"kind"`
	Detail string         `json:"detail,omitempty"`
}

// NativeObservation is the smallest independently locatable native unit.
type NativeObservation struct {
	ID             ObservationID        `json:"id"`
	NativeKind     string               `json:"native_kind"`
	NativeVersion  string               `json:"native_version,omitempty"`
	Timestamp      *time.Time           `json:"timestamp,omitempty"`
	Locator        SourceLocator        `json:"locator"`
	Revision       Revision             `json:"revision"`
	Representation NativeRepresentation `json:"representation"`
	Limitations    []SourceLimitation   `json:"limitations,omitempty"`
}

// ReadItem contains one native observation and its normalized projections.
type ReadItem struct {
	Session     SessionRef        `json:"session"`
	Observation NativeObservation `json:"observation"`
	Events      []Event           `json:"events,omitempty"`
	Relations   []Relation        `json:"relations,omitempty"`
	Diagnostics []Diagnostic      `json:"diagnostics,omitempty"`
}

// EventKind selects the active Event variant.
type EventKind string

const (
	EventKindMessage    EventKind = "message"
	EventKindReasoning  EventKind = "reasoning"
	EventKindToolCall   EventKind = "tool_call"
	EventKindToolResult EventKind = "tool_result"
	EventKindUsage      EventKind = "usage"
	EventKindFacts      EventKind = "facts"
	EventKindMarker     EventKind = "marker"
	EventKindUnknown    EventKind = "unknown"
)

// Event is one normalized event backed by native evidence.
type Event struct {
	ID         EventID          `json:"id"`
	Kind       EventKind        `json:"kind"`
	Timestamp  *time.Time       `json:"timestamp,omitempty"`
	Evidence   []EvidenceRef    `json:"evidence"`
	Message    *MessageEvent    `json:"message,omitempty"`
	Reasoning  *ReasoningEvent  `json:"reasoning,omitempty"`
	ToolCall   *ToolCallEvent   `json:"tool_call,omitempty"`
	ToolResult *ToolResultEvent `json:"tool_result,omitempty"`
	Usage      *UsageEvent      `json:"usage,omitempty"`
	Facts      *FactEvent       `json:"facts,omitempty"`
	Marker     *MarkerEvent     `json:"marker,omitempty"`
	Unknown    *UnknownEvent    `json:"unknown,omitempty"`
}

// EvidenceRef points from normalized data to a native observation.
type EvidenceRef struct {
	Observation ObservationID `json:"observation"`
	Locator     SourceLocator `json:"locator"`
}

// MessageRole identifies the native conversational role.
type MessageRole string

const (
	MessageRoleUser      MessageRole = "user"
	MessageRoleAssistant MessageRole = "assistant"
	MessageRoleDeveloper MessageRole = "developer"
	MessageRoleSystem    MessageRole = "system"
	MessageRoleTool      MessageRole = "tool"
	MessageRoleUnknown   MessageRole = "unknown"
)

// MessageEvent contains ordered native message content.
type MessageEvent struct {
	Role    MessageRole    `json:"role"`
	Content []ContentBlock `json:"content,omitempty"`
}

// ReasoningEvent contains reasoning and any separately exposed summary.
type ReasoningEvent struct {
	Content []ContentBlock `json:"content,omitempty"`
	Summary []ContentBlock `json:"summary,omitempty"`
}

// ContentKind selects the active ContentBlock variant.
type ContentKind string

const (
	ContentKindText   ContentKind = "text"
	ContentKindMedia  ContentKind = "media"
	ContentKindOpaque ContentKind = "opaque"
)

// ContentAvailability describes why content is or is not directly readable.
type ContentAvailability string

const (
	ContentAvailabilityAvailable   ContentAvailability = "available"
	ContentAvailabilityEncrypted   ContentAvailability = "encrypted"
	ContentAvailabilityRedacted    ContentAvailability = "redacted"
	ContentAvailabilityExternal    ContentAvailability = "external"
	ContentAvailabilityUnavailable ContentAvailability = "unavailable"
)

// ContentBlock is one typed message or reasoning content block.
type ContentBlock struct {
	ID           ContentID           `json:"id"`
	Kind         ContentKind         `json:"kind"`
	Availability ContentAvailability `json:"availability"`
	Text         *TextContent        `json:"text,omitempty"`
	Media        *MediaContent       `json:"media,omitempty"`
	Opaque       *OpaqueContent      `json:"opaque,omitempty"`
}

// TextContent contains a native text block.
type TextContent struct {
	Text string `json:"text"`
}

// MediaContent contains inline or externally referenced media.
type MediaContent struct {
	MediaType string `json:"media_type,omitempty"`
	Data      []byte `json:"data,omitempty"`
	Reference string `json:"reference,omitempty"`
}

// OpaqueContent preserves a harness-specific content block.
type OpaqueContent struct {
	NativeType string `json:"native_type"`
	MediaType  string `json:"media_type,omitempty"`
	Data       []byte `json:"data,omitempty"`
}

// Payload preserves a typed tool payload without interpreting its bytes.
type Payload struct {
	MediaType string `json:"media_type"`
	Data      []byte `json:"data"`
}

// ToolCallEvent contains one native tool invocation.
type ToolCallEvent struct {
	CallID string  `json:"call_id"`
	Name   string  `json:"name"`
	Input  Payload `json:"input"`
}

// ToolResultStatus describes the state represented by a tool result.
type ToolResultStatus string

const (
	ToolResultStatusSuccess ToolResultStatus = "success"
	ToolResultStatusError   ToolResultStatus = "error"
	ToolResultStatusPending ToolResultStatus = "pending"
	ToolResultStatusRunning ToolResultStatus = "running"
	ToolResultStatusUnknown ToolResultStatus = "unknown"
)

// ToolResultEvent contains one native tool result or state transition.
type ToolResultEvent struct {
	CallID string           `json:"call_id"`
	Status ToolResultStatus `json:"status"`
	Output Payload          `json:"output"`
}

// UsageEvent contains normalized optional token counters.
type UsageEvent struct {
	InputTokens      *int64 `json:"input_tokens,omitempty"`
	OutputTokens     *int64 `json:"output_tokens,omitempty"`
	ReasoningTokens  *int64 `json:"reasoning_tokens,omitempty"`
	CacheReadTokens  *int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens *int64 `json:"cache_write_tokens,omitempty"`
	TotalTokens      *int64 `json:"total_tokens,omitempty"`
}

// FactEvent contains normalized session facts.
type FactEvent struct {
	Facts []Fact `json:"facts"`
}

// FactKind identifies a normalized session fact.
type FactKind string

const (
	FactKindLaunchDirectory  FactKind = "launch_directory"
	FactKindWorkingDirectory FactKind = "working_directory"
	FactKindModel            FactKind = "model"
	FactKindProvider         FactKind = "provider"
	FactKindEffort           FactKind = "effort"
	FactKindGitRoot          FactKind = "git_root"
	FactKindGitRemote        FactKind = "git_remote"
	FactKindGitBranch        FactKind = "git_branch"
	FactKindGitCommit        FactKind = "git_commit"
	FactKindApprovalPolicy   FactKind = "approval_policy"
	FactKindSandboxPolicy    FactKind = "sandbox_policy"
	FactKindTimezone         FactKind = "timezone"
	FactKindCurrentDate      FactKind = "current_date"
)

// Fact is one typed normalized fact.
type Fact struct {
	Kind  FactKind `json:"kind"`
	Value string   `json:"value"`
}

// MarkerEvent preserves an operational or structural native marker.
type MarkerEvent struct {
	Name  string `json:"name"`
	State string `json:"state,omitempty"`
}

// UnknownEvent preserves the type of an otherwise unknown native record.
type UnknownEvent struct {
	NativeType string `json:"native_type"`
}

// RelationKind identifies a structural relation.
type RelationKind string

const (
	RelationKindPrevious     RelationKind = "previous"
	RelationKindNext         RelationKind = "next"
	RelationKindReplyTo      RelationKind = "reply_to"
	RelationKindBranchParent RelationKind = "branch_parent"
	RelationKindContains     RelationKind = "contains"
	RelationKindToolPair     RelationKind = "tool_pair"
	RelationKindMaterializes RelationKind = "materializes"
	RelationKindUpdates      RelationKind = "updates"
)

// RelationOrigin distinguishes native relations from deterministic projections.
type RelationOrigin string

const (
	RelationOriginNative        RelationOrigin = "native"
	RelationOriginDeterministic RelationOrigin = "deterministic"
)

// NodeKind identifies the object addressed by a relation endpoint.
type NodeKind string

const (
	NodeKindSession     NodeKind = "session"
	NodeKindObservation NodeKind = "observation"
	NodeKindEvent       NodeKind = "event"
	NodeKindContent     NodeKind = "content"
)

// Relation links two typed nodes using native evidence.
type Relation struct {
	ID       RelationID     `json:"id"`
	Kind     RelationKind   `json:"kind"`
	From     NodeRef        `json:"from"`
	To       NodeRef        `json:"to"`
	Origin   RelationOrigin `json:"origin"`
	Evidence []EvidenceRef  `json:"evidence"`
}

// NodeRef identifies one relation endpoint.
type NodeRef struct {
	Kind NodeKind `json:"kind"`
	ID   string   `json:"id"`
}

// DiagnosticSeverity identifies the impact of a reader diagnostic.
type DiagnosticSeverity string

const (
	DiagnosticSeverityInfo    DiagnosticSeverity = "info"
	DiagnosticSeverityWarning DiagnosticSeverity = "warning"
	DiagnosticSeverityError   DiagnosticSeverity = "error"
)

// Diagnostic reports a non-fatal source or capability condition.
type Diagnostic struct {
	Code     string             `json:"code"`
	Severity DiagnosticSeverity `json:"severity"`
	Message  string             `json:"message"`
	Locator  *SourceLocator     `json:"locator,omitempty"`
	Cause    error              `json:"-"`
}

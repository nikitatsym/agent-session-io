package sessionio

import "fmt"

func validateAdapterDescriptor(descriptor AdapterDescriptor) error {
	if descriptor.Harness == "" {
		return invalid("adapter.harness", "must not be empty")
	}
	if descriptor.Version == "" {
		return invalid("adapter.version", "must not be empty")
	}
	return validateCapabilityStatuses("adapter.capabilities", descriptor.Capabilities, true)
}

func validateCapabilityStatuses(path string, statuses []CapabilityStatus, required bool) error {
	if required && len(statuses) == 0 {
		return invalid(path, "must not be empty")
	}

	seen := make(map[Capability]struct{}, len(statuses))
	for index, status := range statuses {
		itemPath := fmt.Sprintf("%s[%d]", path, index)
		if !validCapability(status.Capability) {
			return invalid(itemPath+".capability", "unsupported value %q", status.Capability)
		}
		if !validSupportLevel(status.Support) {
			return invalid(itemPath+".support", "unsupported value %q", status.Support)
		}
		if _, exists := seen[status.Capability]; exists {
			return invalid(itemPath+".capability", "duplicate value %q", status.Capability)
		}
		seen[status.Capability] = struct{}{}
	}
	return nil
}

func validateSource(path string, source Source) error {
	if source.ID == "" {
		return invalid(path+".id", "must not be empty")
	}
	if source.Harness == "" {
		return invalid(path+".harness", "must not be empty")
	}
	if !validSourceKind(source.Kind) {
		return invalid(path+".kind", "unsupported value %q", source.Kind)
	}
	if !validSourceStatus(source.Status) {
		return invalid(path+".status", "unsupported value %q", source.Status)
	}
	if err := validateSourceLocator(path+".locator", source.Locator); err != nil {
		return err
	}
	if err := validateCapabilityStatuses(path+".capabilities", source.Capabilities, false); err != nil {
		return err
	}
	for index, diagnostic := range source.Diagnostics {
		if err := validateDiagnostic(fmt.Sprintf("%s.diagnostics[%d]", path, index), diagnostic); err != nil {
			return err
		}
	}
	return nil
}

func validateSessionRef(path string, session SessionRef) error {
	if session.ID == "" {
		return invalid(path+".id", "must not be empty")
	}
	if session.NativeID == "" {
		return invalid(path+".native_id", "must not be empty")
	}
	if session.DiscoveryRevision == "" {
		return invalid(path+".discovery_revision", "must not be empty")
	}
	if err := validateNativeSessionMetadata(path+".native", session.Native); err != nil {
		return err
	}
	for index, diagnostic := range session.Diagnostics {
		if err := validateDiagnostic(fmt.Sprintf("%s.diagnostics[%d]", path, index), diagnostic); err != nil {
			return err
		}
	}
	return validateSourceOccurrence(path+".occurrence", session.Occurrence)
}

func validateSourceOccurrence(path string, occurrence SourceOccurrence) error {
	if occurrence.ID == "" {
		return invalid(path+".id", "must not be empty")
	}
	if occurrence.SourceID == "" {
		return invalid(path+".source_id", "must not be empty")
	}
	if occurrence.Harness == "" {
		return invalid(path+".harness", "must not be empty")
	}
	if err := validateSourceLocator(path+".locator", occurrence.Locator); err != nil {
		return err
	}
	return nil
}

func validateNativeSessionMetadata(path string, metadata NativeSessionMetadata) error {
	identities := make(map[NativeIdentityKind]struct{}, len(metadata.Identities))
	for index, identity := range metadata.Identities {
		itemPath := fmt.Sprintf("%s.identities[%d]", path, index)
		if identity.Kind != NativeIdentityKindSession {
			return invalid(itemPath+".kind", "unsupported value %q", identity.Kind)
		}
		if identity.Value == "" {
			return invalid(itemPath+".value", "must not be empty")
		}
		if _, exists := identities[identity.Kind]; exists {
			return invalid(itemPath+".kind", "duplicate value %q", identity.Kind)
		}
		identities[identity.Kind] = struct{}{}
	}
	relationships := make(map[NativeRelationshipHint]struct{}, len(metadata.Relationships))
	for index, relationship := range metadata.Relationships {
		itemPath := fmt.Sprintf("%s.relationships[%d]", path, index)
		if relationship.Kind != NativeRelationshipKindForkParent && relationship.Kind != NativeRelationshipKindControlParent {
			return invalid(itemPath+".kind", "unsupported value %q", relationship.Kind)
		}
		if relationship.TargetNativeID == "" {
			return invalid(itemPath+".target_native_id", "must not be empty")
		}
		if _, exists := relationships[relationship]; exists {
			return invalid(itemPath, "duplicate relationship")
		}
		relationships[relationship] = struct{}{}
	}
	if metadata.History != nil {
		history := metadata.History
		if history.BaseNativeID == "" && (history.EndOrdinalExclusive != nil || history.EndByteOffset != nil) {
			return invalid(path+".history.base_native_id", "is required with history bounds")
		}
	}
	return nil
}

func validateSourceLocator(path string, locator SourceLocator) error {
	variants := 0
	if locator.File != nil {
		variants++
	}
	if locator.Database != nil {
		variants++
	}
	if locator.Opaque != nil {
		variants++
	}
	if variants != 1 {
		return invalid(path, "must contain exactly one locator variant")
	}

	switch locator.Kind {
	case LocatorKindFile:
		if locator.File == nil {
			return invalid(path, "kind %q requires file variant", locator.Kind)
		}
		return validateFileLocator(path+".file", *locator.File)
	case LocatorKindDatabase:
		if locator.Database == nil {
			return invalid(path, "kind %q requires database variant", locator.Kind)
		}
		return validateDatabaseLocator(path+".database", *locator.Database)
	case LocatorKindOpaque:
		if locator.Opaque == nil {
			return invalid(path, "kind %q requires opaque variant", locator.Kind)
		}
		return validateOpaqueLocator(path+".opaque", *locator.Opaque)
	default:
		return invalid(path+".kind", "unsupported value %q", locator.Kind)
	}
}

func validateFileLocator(path string, locator FileLocator) error {
	if locator.Root == "" {
		return invalid(path+".root", "must not be empty")
	}
	if locator.Path == "" {
		return invalid(path+".path", "must not be empty")
	}
	if locator.ByteRange == nil {
		return nil
	}
	if locator.ByteRange.Start < 0 {
		return invalid(path+".byte_range.start", "must not be negative")
	}
	if locator.ByteRange.End < 0 {
		return invalid(path+".byte_range.end", "must not be negative")
	}
	if locator.ByteRange.End < locator.ByteRange.Start {
		return invalid(path+".byte_range", "end must not precede start")
	}
	return nil
}

func validateDatabaseLocator(path string, locator DatabaseLocator) error {
	if locator.Path == "" {
		return invalid(path+".path", "must not be empty")
	}
	if locator.Table == "" {
		return invalid(path+".table", "must not be empty")
	}

	seen := make(map[string]struct{}, len(locator.Keys))
	for index, key := range locator.Keys {
		keyPath := fmt.Sprintf("%s.keys[%d]", path, index)
		if key.Name == "" {
			return invalid(keyPath+".name", "must not be empty")
		}
		if _, exists := seen[key.Name]; exists {
			return invalid(keyPath+".name", "duplicate value %q", key.Name)
		}
		seen[key.Name] = struct{}{}
	}
	return nil
}

func validateOpaqueLocator(path string, locator OpaqueLocator) error {
	if locator.Scheme == "" {
		return invalid(path+".scheme", "must not be empty")
	}
	if locator.Value == "" {
		return invalid(path+".value", "must not be empty")
	}
	return nil
}

func validateRevision(path string, revision Revision) error {
	if !validRevisionKind(revision.Kind) {
		return invalid(path+".kind", "unsupported value %q", revision.Kind)
	}
	if revision.Value == "" {
		return invalid(path+".value", "must not be empty")
	}
	return nil
}

func validateNativeObservation(path string, observation NativeObservation) error {
	if observation.ID == "" {
		return invalid(path+".id", "must not be empty")
	}
	if observation.NativeKind == "" {
		return invalid(path+".native_kind", "must not be empty")
	}
	if err := validateSourceLocator(path+".locator", observation.Locator); err != nil {
		return err
	}
	if err := validateRevision(path+".revision", observation.Revision); err != nil {
		return err
	}
	if err := validateNativeRepresentation(path+".representation", observation.Representation); err != nil {
		return err
	}
	for index, limitation := range observation.Limitations {
		if !validLimitationKind(limitation.Kind) {
			return invalid(
				fmt.Sprintf("%s.limitations[%d].kind", path, index),
				"unsupported value %q",
				limitation.Kind,
			)
		}
	}
	return nil
}

func validateNativeRepresentation(path string, representation NativeRepresentation) error {
	if !validCaptureKind(representation.Capture) {
		return invalid(path+".capture", "unsupported value %q", representation.Capture)
	}
	if representation.MediaType == "" {
		return invalid(path+".media_type", "must not be empty")
	}
	if representation.Data == nil {
		return invalid(path+".data", "must not be nil")
	}
	switch representation.Capture {
	case CaptureKindByteExact, CaptureKindStructuredSnapshot:
		if representation.Codec != "" {
			return invalid(path+".codec", "must be empty for %q capture", representation.Capture)
		}
		if representation.Capture == CaptureKindStructuredSnapshot && len(representation.Framing) != 0 {
			return invalid(path+".framing", "is only valid for byte-exact or decoded-stream capture")
		}
	case CaptureKindDecodedStream:
		if representation.Codec == "" {
			return invalid(path+".codec", "must not be empty for decoded-stream capture")
		}
	}
	return nil
}

func validateReadItem(path string, item ReadItem) error {
	if err := validateSessionRef(path+".session", item.Session); err != nil {
		return err
	}
	if err := validateNativeObservation(path+".observation", item.Observation); err != nil {
		return err
	}
	for index, event := range item.Events {
		if err := validateEvent(fmt.Sprintf("%s.events[%d]", path, index), event); err != nil {
			return err
		}
	}
	for index, relation := range item.Relations {
		if err := validateRelation(fmt.Sprintf("%s.relations[%d]", path, index), relation); err != nil {
			return err
		}
	}
	for index, diagnostic := range item.Diagnostics {
		if err := validateDiagnostic(fmt.Sprintf("%s.diagnostics[%d]", path, index), diagnostic); err != nil {
			return err
		}
	}
	return nil
}

func validateEvent(path string, event Event) error {
	if event.ID == "" {
		return invalid(path+".id", "must not be empty")
	}
	if len(event.Evidence) == 0 {
		return invalid(path+".evidence", "must not be empty")
	}
	for index, evidence := range event.Evidence {
		if err := validateEvidenceRef(fmt.Sprintf("%s.evidence[%d]", path, index), evidence); err != nil {
			return err
		}
	}

	variants := 0
	for _, present := range []bool{
		event.Message != nil,
		event.Reasoning != nil,
		event.ToolCall != nil,
		event.ToolResult != nil,
		event.Usage != nil,
		event.Facts != nil,
		event.Marker != nil,
		event.Unknown != nil,
	} {
		if present {
			variants++
		}
	}
	if variants != 1 {
		return invalid(path, "must contain exactly one event variant")
	}

	switch event.Kind {
	case EventKindMessage:
		if event.Message == nil {
			return invalid(path, "kind %q requires message variant", event.Kind)
		}
		return validateMessageEvent(path+".message", *event.Message)
	case EventKindReasoning:
		if event.Reasoning == nil {
			return invalid(path, "kind %q requires reasoning variant", event.Kind)
		}
		return validateReasoningEvent(path+".reasoning", *event.Reasoning)
	case EventKindToolCall:
		if event.ToolCall == nil {
			return invalid(path, "kind %q requires tool_call variant", event.Kind)
		}
		return validateToolCallEvent(path+".tool_call", *event.ToolCall)
	case EventKindToolResult:
		if event.ToolResult == nil {
			return invalid(path, "kind %q requires tool_result variant", event.Kind)
		}
		return validateToolResultEvent(path+".tool_result", *event.ToolResult)
	case EventKindUsage:
		if event.Usage == nil {
			return invalid(path, "kind %q requires usage variant", event.Kind)
		}
		return validateUsageEvent(path+".usage", *event.Usage)
	case EventKindFacts:
		if event.Facts == nil {
			return invalid(path, "kind %q requires facts variant", event.Kind)
		}
		return validateFactEvent(path+".facts", *event.Facts)
	case EventKindMarker:
		if event.Marker == nil {
			return invalid(path, "kind %q requires marker variant", event.Kind)
		}
		return validateMarkerEvent(path+".marker", *event.Marker)
	case EventKindUnknown:
		if event.Unknown == nil {
			return invalid(path, "kind %q requires unknown variant", event.Kind)
		}
		return validateUnknownEvent(path+".unknown", *event.Unknown)
	default:
		return invalid(path+".kind", "unsupported value %q", event.Kind)
	}
}

func validateEvidenceRef(path string, evidence EvidenceRef) error {
	if evidence.Observation == "" {
		return invalid(path+".observation", "must not be empty")
	}
	return validateSourceLocator(path+".locator", evidence.Locator)
}

func validateMessageEvent(path string, event MessageEvent) error {
	if !validMessageRole(event.Role) {
		return invalid(path+".role", "unsupported value %q", event.Role)
	}
	return validateContentBlocks(path+".content", event.Content)
}

func validateReasoningEvent(path string, event ReasoningEvent) error {
	if err := validateContentBlocks(path+".content", event.Content); err != nil {
		return err
	}
	return validateContentBlocks(path+".summary", event.Summary)
}

func validateContentBlocks(path string, blocks []ContentBlock) error {
	for index, block := range blocks {
		if err := validateContentBlock(fmt.Sprintf("%s[%d]", path, index), block); err != nil {
			return err
		}
	}
	return nil
}

func validateContentBlock(path string, block ContentBlock) error {
	if block.ID == "" {
		return invalid(path+".id", "must not be empty")
	}
	if !validContentAvailability(block.Availability) {
		return invalid(path+".availability", "unsupported value %q", block.Availability)
	}

	variants := 0
	if block.Text != nil {
		variants++
	}
	if block.Media != nil {
		variants++
	}
	if block.Opaque != nil {
		variants++
	}
	if variants != 1 {
		return invalid(path, "must contain exactly one content variant")
	}

	switch block.Kind {
	case ContentKindText:
		if block.Text == nil {
			return invalid(path, "kind %q requires text variant", block.Kind)
		}
	case ContentKindMedia:
		if block.Media == nil {
			return invalid(path, "kind %q requires media variant", block.Kind)
		}
	case ContentKindOpaque:
		if block.Opaque == nil {
			return invalid(path, "kind %q requires opaque variant", block.Kind)
		}
		if block.Opaque.NativeType == "" {
			return invalid(path+".opaque.native_type", "must not be empty")
		}
	default:
		return invalid(path+".kind", "unsupported value %q", block.Kind)
	}
	return nil
}

func validateToolCallEvent(path string, event ToolCallEvent) error {
	if event.CallID == "" {
		return invalid(path+".call_id", "must not be empty")
	}
	if event.Name == "" {
		return invalid(path+".name", "must not be empty")
	}
	return validatePayload(path+".input", event.Input)
}

func validateToolResultEvent(path string, event ToolResultEvent) error {
	if event.CallID == "" {
		return invalid(path+".call_id", "must not be empty")
	}
	if !validToolResultStatus(event.Status) {
		return invalid(path+".status", "unsupported value %q", event.Status)
	}
	return validatePayload(path+".output", event.Output)
}

func validatePayload(path string, payload Payload) error {
	if payload.MediaType == "" {
		return invalid(path+".media_type", "must not be empty")
	}
	if payload.Data == nil {
		return invalid(path+".data", "must not be nil")
	}
	return nil
}

func validateUsageEvent(path string, event UsageEvent) error {
	counters := []*int64{
		event.InputTokens,
		event.OutputTokens,
		event.ReasoningTokens,
		event.CacheReadTokens,
		event.CacheWriteTokens,
		event.TotalTokens,
	}
	present := false
	for index, counter := range counters {
		if counter == nil {
			continue
		}
		present = true
		if *counter < 0 {
			return invalid(fmt.Sprintf("%s.counter[%d]", path, index), "must not be negative")
		}
	}
	if !present {
		return invalid(path, "must contain at least one token counter")
	}
	return nil
}

func validateFactEvent(path string, event FactEvent) error {
	if len(event.Facts) == 0 {
		return invalid(path+".facts", "must not be empty")
	}
	for index, fact := range event.Facts {
		factPath := fmt.Sprintf("%s.facts[%d]", path, index)
		if !validFactKind(fact.Kind) {
			return invalid(factPath+".kind", "unsupported value %q", fact.Kind)
		}
		if fact.Value == "" {
			return invalid(factPath+".value", "must not be empty")
		}
	}
	return nil
}

func validateMarkerEvent(path string, event MarkerEvent) error {
	if event.Name == "" {
		return invalid(path+".name", "must not be empty")
	}
	return nil
}

func validateUnknownEvent(path string, event UnknownEvent) error {
	if event.NativeType == "" {
		return invalid(path+".native_type", "must not be empty")
	}
	return nil
}

func validateRelation(path string, relation Relation) error {
	if relation.ID == "" {
		return invalid(path+".id", "must not be empty")
	}
	if !validRelationKind(relation.Kind) {
		return invalid(path+".kind", "unsupported value %q", relation.Kind)
	}
	if err := validateNodeRef(path+".from", relation.From); err != nil {
		return err
	}
	if err := validateNodeRef(path+".to", relation.To); err != nil {
		return err
	}
	if !validRelationOrigin(relation.Origin) {
		return invalid(path+".origin", "unsupported value %q", relation.Origin)
	}
	if len(relation.Evidence) == 0 {
		return invalid(path+".evidence", "must not be empty")
	}
	for index, evidence := range relation.Evidence {
		if err := validateEvidenceRef(fmt.Sprintf("%s.evidence[%d]", path, index), evidence); err != nil {
			return err
		}
	}
	return nil
}

func validateNodeRef(path string, ref NodeRef) error {
	if !validNodeKind(ref.Kind) {
		return invalid(path+".kind", "unsupported value %q", ref.Kind)
	}
	if ref.ID == "" {
		return invalid(path+".id", "must not be empty")
	}
	return nil
}

func validateDiagnostic(path string, diagnostic Diagnostic) error {
	if diagnostic.Code == "" {
		return invalid(path+".code", "must not be empty")
	}
	if !validDiagnosticSeverity(diagnostic.Severity) {
		return invalid(path+".severity", "unsupported value %q", diagnostic.Severity)
	}
	if diagnostic.Message == "" {
		return invalid(path+".message", "must not be empty")
	}
	if diagnostic.Locator != nil {
		return validateSourceLocator(path+".locator", *diagnostic.Locator)
	}
	return nil
}

func invalid(path, format string, arguments ...any) error {
	return fmt.Errorf("sessionio: invalid %s: %s", path, fmt.Sprintf(format, arguments...))
}

func validCapability(value Capability) bool {
	switch value {
	case CapabilityDiscovery,
		CapabilityMessages,
		CapabilityRichContent,
		CapabilityTools,
		CapabilityReasoning,
		CapabilityBranches,
		CapabilityUsage,
		CapabilityEnvironment,
		CapabilityRepository,
		CapabilityIncrementalReading:
		return true
	default:
		return false
	}
}

func validSupportLevel(value SupportLevel) bool {
	switch value {
	case SupportFull, SupportPartial, SupportExperimental, SupportUnavailable:
		return true
	default:
		return false
	}
}

func validSourceKind(value SourceKind) bool {
	return value == SourceKindCanonical || value == SourceKindAuxiliary
}

func validSourceStatus(value SourceStatus) bool {
	switch value {
	case SourceStatusAvailable, SourceStatusMissing, SourceStatusDisabled, SourceStatusUnsupported:
		return true
	default:
		return false
	}
}

func validRevisionKind(value RevisionKind) bool {
	switch value {
	case RevisionKindFileSnapshot,
		RevisionKindDatabaseTransaction,
		RevisionKindEventSequence,
		RevisionKindOpaque:
		return true
	default:
		return false
	}
}

func validCaptureKind(value CaptureKind) bool {
	return value == CaptureKindByteExact || value == CaptureKindStructuredSnapshot || value == CaptureKindDecodedStream
}

func validLimitationKind(value LimitationKind) bool {
	switch value {
	case LimitationKindUpstreamTruncation,
		LimitationKindExternalPayload,
		LimitationKindMissingExternalPayload,
		LimitationKindMutableMaterialization:
		return true
	default:
		return false
	}
}

func validMessageRole(value MessageRole) bool {
	switch value {
	case MessageRoleUser,
		MessageRoleAssistant,
		MessageRoleDeveloper,
		MessageRoleSystem,
		MessageRoleTool,
		MessageRoleUnknown:
		return true
	default:
		return false
	}
}

func validContentAvailability(value ContentAvailability) bool {
	switch value {
	case ContentAvailabilityAvailable,
		ContentAvailabilityEncrypted,
		ContentAvailabilityRedacted,
		ContentAvailabilityExternal,
		ContentAvailabilityUnavailable:
		return true
	default:
		return false
	}
}

func validToolResultStatus(value ToolResultStatus) bool {
	switch value {
	case ToolResultStatusSuccess,
		ToolResultStatusError,
		ToolResultStatusPending,
		ToolResultStatusRunning,
		ToolResultStatusUnknown:
		return true
	default:
		return false
	}
}

func validFactKind(value FactKind) bool {
	switch value {
	case FactKindLaunchDirectory,
		FactKindWorkingDirectory,
		FactKindModel,
		FactKindProvider,
		FactKindEffort,
		FactKindGitRoot,
		FactKindGitRemote,
		FactKindGitBranch,
		FactKindGitCommit,
		FactKindApprovalPolicy,
		FactKindSandboxPolicy,
		FactKindTimezone,
		FactKindCurrentDate:
		return true
	default:
		return false
	}
}

func validRelationKind(value RelationKind) bool {
	switch value {
	case RelationKindPrevious,
		RelationKindNext,
		RelationKindReplyTo,
		RelationKindBranchParent,
		RelationKindContains,
		RelationKindToolPair,
		RelationKindMaterializes,
		RelationKindUpdates,
		RelationKindActiveLeaf:
		return true
	default:
		return false
	}
}

func validRelationOrigin(value RelationOrigin) bool {
	return value == RelationOriginNative || value == RelationOriginDeterministic
}

func validNodeKind(value NodeKind) bool {
	switch value {
	case NodeKindSession, NodeKindObservation, NodeKindEvent, NodeKindContent:
		return true
	default:
		return false
	}
}

func validDiagnosticSeverity(value DiagnosticSeverity) bool {
	switch value {
	case DiagnosticSeverityInfo, DiagnosticSeverityWarning, DiagnosticSeverityError:
		return true
	default:
		return false
	}
}

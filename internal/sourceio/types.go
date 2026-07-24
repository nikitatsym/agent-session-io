package sourceio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"

	sessionio "github.com/nikitatsym/agent-session-io"
)

// TailMode controls how an unterminated final JSONL candidate is handled.
type TailMode string

const (
	TailModeGrowing TailMode = "growing"
	TailModeFinal   TailMode = "final"
)

// UnlimitedRecordBytes disables the explicit per-record size limit.
const UnlimitedRecordBytes int64 = -1

// RecordSizePolicy bounds one materialized native record.
type RecordSizePolicy struct {
	MaxBytes int64
}

// FileSpec separates the I/O path from literal provenance.
type FileSpec struct {
	OpenPath string
	Locator  sessionio.FileLocator
}

// OpenOptions configures one fixed JSONL generation.
type OpenOptions struct {
	TailMode   TailMode
	SizePolicy RecordSizePolicy
	Resume     *ResumeToken
}

// OpenResult contains an available generation and its lifecycle classification.
type OpenResult struct {
	Generation     *JSONLGeneration
	Reconciliation Reconciliation
}

// FileIdentity is an opaque process-local identity from an opened handle.
type FileIdentity struct {
	info os.FileInfo
}

// ResumeToken separates observed generation state from the confirmed cursor.
type ResumeToken struct {
	Identity              FileIdentity
	GenerationSize        int64
	GenerationRevision    sessionio.Revision
	ConfirmedEnd          int64
	NextRecord            uint64
	NextLine              uint64
	ConfirmedPrefixSHA256 [sha256.Size]byte
}

// JSONLRecord is one verified byte-exact native JSONL record.
type JSONLRecord struct {
	Record    uint64
	Line      uint64
	ByteRange sessionio.ByteRange
	Data      []byte
	Framing   []byte
}

// SourceLocator attaches record provenance to a literal file locator.
func (record JSONLRecord) SourceLocator(base sessionio.FileLocator) sessionio.SourceLocator {
	number := record.Record
	line := record.Line
	byteRange := record.ByteRange
	file := base
	file.Record = &number
	file.Line = &line
	file.ByteRange = &byteRange
	return sessionio.SourceLocator{
		Kind: sessionio.LocatorKindFile,
		File: &file,
	}
}

// NativeRepresentation returns the byte-exact JSON representation.
func (record JSONLRecord) NativeRepresentation() sessionio.NativeRepresentation {
	return sessionio.NativeRepresentation{
		Capture:   sessionio.CaptureKindByteExact,
		MediaType: "application/json",
		Data:      record.Data,
		Framing:   record.Framing,
	}
}

// TailKind distinguishes clean completion from an unconfirmed growing tail.
type TailKind string

const (
	TailKindClean   TailKind = "clean"
	TailKindPending TailKind = "pending"
)

// TailState describes bytes not represented by complete records.
type TailState struct {
	Kind      TailKind
	ByteRange *sessionio.ByteRange
}

// FileChangeKind classifies a container change independently of resume safety.
type FileChangeKind string

const (
	FileChangeInitial     FileChangeKind = "initial"
	FileChangeUnchanged   FileChangeKind = "unchanged"
	FileChangeGrown       FileChangeKind = "grown"
	FileChangeTruncated   FileChangeKind = "truncated"
	FileChangeRewritten   FileChangeKind = "rewritten"
	FileChangeReplaced    FileChangeKind = "replaced"
	FileChangeDisappeared FileChangeKind = "disappeared"
)

// ResumeAction states whether the confirmed cursor remains byte-safe.
type ResumeAction string

const (
	ResumeContinue    ResumeAction = "continue"
	ResumeReplay      ResumeAction = "replay"
	ResumeUnavailable ResumeAction = "unavailable"
)

// Reconciliation combines container lifecycle with cursor safety.
type Reconciliation struct {
	Change FileChangeKind
	Resume ResumeAction
}

// OpenJSONLGeneration acquires and indexes one bounded JSONL generation.
func OpenJSONLGeneration(
	ctx context.Context,
	spec FileSpec,
	options OpenOptions,
) (OpenResult, error) {
	return openJSONLGeneration(ctx, spec, options)
}

func revisionFromDigest(digest [sha256.Size]byte) sessionio.Revision {
	return sessionio.Revision{
		Kind:  sessionio.RevisionKindFileSnapshot,
		Value: "sha256:" + hex.EncodeToString(digest[:]),
	}
}

func revisionDigest(revision sessionio.Revision) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	if revision.Kind != sessionio.RevisionKindFileSnapshot {
		return digest, fmt.Errorf(
			"sourceio: revision kind %q is not a file snapshot",
			revision.Kind,
		)
	}
	const prefix = "sha256:"
	if len(revision.Value) != len(prefix)+hex.EncodedLen(sha256.Size) ||
		revision.Value[:len(prefix)] != prefix {
		return digest, fmt.Errorf("sourceio: invalid file revision %q", revision.Value)
	}
	decoded, err := hex.DecodeString(revision.Value[len(prefix):])
	if err != nil {
		return digest, fmt.Errorf("sourceio: invalid file revision %q: %w", revision.Value, err)
	}
	copy(digest[:], decoded)
	return digest, nil
}

func sameIdentity(first FileIdentity, second FileIdentity) bool {
	return first.info != nil &&
		second.info != nil &&
		os.SameFile(first.info, second.info)
}

package catalog

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func fixtureBlob(t *testing.T, payload string) (SnapshotBlob, stateBlob) {
	t.Helper()
	blob, err := CompressSnapshot([]byte(payload))
	if err != nil {
		t.Fatalf("compress snapshot: %v", err)
	}
	return blob, stateBlob{
		Schema:           StateBlobSchema,
		ContentHash:      hex.EncodeToString(blob.ContentHash),
		Codec:            blob.Codec,
		CodecVersion:     blob.CodecVersion,
		UncompressedSize: blob.UncompressedSize,
		CompressedSize:   blob.CompressedSize,
		Checksum:         hex.EncodeToString(blob.Checksum),
		Data:             base64.StdEncoding.EncodeToString(blob.Data),
		CreatedAt:        time.Unix(0, 0).UTC(),
	}
}

func fixtureStream(t *testing.T) stateStream {
	t.Helper()
	blob, encoded := fixtureBlob(t, "{\"type\":\"user\"}\n")
	moment := time.Unix(0, 0).UTC()
	revision := SessionRevision{
		SessionKey:          "session-1",
		OccurrenceID:        "occurrence-1",
		NativeID:            "native-1",
		DiscoveryRevision:   "discovery-1",
		SourceRevisionValue: "sha256:abc",
		SnapshotHash:        blob.ContentHash,
	}
	revision.RevisionHash = RevisionHash(revision)
	return stateStream{
		sources: []stateSource{{
			Schema:   StateSourceSchema,
			SourceID: "source-1",
			stateIdentity: stateIdentity{
				Harness:     "codex",
				LocatorKind: "file",
				LocatorRoot: "/root",
				LocatorPath: "sessions",
				FirstSeenAt: moment,
				LastSeenAt:  moment,
			},
		}},
		occurrences: []stateOccurrence{{
			Schema:       StateOccurrenceSchema,
			OccurrenceID: "occurrence-1",
			SourceID:     "source-1",
			stateIdentity: stateIdentity{
				Harness:     "codex",
				LocatorKind: "file",
				LocatorRoot: "/root",
				LocatorPath: "sessions/one.jsonl",
				FirstSeenAt: moment,
				LastSeenAt:  moment,
			},
		}},
		blobs: []stateBlob{encoded},
		revisions: []stateRevision{{
			Schema:              StateRevisionSchema,
			RevisionHash:        hex.EncodeToString(revision.RevisionHash),
			SessionKey:          revision.SessionKey,
			OccurrenceID:        revision.OccurrenceID,
			Harness:             "codex",
			NativeID:            revision.NativeID,
			DiscoveryRevision:   revision.DiscoveryRevision,
			SourceRevisionKind:  "file_snapshot",
			SourceRevisionValue: revision.SourceRevisionValue,
			SnapshotHash:        encoded.ContentHash,
			LocatorKind:         "file",
			LocatorRoot:         "/root",
			LocatorPath:         "sessions/one.jsonl",
			EventCount:          1,
			ObservedAt:          moment,
		}},
		checkpoints: []stateCheckpoint{{
			Schema:              StateCheckpointSchema,
			OccurrenceID:        revision.OccurrenceID,
			RevisionHash:        hex.EncodeToString(revision.RevisionHash),
			DiscoveryRevision:   revision.DiscoveryRevision,
			SourceRevisionValue: revision.SourceRevisionValue,
			SnapshotHash:        encoded.ContentHash,
			SnapshotSize:        blob.UncompressedSize,
			SourceSize:          blob.UncompressedSize,
			RecordCount:         1,
			FileIdentity:        "unix:1:2",
			TailKind:            TailClean,
			ChangeKind:          ChangeInitial,
		}},
	}
}

func renderStream(t *testing.T, stream stateStream) []byte {
	t.Helper()
	lines, err := encodeState(stream)
	if err != nil {
		t.Fatalf("encode state: %v", err)
	}
	return renderLines(t, stream.counts(), lines)
}

func renderLines(t *testing.T, counts StateCounts, lines [][]byte) []byte {
	t.Helper()
	manifest, err := json.Marshal(stateManifest{
		Schema:          StateManifestSchema,
		CatalogSchema:   "sessionio_test",
		CatalogRevision: Revision,
		ExportedAt:      time.Unix(0, 0).UTC(),
		Counts:          counts,
		Checksum:        checksumLines(lines),
	})
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	var buffer bytes.Buffer
	buffer.Write(append(manifest, '\n'))
	for _, line := range lines {
		buffer.Write(line)
	}
	return buffer.Bytes()
}

func requireStateCorrupt(t *testing.T, err error, fragment string) {
	t.Helper()
	var typed *Error
	if !errors.As(err, &typed) || typed.Kind != KindCatalogStateCorrupt {
		t.Fatalf("error = %v, want %q", err, KindCatalogStateCorrupt)
	}
	if !strings.Contains(typed.Message, fragment) {
		t.Fatalf("message = %q, want it to mention %q", typed.Message, fragment)
	}
	if typed.Remediation == "" {
		t.Fatal("a corrupt state stream carries no remediation")
	}
}

func TestStateStreamRoundTripsEveryRetainedRecord(t *testing.T) {
	stream := fixtureStream(t)
	manifest, decoded, err := decodeState(bytes.NewReader(renderStream(t, stream)))
	if err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if manifest.Counts != stream.counts() {
		t.Fatalf("counts = %+v, want %+v", manifest.Counts, stream.counts())
	}
	if len(decoded.sources) != 1 || len(decoded.occurrences) != 1 ||
		len(decoded.blobs) != 1 || len(decoded.revisions) != 1 ||
		len(decoded.checkpoints) != 1 {
		t.Fatalf("decoded stream = %+v", decoded)
	}
	if decoded.checkpoints[0].ChangeKind != ChangeInitial ||
		decoded.checkpoints[0].TailKind != TailClean {
		t.Fatalf("checkpoint = %+v", decoded.checkpoints[0])
	}
}

// One flipped byte anywhere in the payload must be caught before any write.
func TestOneCorruptByteFailsTheStateChecksum(t *testing.T) {
	rendered := renderStream(t, fixtureStream(t))
	corrupt := bytes.Replace(rendered, []byte("source-1"), []byte("source-2"), 1)
	if bytes.Equal(rendered, corrupt) {
		t.Fatal("the corruption did not change the stream")
	}
	_, _, err := decodeState(bytes.NewReader(corrupt))
	requireStateCorrupt(t, err, "checksum")
}

// A stream whose checksum was recomputed over tampered bytes still cannot
// smuggle a broken snapshot past import validation.
func TestTamperedSnapshotPayloadFailsIntegrity(t *testing.T) {
	stream := fixtureStream(t)
	data, err := base64.StdEncoding.DecodeString(stream.blobs[0].Data)
	if err != nil {
		t.Fatalf("decode fixture blob: %v", err)
	}
	data[len(data)/2] ^= 0xff
	stream.blobs[0].Data = base64.StdEncoding.EncodeToString(data)
	_, _, err = decodeState(bytes.NewReader(renderStream(t, stream)))
	requireStateCorrupt(t, err, "integrity")
}

func TestStateRecordsMustReferenceRetainedIdentities(t *testing.T) {
	stream := fixtureStream(t)
	stream.occurrences[0].SourceID = "source-absent"
	_, _, err := decodeState(bytes.NewReader(renderStream(t, stream)))
	requireStateCorrupt(t, err, "absent source")
}

func TestStateRecordSchemasAreClosed(t *testing.T) {
	stream := fixtureStream(t)
	lines, err := encodeState(stream)
	if err != nil {
		t.Fatalf("encode state: %v", err)
	}
	lines = append(lines, []byte(`{"schema":"sessionio.catalog.future/v1"}`+"\n"))
	_, _, err = decodeState(bytes.NewReader(renderLines(t, stream.counts(), lines)))
	requireStateCorrupt(t, err, "unsupported schema")
}

func TestStateManifestCountsMustMatchTheStream(t *testing.T) {
	stream := fixtureStream(t)
	lines, err := encodeState(stream)
	if err != nil {
		t.Fatalf("encode state: %v", err)
	}
	counts := stream.counts()
	counts.Sources++
	_, _, err = decodeState(bytes.NewReader(renderLines(t, counts, lines)))
	requireStateCorrupt(t, err, "manifest record counts")
}

func TestEmptyStreamCarriesNoManifest(t *testing.T) {
	_, _, err := decodeState(bytes.NewReader(nil))
	requireStateCorrupt(t, err, "empty")
}

// notHex has the length of a sha256 digest but cannot be decoded, so only a
// hex check stands between it and a silently truncated retained hash.
const notHex = "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz" +
	"zzzzzzzzzzzzzzzzzzzzzzzz"

func TestRetainedHashesMustBeHex(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		corrupt func(*stateStream)
		want    string
	}{
		{
			name: "revision hash",
			corrupt: func(stream *stateStream) {
				stream.revisions[0].RevisionHash = notHex
				stream.checkpoints[0].RevisionHash = notHex
			},
			want: "session revision carries a malformed revision hash",
		},
		{
			name: "checkpoint snapshot hash",
			corrupt: func(stream *stateStream) {
				stream.checkpoints[0].SnapshotHash = notHex
			},
			want: "checkpoint carries a malformed snapshot hash",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			stream := fixtureStream(t)
			testCase.corrupt(&stream)
			_, _, err := decodeState(bytes.NewReader(renderStream(t, stream)))
			requireStateCorrupt(t, err, testCase.want)
		})
	}
}

func TestSnapshotCompressionIsDeterministicAndVerified(t *testing.T) {
	payload := []byte(strings.Repeat("{\"type\":\"assistant\"}\n", 64))
	first, err := CompressSnapshot(payload)
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	second, err := CompressSnapshot(payload)
	if err != nil {
		t.Fatalf("compress again: %v", err)
	}
	if !bytes.Equal(first.Data, second.Data) ||
		!bytes.Equal(first.ContentHash, second.ContentHash) {
		t.Fatal("snapshot compression is not deterministic")
	}
	restored, err := DecompressSnapshot(first)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if !bytes.Equal(restored, payload) {
		t.Fatal("snapshot did not restore byte for byte")
	}
	first.Data[0] ^= 0xff
	if _, err := DecompressSnapshot(first); err == nil {
		t.Fatal("a corrupt blob restored without an error")
	}
}

// Equal snapshot bytes may share one blob, but never one revision: distinct
// occurrences stay distinct observations.
func TestEqualSnapshotsKeepDistinctRevisions(t *testing.T) {
	blob, err := CompressSnapshot([]byte("{\"type\":\"user\"}\n"))
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	first := RevisionHash(SessionRevision{
		SessionKey:   "session-a",
		OccurrenceID: "occurrence-a",
		SnapshotHash: blob.ContentHash,
	})
	second := RevisionHash(SessionRevision{
		SessionKey:   "session-b",
		OccurrenceID: "occurrence-b",
		SnapshotHash: blob.ContentHash,
	})
	if bytes.Equal(first, second) {
		t.Fatal("two occurrences of equal bytes share one session revision")
	}
}

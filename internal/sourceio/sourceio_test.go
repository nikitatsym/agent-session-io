package sourceio

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	sessionio "github.com/nikitatsym/agent-session-io"
)

func TestJSONLGenerationExactFraming(t *testing.T) {
	content := []byte("{\"id\":1}\r\n{\"id\":2}\n{\"id\":3}")
	spec := writeFixture(t, "mixed.jsonl", content)

	result := openGeneration(t, spec, OpenOptions{
		TailMode:   TailModeFinal,
		SizePolicy: unlimitedPolicy(),
	})
	defer closeGeneration(t, result.Generation)
	if result.Reconciliation != (Reconciliation{
		Change: FileChangeInitial,
		Resume: ResumeReplay,
	}) {
		t.Fatalf("reconciliation = %#v", result.Reconciliation)
	}

	records := readAll(t, result.Generation)
	if len(records) != 3 {
		t.Fatalf("record count = %d, want 3", len(records))
	}

	expectedData := [][]byte{
		[]byte("{\"id\":1}"),
		[]byte("{\"id\":2}"),
		[]byte("{\"id\":3}"),
	}
	expectedFraming := [][]byte{
		[]byte("\r\n"),
		[]byte("\n"),
		nil,
	}
	offset := int64(0)
	var reconstructed []byte
	for index, record := range records {
		number := uint64(index + 1)
		if record.Record != number || record.Line != number {
			t.Fatalf("record %d numbering = %d/%d", index, record.Record, record.Line)
		}
		if !bytes.Equal(record.Data, expectedData[index]) {
			t.Fatalf("record %d data = %q", index, record.Data)
		}
		if !bytes.Equal(record.Framing, expectedFraming[index]) {
			t.Fatalf("record %d framing = %q", index, record.Framing)
		}
		expectedRange := sessionio.ByteRange{
			Start: offset,
			End:   offset + int64(len(record.Data)),
		}
		if record.ByteRange != expectedRange {
			t.Fatalf("record %d range = %#v, want %#v", index, record.ByteRange, expectedRange)
		}
		offset = expectedRange.End + int64(len(record.Framing))
		reconstructed = append(reconstructed, record.Data...)
		reconstructed = append(reconstructed, record.Framing...)
	}
	if !bytes.Equal(reconstructed, content) {
		t.Fatalf("reconstructed bytes = %q, want %q", reconstructed, content)
	}

	tail := result.Generation.Tail()
	if tail.Kind != TailKindClean || tail.ByteRange != nil {
		t.Fatalf("tail = %#v, want clean", tail)
	}
	assertRevision(t, result.Generation.Revision(), content)

	token := result.Generation.ResumeToken()
	if token.ConfirmedEnd != int64(len(content)) ||
		token.NextRecord != 4 ||
		token.NextLine != 4 {
		t.Fatalf("resume token cursor = %#v", token)
	}

	records[0].Data[0] = '!'
	if records[1].Data[0] != '{' || records[2].Data[0] != '{' {
		t.Fatal("returned record slices alias")
	}
}

func TestJSONLGenerationGrowingAndFinalTail(t *testing.T) {
	first := []byte("{\"id\":1}\n")
	tail := []byte("{\"id\":2}")
	content := append(append([]byte(nil), first...), tail...)
	spec := writeFixture(t, "tail.jsonl", content)

	growing := openGeneration(t, spec, OpenOptions{
		TailMode:   TailModeGrowing,
		SizePolicy: unlimitedPolicy(),
	})
	records := readAll(t, growing.Generation)
	if len(records) != 1 || !bytes.Equal(records[0].Data, []byte("{\"id\":1}")) {
		t.Fatalf("growing records = %#v", records)
	}
	expectedTailRange := sessionio.ByteRange{
		Start: int64(len(first)),
		End:   int64(len(content)),
	}
	tailState := growing.Generation.Tail()
	if tailState.Kind != TailKindPending ||
		tailState.ByteRange == nil ||
		*tailState.ByteRange != expectedTailRange {
		t.Fatalf("growing tail = %#v, want %#v", tailState, expectedTailRange)
	}
	growingToken := growing.Generation.ResumeToken()
	if growingToken.ConfirmedEnd != int64(len(first)) ||
		growingToken.GenerationSize != int64(len(content)) {
		t.Fatalf("growing resume token = %#v", growingToken)
	}
	closeGeneration(t, growing.Generation)

	final := openGeneration(t, spec, OpenOptions{
		TailMode:   TailModeFinal,
		SizePolicy: unlimitedPolicy(),
		Resume:     &growingToken,
	})
	if final.Reconciliation != (Reconciliation{
		Change: FileChangeUnchanged,
		Resume: ResumeContinue,
	}) {
		t.Fatalf("final reconciliation = %#v", final.Reconciliation)
	}
	finalRecords := readAll(t, final.Generation)
	if len(finalRecords) != 1 ||
		!bytes.Equal(finalRecords[0].Data, tail) ||
		len(finalRecords[0].Framing) != 0 {
		t.Fatalf("final records = %#v", finalRecords)
	}
	closeGeneration(t, final.Generation)

	malformedSpec := writeFixture(t, "growing-malformed-tail.jsonl", []byte("not-json"))
	malformedGrowing := openGeneration(t, malformedSpec, OpenOptions{
		TailMode:   TailModeGrowing,
		SizePolicy: unlimitedPolicy(),
	})
	if records := readAll(t, malformedGrowing.Generation); len(records) != 0 {
		t.Fatalf("malformed growing-tail records = %#v", records)
	}
	malformedTail := malformedGrowing.Generation.Tail()
	if malformedTail.Kind != TailKindPending ||
		malformedTail.ByteRange == nil ||
		*malformedTail.ByteRange != (sessionio.ByteRange{Start: 0, End: 8}) {
		t.Fatalf("malformed growing tail = %#v", malformedTail)
	}
	closeGeneration(t, malformedGrowing.Generation)

	emptySpec := writeFixture(t, "empty.jsonl", nil)
	empty := openGeneration(t, emptySpec, OpenOptions{
		TailMode:   TailModeFinal,
		SizePolicy: unlimitedPolicy(),
	})
	if records := readAll(t, empty.Generation); len(records) != 0 {
		t.Fatalf("empty records = %#v", records)
	}
	if empty.Generation.Tail().Kind != TailKindClean {
		t.Fatalf("empty tail = %#v", empty.Generation.Tail())
	}
	closeGeneration(t, empty.Generation)
}

func TestJSONLGenerationRejectsMalformedRecords(t *testing.T) {
	tests := []struct {
		name       string
		content    []byte
		wantRecord uint64
		wantStart  int64
		wantEnd    int64
	}{
		{
			name:       "terminated malformed",
			content:    []byte("{\"id\":1}\nnot-json\n"),
			wantRecord: 2,
			wantStart:  9,
			wantEnd:    17,
		},
		{
			name:       "final malformed tail",
			content:    []byte("not-json"),
			wantRecord: 1,
			wantStart:  0,
			wantEnd:    8,
		},
		{
			name:       "empty",
			content:    []byte("\n"),
			wantRecord: 1,
			wantStart:  0,
			wantEnd:    0,
		},
		{
			name:       "whitespace",
			content:    []byte(" \t \r\n"),
			wantRecord: 1,
			wantStart:  0,
			wantEnd:    3,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := writeFixture(t, "malformed.jsonl", test.content)
			result, err := OpenJSONLGeneration(
				context.Background(),
				spec,
				OpenOptions{
					TailMode:   TailModeFinal,
					SizePolicy: unlimitedPolicy(),
				},
			)
			if result.Generation != nil {
				t.Fatal("malformed input returned a generation")
			}
			var malformed *MalformedJSONLError
			if !errors.As(err, &malformed) {
				t.Fatalf("error = %T %v, want MalformedJSONLError", err, err)
			}
			assertErrorLocator(
				t,
				malformed.Locator,
				test.wantRecord,
				test.wantRecord,
				test.wantStart,
				test.wantEnd,
			)
			if errors.Unwrap(malformed) == nil {
				t.Fatal("MalformedJSONLError does not unwrap")
			}
		})
	}
}

func TestJSONLGenerationLargeRecordAndLimit(t *testing.T) {
	data := []byte(`{"value":"` + strings.Repeat("x", 1<<20) + "\"}\n")
	spec := writeFixture(t, "large.jsonl", data)

	result := openGeneration(t, spec, OpenOptions{
		TailMode:   TailModeFinal,
		SizePolicy: unlimitedPolicy(),
	})
	records := readAll(t, result.Generation)
	if len(records) != 1 || len(records[0].Data) <= 1<<20 {
		t.Fatalf("large record size = %d", len(records[0].Data))
	}
	reconstructed := append(append([]byte(nil), records[0].Data...), records[0].Framing...)
	if !bytes.Equal(reconstructed, data) {
		t.Fatal("large record did not round-trip")
	}
	closeGeneration(t, result.Generation)

	limitedResult, err := OpenJSONLGeneration(
		context.Background(),
		spec,
		OpenOptions{
			TailMode: TailModeFinal,
			SizePolicy: RecordSizePolicy{
				MaxBytes: 1024,
			},
		},
	)
	if limitedResult.Generation != nil {
		t.Fatal("oversized record returned a generation")
	}
	var tooLarge *RecordTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("error = %T %v, want RecordTooLargeError", err, err)
	}
	if tooLarge.Limit != 1024 || tooLarge.ObservedAtLeast <= tooLarge.Limit {
		t.Fatalf("record size error = %#v", tooLarge)
	}
	assertErrorLocator(t, tooLarge.Locator, 1, 1, 0, tooLarge.ObservedAtLeast)
}

func TestJSONLGenerationRecordLimitExcludesSplitCRLF(t *testing.T) {
	data := []byte(`{"v":"` + strings.Repeat("x", 4087) + `"}`)
	if len(data) != 4095 {
		t.Fatalf("fixture data length = %d, want 4095", len(data))
	}
	spec := writeFixture(t, "split-crlf.jsonl", append(append([]byte(nil), data...), '\r', '\n'))

	accepted := openGeneration(t, spec, OpenOptions{
		TailMode: TailModeFinal,
		SizePolicy: RecordSizePolicy{
			MaxBytes: int64(len(data)),
		},
	})
	records := readAll(t, accepted.Generation)
	if len(records) != 1 ||
		!bytes.Equal(records[0].Data, data) ||
		!bytes.Equal(records[0].Framing, []byte("\r\n")) {
		t.Fatalf("split-CRLF records = %#v", records)
	}
	closeGeneration(t, accepted.Generation)

	result, err := OpenJSONLGeneration(
		context.Background(),
		spec,
		OpenOptions{
			TailMode: TailModeFinal,
			SizePolicy: RecordSizePolicy{
				MaxBytes: int64(len(data) - 1),
			},
		},
	)
	if result.Generation != nil {
		t.Fatal("over-limit split-CRLF record returned a generation")
	}
	var tooLarge *RecordTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("error = %T %v, want RecordTooLargeError", err, err)
	}
}

func TestJSONLGenerationAppendAndResume(t *testing.T) {
	first := []byte("{\"id\":1}\n")
	partial := []byte("{\"id\":")
	spec := writeFixture(t, "append.jsonl", append(append([]byte(nil), first...), partial...))

	initial := openGeneration(t, spec, OpenOptions{
		TailMode:   TailModeGrowing,
		SizePolicy: unlimitedPolicy(),
	})
	initialRecords := readAll(t, initial.Generation)
	if len(initialRecords) != 1 {
		t.Fatalf("initial record count = %d", len(initialRecords))
	}
	token := initial.Generation.ResumeToken()
	closeGeneration(t, initial.Generation)

	appendFixture(t, spec.OpenPath, []byte("2}\n{\"id\":3}\n"))
	resumed := openGeneration(t, spec, OpenOptions{
		TailMode:   TailModeGrowing,
		SizePolicy: unlimitedPolicy(),
		Resume:     &token,
	})
	if resumed.Reconciliation != (Reconciliation{
		Change: FileChangeGrown,
		Resume: ResumeContinue,
	}) {
		t.Fatalf("reconciliation = %#v", resumed.Reconciliation)
	}
	resumedRecords := readAll(t, resumed.Generation)
	if len(resumedRecords) != 2 ||
		!bytes.Equal(resumedRecords[0].Data, []byte("{\"id\":2}")) ||
		!bytes.Equal(resumedRecords[1].Data, []byte("{\"id\":3}")) {
		t.Fatalf("resumed records = %#v", resumedRecords)
	}
	closeGeneration(t, resumed.Generation)
}

func TestJSONLGenerationPendingTailRewriteCanContinue(t *testing.T) {
	prefix := []byte("{\"id\":1}\n")
	oldTail := []byte("{\"id\":2}")
	newTail := []byte("{\"id\":3}")
	spec, token := growingFixtureToken(t, "pending-rewrite.jsonl", prefix, oldTail)

	writeFixturePath(t, spec.OpenPath, append(append([]byte(nil), prefix...), newTail...))
	resumed := openGeneration(t, spec, OpenOptions{
		TailMode:   TailModeFinal,
		SizePolicy: unlimitedPolicy(),
		Resume:     &token,
	})
	if resumed.Reconciliation != (Reconciliation{
		Change: FileChangeRewritten,
		Resume: ResumeContinue,
	}) {
		t.Fatalf("reconciliation = %#v", resumed.Reconciliation)
	}
	records := readAll(t, resumed.Generation)
	if len(records) != 1 || !bytes.Equal(records[0].Data, newTail) {
		t.Fatalf("records = %#v", records)
	}
	closeGeneration(t, resumed.Generation)
}

func TestJSONLGenerationConfirmedPrefixRewriteReplays(t *testing.T) {
	original := []byte("{\"id\":1}\n{\"id\":2}\n")
	rewritten := []byte("{\"id\":9}\n{\"id\":2}\n")
	spec := writeFixture(t, "prefix-rewrite.jsonl", original)

	initial := openGeneration(t, spec, OpenOptions{
		TailMode:   TailModeFinal,
		SizePolicy: unlimitedPolicy(),
	})
	readAll(t, initial.Generation)
	token := initial.Generation.ResumeToken()
	closeGeneration(t, initial.Generation)

	writeFixturePath(t, spec.OpenPath, rewritten)
	resumed := openGeneration(t, spec, OpenOptions{
		TailMode:   TailModeFinal,
		SizePolicy: unlimitedPolicy(),
		Resume:     &token,
	})
	if resumed.Reconciliation != (Reconciliation{
		Change: FileChangeRewritten,
		Resume: ResumeReplay,
	}) {
		t.Fatalf("reconciliation = %#v", resumed.Reconciliation)
	}
	records := readAll(t, resumed.Generation)
	if len(records) != 2 || !bytes.Equal(records[0].Data, []byte("{\"id\":9}")) {
		t.Fatalf("replayed records = %#v", records)
	}
	closeGeneration(t, resumed.Generation)
}

func TestJSONLGenerationLifecycleChanges(t *testing.T) {
	content := []byte("{\"id\":1}\n{\"id\":2}\n")

	t.Run("replacement with equal bytes", func(t *testing.T) {
		spec, token := completedFixtureToken(t, "replace.jsonl", content)
		replacement := filepath.Join(filepath.Dir(spec.OpenPath), "replacement.jsonl")
		writeFixturePath(t, replacement, content)
		if err := os.Rename(replacement, spec.OpenPath); err != nil {
			if removeErr := os.Remove(spec.OpenPath); removeErr != nil {
				t.Fatalf("remove replacement target: %v", removeErr)
			}
			if renameErr := os.Rename(replacement, spec.OpenPath); renameErr != nil {
				t.Fatalf("replace fixture: %v", renameErr)
			}
		}

		result := openGeneration(t, spec, OpenOptions{
			TailMode:   TailModeFinal,
			SizePolicy: unlimitedPolicy(),
			Resume:     &token,
		})
		if result.Reconciliation != (Reconciliation{
			Change: FileChangeReplaced,
			Resume: ResumeContinue,
		}) {
			t.Fatalf("reconciliation = %#v", result.Reconciliation)
		}
		closeGeneration(t, result.Generation)
	})

	t.Run("truncation", func(t *testing.T) {
		spec, token := completedFixtureToken(t, "truncate.jsonl", content)
		if err := os.Truncate(spec.OpenPath, int64(len("{\"id\":1}\n"))); err != nil {
			t.Fatalf("truncate fixture: %v", err)
		}

		result := openGeneration(t, spec, OpenOptions{
			TailMode:   TailModeFinal,
			SizePolicy: unlimitedPolicy(),
			Resume:     &token,
		})
		if result.Reconciliation != (Reconciliation{
			Change: FileChangeTruncated,
			Resume: ResumeReplay,
		}) {
			t.Fatalf("reconciliation = %#v", result.Reconciliation)
		}
		closeGeneration(t, result.Generation)
	})

	t.Run("disappearance", func(t *testing.T) {
		spec, token := completedFixtureToken(t, "missing.jsonl", content)
		if err := os.Remove(spec.OpenPath); err != nil {
			t.Fatalf("remove fixture: %v", err)
		}

		result, err := OpenJSONLGeneration(
			context.Background(),
			spec,
			OpenOptions{
				TailMode:   TailModeFinal,
				SizePolicy: unlimitedPolicy(),
				Resume:     &token,
			},
		)
		if err != nil {
			t.Fatalf("OpenJSONLGeneration() error = %v", err)
		}
		if result.Generation != nil ||
			result.Reconciliation != (Reconciliation{
				Change: FileChangeDisappeared,
				Resume: ResumeUnavailable,
			}) {
			t.Fatalf("result = %#v", result)
		}
	})
}

func TestJSONLGenerationDetectsMutationBetweenPasses(t *testing.T) {
	spec := writeFixture(t, "mutated.jsonl", []byte("{\"id\":1}\n"))
	result := openGeneration(t, spec, OpenOptions{
		TailMode:   TailModeFinal,
		SizePolicy: unlimitedPolicy(),
	})

	writeFixturePath(t, spec.OpenPath, []byte("{\"id\":2}\n"))
	record, err := result.Generation.Next(context.Background())
	if len(record.Data) != 0 {
		t.Fatalf("changed record returned data %q", record.Data)
	}
	var changed *ChangedSourceError
	if !errors.As(err, &changed) {
		t.Fatalf("Next() error = %T %v, want ChangedSourceError", err, err)
	}
	assertErrorLocator(t, changed.Locator, 1, 1, 0, 8)
	if changed.ExpectedSHA256 == changed.ActualSHA256 {
		t.Fatal("changed-source digests are equal")
	}
	closeGeneration(t, result.Generation)
}

func TestJSONLGenerationDetectsPendingTailMutation(t *testing.T) {
	prefix := []byte("{\"id\":1}\n")
	tail := []byte("{\"pending\":")
	spec := writeFixture(
		t,
		"mutated-tail.jsonl",
		append(append([]byte(nil), prefix...), tail...),
	)
	result := openGeneration(t, spec, OpenOptions{
		TailMode:   TailModeGrowing,
		SizePolicy: unlimitedPolicy(),
	})

	record, err := result.Generation.Next(context.Background())
	if err != nil || !bytes.Equal(record.Data, []byte("{\"id\":1}")) {
		t.Fatalf("first Next() = %#v, %v", record, err)
	}
	writeFixturePath(
		t,
		spec.OpenPath,
		append(append([]byte(nil), prefix...), []byte("{\"changed\":")...),
	)

	_, err = result.Generation.Next(context.Background())
	var changed *ChangedSourceError
	if !errors.As(err, &changed) {
		t.Fatalf("tail Next() error = %T %v, want ChangedSourceError", err, err)
	}
	token := result.Generation.ResumeToken()
	if token.ConfirmedEnd != int64(len(prefix)) ||
		token.NextRecord != 2 ||
		token.NextLine != 2 {
		t.Fatalf("changed-tail resume token = %#v", token)
	}
	if _, repeatedErr := result.Generation.Next(context.Background()); repeatedErr != err {
		t.Fatalf("repeated terminal error = %v, want %v", repeatedErr, err)
	}
	closeGeneration(t, result.Generation)
}

func TestJSONLGenerationExcludesAppendAfterAcquisition(t *testing.T) {
	first := []byte("{\"id\":1}\n")
	second := []byte("{\"id\":2}\n")
	spec := writeFixture(t, "bounded.jsonl", first)
	initial := openGeneration(t, spec, OpenOptions{
		TailMode:   TailModeGrowing,
		SizePolicy: unlimitedPolicy(),
	})

	appendFixture(t, spec.OpenPath, second)
	records := readAll(t, initial.Generation)
	if len(records) != 1 || !bytes.Equal(records[0].Data, []byte("{\"id\":1}")) {
		t.Fatalf("bounded records = %#v", records)
	}
	token := initial.Generation.ResumeToken()
	closeGeneration(t, initial.Generation)

	next := openGeneration(t, spec, OpenOptions{
		TailMode:   TailModeGrowing,
		SizePolicy: unlimitedPolicy(),
		Resume:     &token,
	})
	if next.Reconciliation != (Reconciliation{
		Change: FileChangeGrown,
		Resume: ResumeContinue,
	}) {
		t.Fatalf("next reconciliation = %#v", next.Reconciliation)
	}
	nextRecords := readAll(t, next.Generation)
	if len(nextRecords) != 1 || !bytes.Equal(nextRecords[0].Data, []byte("{\"id\":2}")) {
		t.Fatalf("next records = %#v", nextRecords)
	}
	closeGeneration(t, next.Generation)
}

func TestJSONLGenerationSeparatesChangeFromResumeSafety(t *testing.T) {
	prefix := []byte("{\"id\":1}\n")
	tail := []byte("{\"pending\":true}")
	spec, token := growingFixtureToken(t, "truncate-tail.jsonl", prefix, tail)

	writeFixturePath(
		t,
		spec.OpenPath,
		append(append([]byte(nil), prefix...), []byte("{\"x\":")...),
	)
	truncated := openGeneration(t, spec, OpenOptions{
		TailMode:   TailModeGrowing,
		SizePolicy: unlimitedPolicy(),
		Resume:     &token,
	})
	if truncated.Reconciliation != (Reconciliation{
		Change: FileChangeTruncated,
		Resume: ResumeContinue,
	}) {
		t.Fatalf("truncated reconciliation = %#v", truncated.Reconciliation)
	}
	closeGeneration(t, truncated.Generation)

	original := []byte("{\"id\":1}\n")
	spec = writeFixture(t, "rewrite-and-grow.jsonl", original)
	initial := openGeneration(t, spec, OpenOptions{
		TailMode:   TailModeGrowing,
		SizePolicy: unlimitedPolicy(),
	})
	readAll(t, initial.Generation)
	token = initial.Generation.ResumeToken()
	closeGeneration(t, initial.Generation)

	writeFixturePath(t, spec.OpenPath, []byte("{\"id\":9}\n{\"id\":2}\n"))
	rewritten := openGeneration(t, spec, OpenOptions{
		TailMode:   TailModeGrowing,
		SizePolicy: unlimitedPolicy(),
		Resume:     &token,
	})
	if rewritten.Reconciliation != (Reconciliation{
		Change: FileChangeRewritten,
		Resume: ResumeReplay,
	}) {
		t.Fatalf("rewritten reconciliation = %#v", rewritten.Reconciliation)
	}
	closeGeneration(t, rewritten.Generation)
}

func TestJSONLGenerationEqualBytesKeepDistinctLocators(t *testing.T) {
	content := []byte("{\"id\":1}\n")
	first := writeFixture(t, "first.jsonl", content)
	second := writeFixture(t, "second.jsonl", content)

	firstResult := openGeneration(t, first, OpenOptions{
		TailMode:   TailModeFinal,
		SizePolicy: unlimitedPolicy(),
	})
	secondResult := openGeneration(t, second, OpenOptions{
		TailMode:   TailModeFinal,
		SizePolicy: unlimitedPolicy(),
	})
	firstRecords := readAll(t, firstResult.Generation)
	secondRecords := readAll(t, secondResult.Generation)

	if firstResult.Generation.Revision() != secondResult.Generation.Revision() {
		t.Fatal("equal bytes produced different revisions")
	}
	firstLocator := firstRecords[0].SourceLocator(first.Locator)
	secondLocator := secondRecords[0].SourceLocator(second.Locator)
	if firstLocator.File.Path == secondLocator.File.Path {
		t.Fatalf("duplicate locators merged: %#v %#v", firstLocator, secondLocator)
	}
	closeGeneration(t, firstResult.Generation)
	closeGeneration(t, secondResult.Generation)
}

func TestJSONLRecordPreservesLiteralLocatorAndNativeBytes(t *testing.T) {
	record := JSONLRecord{
		Record:    7,
		Line:      9,
		ByteRange: sessionio.ByteRange{Start: 12, End: 20},
		Data:      []byte("{\"x\":1}"),
		Framing:   []byte("\r\n"),
	}
	base := sessionio.FileLocator{
		Root: "literal/../root",
		Path: "./nested/../session.jsonl",
	}
	locator := record.SourceLocator(base)
	if locator.File == nil ||
		locator.File.Root != base.Root ||
		locator.File.Path != base.Path {
		t.Fatalf("literal locator changed: %#v", locator)
	}
	assertErrorLocator(t, locator, 7, 9, 12, 20)

	representation := record.NativeRepresentation()
	if representation.Capture != sessionio.CaptureKindByteExact ||
		representation.MediaType != "application/json" ||
		!bytes.Equal(representation.Data, record.Data) ||
		!bytes.Equal(representation.Framing, record.Framing) {
		t.Fatalf("native representation = %#v", representation)
	}
}

func TestJSONLGenerationReconstructionProperties(t *testing.T) {
	fixtures := [][]byte{
		[]byte("{\"a\":1}\n"),
		[]byte("{\"a\":1}\r\n{\"b\":2}\n"),
		[]byte("{\"a\":1}\n{\"b\":2}"),
		[]byte(" {\"a\":1} \n\t[1,2,3]\r\ntrue"),
		[]byte("true\r"),
	}

	for index, fixture := range fixtures {
		spec := writeFixture(t, "property-"+string(rune('a'+index))+".jsonl", fixture)
		result := openGeneration(t, spec, OpenOptions{
			TailMode:   TailModeFinal,
			SizePolicy: unlimitedPolicy(),
		})
		records := readAll(t, result.Generation)

		var reconstructed []byte
		var previousEnd int64
		for recordIndex, record := range records {
			if record.ByteRange.Start < previousEnd {
				t.Fatalf("fixture %d record %d overlaps previous range", index, recordIndex)
			}
			if record.ByteRange.End < record.ByteRange.Start {
				t.Fatalf("fixture %d record %d has reversed range", index, recordIndex)
			}
			if record.Record != uint64(recordIndex+1) ||
				record.Line != uint64(recordIndex+1) {
				t.Fatalf("fixture %d record %d numbering = %d/%d", index, recordIndex, record.Record, record.Line)
			}
			reconstructed = append(reconstructed, record.Data...)
			reconstructed = append(reconstructed, record.Framing...)
			previousEnd = record.ByteRange.End + int64(len(record.Framing))
		}
		if !bytes.Equal(reconstructed, fixture) {
			t.Fatalf("fixture %d reconstruction = %q, want %q", index, reconstructed, fixture)
		}
		closeGeneration(t, result.Generation)
	}
}

func TestJSONLGenerationContextAndClose(t *testing.T) {
	spec := writeFixture(t, "lifecycle.jsonl", []byte("{\"id\":1}\n"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := OpenJSONLGeneration(
		ctx,
		spec,
		OpenOptions{
			TailMode:   TailModeFinal,
			SizePolicy: unlimitedPolicy(),
		},
	)
	if result.Generation != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled open = %#v, %v", result, err)
	}

	opened := openGeneration(t, spec, OpenOptions{
		TailMode:   TailModeFinal,
		SizePolicy: unlimitedPolicy(),
	})
	if err := opened.Generation.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := opened.Generation.Close(); err != nil {
		t.Fatalf("repeated Close() error = %v", err)
	}
	if _, err := opened.Generation.Next(context.Background()); !errors.Is(err, sessionio.ErrStreamClosed) {
		t.Fatalf("Next() after Close error = %v", err)
	}
}

func TestJSONLGenerationRejectsInvalidOptions(t *testing.T) {
	spec := writeFixture(t, "invalid-options.jsonl", []byte("{\"id\":1}\n"))
	tests := []OpenOptions{
		{
			TailMode: TailModeFinal,
			SizePolicy: RecordSizePolicy{
				MaxBytes: 0,
			},
		},
		{
			TailMode: "unknown",
			SizePolicy: RecordSizePolicy{
				MaxBytes: UnlimitedRecordBytes,
			},
		},
	}
	for _, options := range tests {
		result, err := OpenJSONLGeneration(context.Background(), spec, options)
		if result.Generation != nil || err == nil {
			t.Fatalf("invalid options result = %#v, error = %v", result, err)
		}
	}

	invalidSpec := spec
	record := uint64(1)
	invalidSpec.Locator.Record = &record
	result, err := OpenJSONLGeneration(
		context.Background(),
		invalidSpec,
		OpenOptions{
			TailMode:   TailModeFinal,
			SizePolicy: unlimitedPolicy(),
		},
	)
	if result.Generation != nil || err == nil {
		t.Fatalf("invalid spec result = %#v, error = %v", result, err)
	}

	opened := openGeneration(t, spec, OpenOptions{
		TailMode:   TailModeFinal,
		SizePolicy: unlimitedPolicy(),
	})
	readAll(t, opened.Generation)
	token := opened.Generation.ResumeToken()
	closeGeneration(t, opened.Generation)
	token.NextRecord++
	result, err = OpenJSONLGeneration(
		context.Background(),
		spec,
		OpenOptions{
			TailMode:   TailModeFinal,
			SizePolicy: unlimitedPolicy(),
			Resume:     &token,
		},
	)
	if result.Generation != nil || err == nil {
		t.Fatalf("invalid resume numbering result = %#v, error = %v", result, err)
	}
}

func openGeneration(t *testing.T, spec FileSpec, options OpenOptions) OpenResult {
	t.Helper()
	result, err := OpenJSONLGeneration(context.Background(), spec, options)
	if err != nil {
		t.Fatalf("OpenJSONLGeneration() error = %v", err)
	}
	if result.Generation == nil {
		t.Fatal("OpenJSONLGeneration() returned nil generation")
	}
	return result
}

func readAll(t *testing.T, generation *JSONLGeneration) []JSONLRecord {
	t.Helper()
	var records []JSONLRecord
	for {
		record, err := generation.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return records
		}
		if err != nil {
			t.Fatalf("Next() error = %v", err)
		}
		records = append(records, record)
	}
}

func completedFixtureToken(t *testing.T, name string, content []byte) (FileSpec, ResumeToken) {
	t.Helper()
	spec := writeFixture(t, name, content)
	result := openGeneration(t, spec, OpenOptions{
		TailMode:   TailModeFinal,
		SizePolicy: unlimitedPolicy(),
	})
	readAll(t, result.Generation)
	token := result.Generation.ResumeToken()
	closeGeneration(t, result.Generation)
	return spec, token
}

func growingFixtureToken(
	t *testing.T,
	name string,
	prefix []byte,
	tail []byte,
) (FileSpec, ResumeToken) {
	t.Helper()
	content := append(append([]byte(nil), prefix...), tail...)
	spec := writeFixture(t, name, content)
	result := openGeneration(t, spec, OpenOptions{
		TailMode:   TailModeGrowing,
		SizePolicy: unlimitedPolicy(),
	})
	readAll(t, result.Generation)
	token := result.Generation.ResumeToken()
	closeGeneration(t, result.Generation)
	return spec, token
}

func writeFixture(t *testing.T, name string, content []byte) FileSpec {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, name)
	writeFixturePath(t, path, content)
	return FileSpec{
		OpenPath: path,
		Locator: sessionio.FileLocator{
			Root: "synthetic-root",
			Path: name,
		},
	}
}

func TestOpenJSONLGenerationObserveRecord(t *testing.T) {
	spec := writeFixture(t, "records.jsonl", []byte("{\"n\":1}\n{\"n\":2}"))
	var seen []string
	result, err := OpenJSONLGeneration(context.Background(), spec, OpenOptions{
		TailMode: TailModeFinal, SizePolicy: unlimitedPolicy(),
		ObserveRecord: func(record JSONLRecord) error {
			seen = append(seen, string(record.Data)+string(record.Framing))
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	closeGeneration(t, result.Generation)
	if !reflect.DeepEqual(seen, []string{"{\"n\":1}\n", "{\"n\":2}"}) {
		t.Fatalf("seen = %#v", seen)
	}

	_, err = OpenJSONLGeneration(context.Background(), spec, OpenOptions{TailMode: TailModeFinal, SizePolicy: unlimitedPolicy(), ObserveRecord: func(JSONLRecord) error { return errors.New("stop") }})
	if err == nil || !strings.Contains(err.Error(), "observe record") {
		t.Fatalf("observer error = %v", err)
	}

	pending := writeFixture(t, "pending.jsonl", []byte("{\"n\":1}\n{\"n\":"))
	seen = nil
	result, err = OpenJSONLGeneration(context.Background(), pending, OpenOptions{TailMode: TailModeGrowing, SizePolicy: unlimitedPolicy(), ObserveRecord: func(record JSONLRecord) error { seen = append(seen, string(record.Data)); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	closeGeneration(t, result.Generation)
	if !reflect.DeepEqual(seen, []string{"{\"n\":1}"}) {
		t.Fatalf("pending observer = %#v", seen)
	}
}

func writeFixturePath(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

func appendFixture(t *testing.T, path string, content []byte) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open fixture for append: %v", err)
	}
	if _, err := file.Write(content); err != nil {
		t.Fatalf("append fixture: %v", errors.Join(err, file.Close()))
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close appended fixture: %v", err)
	}
}

func closeGeneration(t *testing.T, generation *JSONLGeneration) {
	t.Helper()
	if err := generation.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func unlimitedPolicy() RecordSizePolicy {
	return RecordSizePolicy{MaxBytes: UnlimitedRecordBytes}
}

func assertRevision(t *testing.T, revision sessionio.Revision, content []byte) {
	t.Helper()
	sum := sha256.Sum256(content)
	expected := revisionFromDigest(sum)
	if revision != expected {
		t.Fatalf("revision = %#v, want %#v", revision, expected)
	}
}

func assertErrorLocator(
	t *testing.T,
	locator sessionio.SourceLocator,
	record uint64,
	line uint64,
	start int64,
	end int64,
) {
	t.Helper()
	if locator.Kind != sessionio.LocatorKindFile || locator.File == nil {
		t.Fatalf("locator = %#v, want file", locator)
	}
	if locator.File.Record == nil || *locator.File.Record != record {
		t.Fatalf("locator record = %#v, want %d", locator.File.Record, record)
	}
	if locator.File.Line == nil || *locator.File.Line != line {
		t.Fatalf("locator line = %#v, want %d", locator.File.Line, line)
	}
	expectedRange := sessionio.ByteRange{Start: start, End: end}
	if locator.File.ByteRange == nil || *locator.File.ByteRange != expectedRange {
		t.Fatalf("locator range = %#v, want %#v", locator.File.ByteRange, expectedRange)
	}
}

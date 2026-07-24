package sourceio

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	sessionio "github.com/nikitatsym/agent-session-io"
)

func TestDecodedJSONLGenerationFramingAndPhysicalRevision(t *testing.T) {
	decoded := []byte("{\"id\":1}\r\n{\"id\":2}")
	path := writeZstd(t, decoded)
	generation, err := OpenDecodedJSONLGeneration(context.Background(), decodedSpec(path), DecodedOpenOptions{SizePolicy: unlimitedPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	defer generation.Close()
	var rebuilt []byte
	for index := uint64(1); ; index++ {
		record, err := generation.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		locator := record.SourceLocator(decodedSpec(path).Locator)
		if record.Record != index || locator.File == nil || locator.File.ByteRange != nil {
			t.Fatalf("decoded record = %#v locator=%#v", record, locator)
		}
		rebuilt = append(rebuilt, record.Data...)
		rebuilt = append(rebuilt, record.Framing...)
	}
	if !bytes.Equal(rebuilt, decoded) {
		t.Fatalf("rebuilt = %q, want %q", rebuilt, decoded)
	}
	physical, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := revisionFromDigest(sha256.Sum256(physical))
	if generation.Revision() != want {
		t.Fatalf("revision = %#v, want %#v", generation.Revision(), want)
	}
}

func TestDecodedJSONLGenerationRejectsLimitsAndTrailingBytes(t *testing.T) {
	path := writeZstd(t, []byte(`{"padding":"`+string(bytes.Repeat([]byte("x"), 300))+`"}`+"\n"))
	_, err := OpenDecodedJSONLGeneration(context.Background(), decodedSpec(path), DecodedOpenOptions{SizePolicy: RecordSizePolicy{MaxBytes: 256}})
	var tooLarge *RecordTooLargeError
	if !errors.As(err, &tooLarge) || tooLarge.Locator.File == nil || tooLarge.Locator.File.ByteRange != nil {
		t.Fatalf("limit error = %T %v", err, err)
	}

	valid := writeZstd(t, []byte("{\"id\":1}\n"))
	file, err := os.OpenFile(valid, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("trailing")); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDecodedJSONLGeneration(context.Background(), decodedSpec(valid), DecodedOpenOptions{SizePolicy: unlimitedPolicy()}); err == nil {
		t.Fatal("trailing bytes accepted")
	}
}

func TestDecodedJSONLGenerationDetectsMutationBeforeReturn(t *testing.T) {
	path := writeZstd(t, []byte("{\"id\":1}\n"))
	generation, err := OpenDecodedJSONLGeneration(context.Background(), decodedSpec(path), DecodedOpenOptions{SizePolicy: unlimitedPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	defer generation.Close()
	var replacement bytes.Buffer
	encoder, err := zstd.NewWriter(&replacement)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encoder.Write([]byte("{\"id\":2}\n")); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, replacement.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = generation.Next(context.Background())
	var changed *ChangedSourceError
	if !errors.As(err, &changed) || changed.Locator.File == nil || changed.Locator.File.ByteRange != nil {
		t.Fatalf("mutation error = %T %v", err, err)
	}
}

func TestDecodedJSONLGenerationRejectsEqualDecodedPhysicalMutationAtEOF(t *testing.T) {
	decoded := []byte("{\"id\":1}\n{\"id\":2}\n")
	first := encodeZstd(t, decoded, zstd.WithEncoderPadding(256))
	second := encodeZstd(t, decoded, zstd.WithEncoderPadding(256))
	if len(first) != len(second) || bytes.Equal(first, second) {
		t.Fatalf("padded fixtures are not distinct equal-size streams: first=%d second=%d equal=%t", len(first), len(second), bytes.Equal(first, second))
	}
	path := filepath.Join(t.TempDir(), "rollout.jsonl.zst")
	if err := os.WriteFile(path, first, 0o600); err != nil {
		t.Fatal(err)
	}
	generation, err := OpenDecodedJSONLGeneration(context.Background(), decodedSpec(path), DecodedOpenOptions{SizePolicy: unlimitedPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	defer generation.Close()
	if err := os.WriteFile(path, second, 0o600); err != nil {
		t.Fatal(err)
	}
	for record := 1; record <= 2; record++ {
		value, err := generation.Next(context.Background())
		if err != nil || value.Record != uint64(record) {
			t.Fatalf("verified equal decoded record %d = %#v, %v", record, value, err)
		}
	}
	_, terminal := generation.Next(context.Background())
	var changed *ChangedSourceError
	if !errors.As(terminal, &changed) {
		t.Fatalf("terminal mutation error = %T %v", terminal, terminal)
	}
	if changed.ExpectedSHA256 != sha256.Sum256(first) || changed.ActualSHA256 != sha256.Sum256(second) {
		t.Fatalf("mutation digests = expected:%x actual:%x", changed.ExpectedSHA256, changed.ActualSHA256)
	}
	if _, repeated := generation.Next(context.Background()); repeated != terminal {
		t.Fatalf("repeated terminal error = %v, want same %v", repeated, terminal)
	}
}

func TestDecodedJSONLGenerationCancellation(t *testing.T) {
	path := writeZstd(t, []byte("{\"id\":1}\n"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := OpenDecodedJSONLGeneration(ctx, decodedSpec(path), DecodedOpenOptions{SizePolicy: unlimitedPolicy()}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled open error = %v", err)
	}
}

func decodedSpec(path string) DecodedFileSpec {
	return DecodedFileSpec{
		OpenPath: path,
		Locator:  sessionio.FileLocator{Root: filepath.Dir(path), Path: filepath.Base(path)},
		Codec:    "zstd",
		OpenDecoder: func(reader io.Reader) (io.ReadCloser, error) {
			decoder, err := zstd.NewReader(reader, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(128<<20))
			if err != nil {
				return nil, err
			}
			return decoder.IOReadCloser(), nil
		},
	}
}

func writeZstd(t *testing.T, decoded []byte) string {
	t.Helper()
	compressed := encodeZstd(t, decoded)
	path := filepath.Join(t.TempDir(), "rollout.jsonl.zst")
	if err := os.WriteFile(path, compressed, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func encodeZstd(t *testing.T, decoded []byte, options ...zstd.EOption) []byte {
	t.Helper()
	encoder, err := zstd.NewWriter(nil, options...)
	if err != nil {
		t.Fatal(err)
	}
	compressed := encoder.EncodeAll(decoded, nil)
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed
}

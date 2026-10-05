//go:build pgintegration

package catalog

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"
	"time"

	"crypto/sha256"
	"errors"
	"fmt"
	sessionio "github.com/nikitatsym/agent-session-io"
	"github.com/nikitatsym/agent-session-io/adapters/omp"
	"io"
	"os"
	"path/filepath"
)

func TestOMPExternalEvidenceSurvivesSourceDeletionAndStateTransfer(t *testing.T) {
	ctx := context.Background()
	dsn := testEndpoint(t, primaryEndpointEnv)
	origin := newTestCatalog(t, dsn)
	mustInit(t, origin)
	root := t.TempDir()
	payload := []byte{0, 1, 255, 10}
	hash := fmt.Sprintf("%x", sha256.Sum256(payload))
	if err := os.MkdirAll(filepath.Join(root, "sessions", "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "blobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(root, "sessions", "a", "session.jsonl")
	body := `{"type":"session","version":3,"id":"retained"}` + "\n" + fmt.Sprintf(`{"type":"message","id":"u","parentId":null,"message":{"role":"user","content":[{"type":"image","mimeType":"image/png","data":"blob:sha256:%s"}]}}`+"\n", hash)
	if err := os.WriteFile(transcript, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "blobs", hash), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	config := omp.DefaultConfig()
	config.AgentDir = root
	adapter, err := omp.New(config)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := adapter.Sessions(ctx, sessionio.SessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := refs.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := refs.Close(); err != nil {
		t.Fatal(err)
	}
	stream, err := adapter.Read(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	var items []sessionio.ReadItem
	for {
		item, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	external, err := EncodeExternalSnapshot(items, ref.Occurrence.Locator)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := CompressSnapshot([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	native, err := CompressSnapshot(external)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	locator := Locator{Kind: "file", Root: root, Path: "sessions/a/session.jsonl"}
	if err := origin.ObserveSource(ctx, RetainedSource{SourceID: string(ref.Occurrence.SourceID), Harness: "omp", Locator: Locator{Kind: "file", Root: root, Path: "."}}, now); err != nil {
		t.Fatal(err)
	}
	if err := origin.ObserveOccurrence(ctx, RetainedOccurrence{OccurrenceID: string(ref.Occurrence.ID), SourceID: string(ref.Occurrence.SourceID), Harness: "omp", Locator: locator}, now); err != nil {
		t.Fatal(err)
	}
	for _, blob := range []SnapshotBlob{canonical, native} {
		if _, err := origin.PutSnapshot(ctx, blob, now); err != nil {
			t.Fatal(err)
		}
	}
	revision := SessionRevision{SessionKey: string(ref.ID), OccurrenceID: string(ref.Occurrence.ID), Harness: "omp", NativeID: ref.NativeID, DiscoveryRevision: string(ref.DiscoveryRevision), SourceRevisionKind: "file_snapshot", SourceRevisionValue: "sha256:source", SnapshotHash: canonical.ContentHash, ExternalSnapshotHash: native.ContentHash, Locator: locator}
	revision.RevisionHash = RevisionHash(revision)
	if _, err := origin.PutSessionRevision(ctx, revision, now); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(transcript); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "blobs", hash)); err != nil {
		t.Fatal(err)
	}
	var state bytes.Buffer
	if _, err := origin.ExportState(ctx, &state, now); err != nil {
		t.Fatal(err)
	}
	target := newTestCatalog(t, dsn)
	mustInit(t, target)
	if _, err := target.ImportState(ctx, bytes.NewReader(state.Bytes())); err != nil {
		t.Fatal(err)
	}
	var retainedHash []byte
	pool, err := target.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT external_snapshot_hash FROM "+target.schema+".session_revision").Scan(&retainedHash); err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(retainedHash) != hex.EncodeToString(native.ContentHash) {
		t.Fatal("external snapshot identity lost")
	}
	data, found, err := target.LoadSnapshot(ctx, retainedHash)
	if err != nil || !found {
		t.Fatalf("load retained external bytes: %v %v", found, err)
	}
	restored, err := DecodeExternalSnapshot(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 1 || !bytes.Equal(restored[0].Observation.Representation.Data, payload) || restored[0].Observation.NativeKey != "blob:sha256:"+hash || restored[0].Observation.Locator.File.Path != "blobs/"+hash {
		t.Fatalf("retained blob/provenance lost: %+v", restored)
	}
}

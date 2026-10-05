package omp

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	sessionio "github.com/nikitatsym/agent-session-io"
)

type listingCache struct {
	refs   map[string]sessionio.SessionRef
	stamps map[string]string
	hits   int
}

func (cache *listingCache) ListingCache(string) (sessionio.ListingCache, bool) { return cache, true }
func (cache *listingCache) Lookup(key, stamp string) (sessionio.SessionRef, bool) {
	ref, found := cache.refs[key]
	if found && cache.stamps[key] == stamp {
		cache.hits++
		return ref, true
	}
	return sessionio.SessionRef{}, false
}
func (cache *listingCache) Retain(key, stamp string, ref sessionio.SessionRef) {
	cache.refs[key] = ref
	cache.stamps[key] = stamp
}

func TestListingCachePreservesDatesAndInvalidatesExternalDependencies(t *testing.T) {
	root := t.TempDir()
	payload := "native binary"
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(payload)))
	writeFixture(t, root, "blobs/"+hash, payload)
	writeFixture(t, root, "sessions/a/file.jsonl", `{"type":"session","version":3,"id":"cached","timestamp":"2026-10-01T00:00:00Z"}`+"\n"+fmt.Sprintf(`{"type":"message","id":"u","parentId":null,"timestamp":"2026-10-03T00:00:00Z","message":{"role":"user","timestamp":1790812800000,"content":[{"type":"image","data":"blob:sha256:%s"}]}}`+"\n", hash)+`{"type":"model_change","id":"model","parentId":"u","timestamp":"2026-10-03T23:00:00Z","model":"model"}`+"\n")
	cache := &listingCache{refs: map[string]sessionio.SessionRef{}, stamps: map[string]string{}}
	config := DefaultConfig()
	config.AgentDir = root
	config.Cache = cache
	adapter, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	cold := sessions(t, adapter)
	warm := sessions(t, adapter)
	if !reflect.DeepEqual(cold, warm) || cache.hits != 1 {
		t.Fatalf("cache changed metadata or missed: %d", cache.hits)
	}
	stream, err := adapter.Sessions(context.Background(), sessionio.SessionRequest{Sources: []sessionio.SourceID{cold[0].Occurrence.SourceID}})
	filtered := drain(t, stream, err)
	if !reflect.DeepEqual(cold, filtered) {
		t.Fatal("source filter changed dates")
	}
	if err := os.Remove(filepath.Join(root, "blobs", hash)); err != nil {
		t.Fatal(err)
	}
	missing := sessions(t, adapter)
	if missing[0].DiscoveryRevision == cold[0].DiscoveryRevision || missing[0].CreatedAt == nil || !missing[0].CreatedAt.Equal(*cold[0].CreatedAt) || !missing[0].LastMessageAt.Equal(*cold[0].LastMessageAt) {
		t.Fatal("external deletion did not invalidate cache independently of dates")
	}
	for _, item := range items(t, adapter, missing[0]) {
		if item.Observation.NativeKind == "blob" {
			t.Fatal("deleted blob fabricated")
		}
	}
}

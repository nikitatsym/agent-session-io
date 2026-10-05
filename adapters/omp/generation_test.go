package omp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAppendAndAtomicRewriteRefreshSelectedGeneration(t *testing.T) {
	root := t.TempDir()
	relative := "sessions/a/file.jsonl"
	header := `{"type":"session","version":3,"id":"generation","title":"first"}` + "\n"
	first := `{"type":"message","id":"one","parentId":null,"message":{"role":"user","timestamp":1790812800000,"content":"first"}}` + "\n"
	writeFixture(t, root, relative, header+first)
	adapter := fixtureAdapter(t, root)
	old := sessions(t, adapter)[0]
	oldItems := items(t, adapter, old)
	path := filepath.Join(root, filepath.FromSlash(relative))
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"type":"message","id":"two","parentId":"one","message":{"role":"assistant","timestamp":1790812801000,"content":"second"}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	grown := items(t, adapter, old)
	if len(grown) != 3 || grown[2].Observation.NativeKey != "two" || grown[0].Observation.Revision == oldItems[0].Observation.Revision || grown[0].Session.LastMessageAt.UnixMilli() != 1790812801000 {
		t.Fatal("append did not refresh generation and message date")
	}
	rewrite := `{"type":"session","version":3,"id":"generation","title":"rewritten"}` + "\n" + first
	if err := os.WriteFile(path+".replacement", []byte(rewrite), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".replacement", path); err != nil {
		t.Fatal(err)
	}
	replaced := items(t, adapter, old)
	if len(replaced) != 2 || replaced[0].Session.ID != old.ID || replaced[0].Session.Title != "rewritten" || replaced[0].Session.LastMessageAt.UnixMilli() != 1790812800000 || replaced[0].Observation.Revision == grown[0].Observation.Revision {
		t.Fatal("atomic rewrite reused stale generation")
	}
}

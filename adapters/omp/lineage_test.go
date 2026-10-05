package omp

import (
	"os"
	"path/filepath"
	"testing"

	sessionio "github.com/nikitatsym/agent-session-io"
)

func TestNestedControlParentAndSharedArtifacts(t *testing.T) {
	root := t.TempDir()
	parent := `{"type":"session","version":3,"id":"parent"}` + "\n"
	child := `{"type":"session","version":3,"id":"child","parentSession":"sessions/a/parent.jsonl"}` + "\n" + `{"type":"message","id":"u","parentId":null,"message":{"role":"user","content":"artifact://2 and agent://Peer"}}` + "\n"
	writeFixture(t, root, "sessions/a/parent.jsonl", parent)
	writeFixture(t, root, "sessions/a/parent/child.jsonl", child)
	writeFixture(t, root, "sessions/a/parent/2.bash.log", "shared full output")
	writeFixture(t, root, "sessions/a/parent/Peer.md", "agent output")
	writeFixture(t, root, "sessions/a/parent/Peer.json", `{"native":"output"}`)
	adapter := fixtureAdapter(t, root)
	refs := sessions(t, adapter)
	var childRef sessionio.SessionRef
	for _, ref := range refs {
		if ref.NativeID == "child" {
			childRef = ref
		}
	}
	if len(childRef.Native.Relationships) != 2 {
		t.Fatalf("child topology = %+v", childRef.Native)
	}
	for _, hint := range childRef.Native.Relationships {
		if hint.TargetNativeID != "parent" {
			t.Fatal("path lineage not resolved to observed identity")
		}
	}
	read := items(t, adapter, childRef)
	external := 0
	branch := 0
	for _, item := range read {
		if item.Observation.NativeKind == "artifact" || item.Observation.NativeKind == "agent_output" {
			external++
			if filepath.Dir(item.Observation.Locator.File.Path) != "sessions/a/parent" {
				t.Fatal("shared artifact provenance lost")
			}
		}
		for _, relation := range item.Relations {
			if relation.Kind == sessionio.RelationKindBranchParent {
				branch++
			}
		}
	}
	if external != 3 || branch != 1 {
		t.Fatalf("external=%d branch=%d", external, branch)
	}
	if err := os.WriteFile(filepath.Join(root, "sessions/a/parent/2.bash.log"), []byte("changed shared full output"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, ref := range sessions(t, adapter) {
		if ref.NativeID == "child" && ref.DiscoveryRevision == childRef.DiscoveryRevision {
			t.Fatal("shared artifact edit did not invalidate child freshness")
		}
	}
}

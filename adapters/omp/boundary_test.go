package omp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	sessionio "github.com/nikitatsym/agent-session-io"
)

func TestBlobIntegrityAndMalformedParentEvidence(t *testing.T) {
	t.Run("blob integrity", func(t *testing.T) {
		root := t.TempDir()
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte("expected")))
		writeFixture(t, root, "blobs/"+hash, "corrupt")
		writeFixture(t, root, "sessions/a/file.jsonl", `{"type":"session","version":3,"id":"integrity"}`+"\n"+fmt.Sprintf(`{"type":"message","id":"u","parentId":null,"message":{"role":"user","content":[{"type":"image","data":"blob:sha256:%s"}]}}`+"\n", hash))
		_, err := fixtureAdapter(t, root).Sessions(context.Background(), sessionio.SessionRequest{})
		if err == nil || !strings.Contains(err.Error(), "fails its SHA-256 content address") {
			t.Fatalf("corrupt content accepted: %v", err)
		}
	})
	t.Run("malformed parent", func(t *testing.T) {
		root := t.TempDir()
		body := `{"type":"session","version":3,"id":"parent"}` + "\n" + `{"type":"custom","id":"entry","parentId":7,"customType":"opaque"}` + "\n"
		writeFixture(t, root, "sessions/a/file.jsonl", body)
		adapter := fixtureAdapter(t, root)
		read := items(t, adapter, sessions(t, adapter)[0])
		item := read[1]
		if len(item.Diagnostics) != 1 || item.Diagnostics[0].Cause == nil || item.Diagnostics[0].Locator.File.Record == nil || *item.Diagnostics[0].Locator.File.Record != 2 {
			t.Fatal("malformed parent lost diagnostic/source context")
		}
		var cause *json.UnmarshalTypeError
		if !errors.As(item.Diagnostics[0].Cause, &cause) || !strings.Contains(item.Diagnostics[0].Message, cause.Error()) {
			t.Fatal("malformed parent diagnostic lost the original typed cause or its emitted text")
		}
		var output bytes.Buffer
		if err := sessionio.WriteJSON(&output, sessionio.Producer{Name: "fixture", Version: "test"}, []sessionio.Record{{Kind: sessionio.RecordKindReadItem, ReadItem: &item}}); err != nil {
			t.Fatal(err)
		}
		var document struct {
			Records []struct {
				ReadItem sessionio.ReadItem `json:"read_item"`
			} `json:"records"`
		}
		if err := json.Unmarshal(output.Bytes(), &document); err != nil {
			t.Fatal(err)
		}
		if len(document.Records) != 1 || len(document.Records[0].ReadItem.Diagnostics) != 1 || !strings.Contains(document.Records[0].ReadItem.Diagnostics[0].Message, cause.Error()) {
			t.Fatal("machine output dropped the malformed parent cause")
		}
		if string(item.Observation.Representation.Data) != strings.TrimSuffix(strings.Split(body, "\n")[1], "\n") {
			t.Fatal("malformed parent raw observation changed")
		}
		for _, relation := range item.Relations {
			if relation.Kind == sessionio.RelationKindReplyTo {
				t.Fatal("malformed parent invented a relation")
			}
		}
	})
}

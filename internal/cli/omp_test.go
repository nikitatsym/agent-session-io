package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOMPConfiguredReaderAndUnavailablePresence(t *testing.T) {
	root := t.TempDir()
	agent := filepath.Join(root, "omp")
	for _, bucket := range []string{"project-a", "project-b"} {
		path := filepath.Join(agent, "sessions", bucket, bucket+".jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf(`{"type":"session","version":3,"id":%q,"timestamp":"2026-10-01T10:00:00Z","title":%q}`+"\n", bucket, bucket) + `{"type":"message","id":"user","parentId":null,"timestamp":"2026-10-01T10:00:01Z","message":{"role":"user","timestamp":1790848801000,"content":"OMP end to end"}}` + "\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	config := filepath.Join(root, "config.toml")
	if err := os.WriteFile(config, []byte("schema = 'sessionio.config/v1'\n[sources.omp]\nagent_dir = 'omp'\n[cache]\ndir = 'cache'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, diagnostic, err := runRootCommand(config, "list", "--harness", "omp", "--format", "json")
	if err != nil {
		t.Fatalf("list: %v %s", err, diagnostic)
	}
	var document struct {
		Records []struct {
			Session struct {
				ID            string  `json:"id"`
				CreatedAt     *string `json:"created_at"`
				LastMessageAt *string `json:"last_message_at"`
			} `json:"session"`
		} `json:"records"`
	}
	if err := json.Unmarshal([]byte(output), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Records) != 2 || document.Records[0].Session.CreatedAt == nil || document.Records[0].Session.LastMessageAt == nil {
		t.Fatalf("all project dates missing: %s", output)
	}
	id := document.Records[0].Session.ID
	for _, command := range [][]string{{"show", id}, {"export", id, "--format", "json"}} {
		shown, diagnostic, err := runRootCommand(config, command...)
		if err != nil {
			t.Fatalf("%s: %v %s", command[0], err, diagnostic)
		}
		if !strings.Contains(shown, "OMP end to end") && command[0] == "show" {
			t.Fatal("show lost message")
		}
	}
	current, diagnostic, err := runRootCommand(config, "list", "--harness", "omp", "--current", "--format", "json")
	if err != nil {
		t.Fatalf("presence: %v %s", err, diagnostic)
	}
	var presence struct {
		Snapshot struct {
			Providers []struct {
				Harness string `json:"harness"`
				Support string `json:"support"`
			} `json:"providers"`
			Matches []any `json:"matches"`
		} `json:"snapshot"`
	}
	if err := json.Unmarshal([]byte(current), &presence); err != nil {
		t.Fatal(err)
	}
	if len(presence.Snapshot.Providers) != 1 || presence.Snapshot.Providers[0].Harness != "omp" || presence.Snapshot.Providers[0].Support != "unavailable" || len(presence.Snapshot.Matches) != 0 {
		t.Fatalf("dishonest OMP presence: %s", current)
	}
}

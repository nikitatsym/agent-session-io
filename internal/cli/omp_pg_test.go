//go:build pgintegration

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestOMPScanInvalidatesExternalEvidenceWithoutTranscriptChanges(t *testing.T) {
	fixture := newScanFixture(t)
	root := filepath.Join(filepath.Dir(fixture.configPath), "omp")
	configuration, err := os.ReadFile(fixture.configPath)
	if err != nil {
		t.Fatal(err)
	}
	configuration = append(configuration, []byte("\n[sources.omp]\nagent_dir='omp'\n")...)
	configuration = []byte(strings.Replace(string(configuration), "[sources.omp]\nagent_dir = 'empty-omp'\n\n", "", 1))
	if err := os.WriteFile(fixture.configPath, configuration, 0o600); err != nil {
		t.Fatal(err)
	}
	payload := []byte{1, 2, 3}
	hash := fmt.Sprintf("%x", sha256.Sum256(payload))
	fixture.write(filepath.Join(root, "sessions", "a", "session.jsonl"), `{"type":"session","version":3,"id":"omp-scan","timestamp":"2026-10-01T00:00:00Z"}`, fmt.Sprintf(`{"type":"message","id":"u","parentId":null,"timestamp":"2026-10-01T01:00:00Z","message":{"role":"user","timestamp":1790816400000,"content":[{"type":"text","text":"omp_search_boundary"},{"type":"image","mimeType":"image/png","data":"blob:sha256:%s"}]}}`, hash))
	fixture.writeBytes(filepath.Join(root, "blobs", hash), payload)
	for _, args := range [][]string{{"catalog", "init", "--format", "json"}, {"scan", "--format", "json"}} {
		_, diagnostic, err := runRootCommand(fixture.configPath, args...)
		if err != nil {
			t.Fatalf("%v: %v %s", args, err, diagnostic)
		}
	}
	warm, diagnostic, err := runRootCommand(fixture.configPath, "scan", "--format", "json")
	if err != nil {
		t.Fatalf("warm scan: %v %s", err, diagnostic)
	}
	var report struct {
		Retention retentionCounts `json:"retention"`
	}
	if err := json.Unmarshal([]byte(warm), &report); err != nil {
		t.Fatal(err)
	}
	if report.Retention.SessionsReused != 1 || report.Retention.SessionsRead != 0 {
		t.Fatalf("unchanged not reused: %s", warm)
	}
	if err := os.Remove(filepath.Join(root, "blobs", hash)); err != nil {
		t.Fatal(err)
	}
	changed, diagnostic, err := runRootCommand(fixture.configPath, "scan", "--format", "json")
	if err != nil {
		t.Fatalf("missing scan: %v %s", err, diagnostic)
	}
	if err := json.Unmarshal([]byte(changed), &report); err != nil {
		t.Fatal(err)
	}
	if report.Retention.SessionsRead != 1 || report.Retention.SessionsReused != 0 {
		t.Fatalf("external deletion reused stale revision: %s", changed)
	}
	connection, err := pgx.Connect(context.Background(), fixture.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	var total, available int
	if err := connection.QueryRow(context.Background(), "SELECT count(*),count(external_snapshot_hash) FROM "+pgx.Identifier{fixture.schema}.Sanitize()+".session_revision").Scan(&total, &available); err != nil {
		t.Fatal(err)
	}
	if total != 2 || available != 1 {
		t.Fatalf("missing payload overwrote retained evidence: total=%d available=%d", total, available)
	}
	if err := os.WriteFile(filepath.Join(root, "blobs", hash), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	_, diagnostic, err = runRootCommand(fixture.configPath, "scan", "--format", "json")
	if err != nil {
		t.Fatalf("returned blob scan: %v %s", err, diagnostic)
	}
	answer, diagnostic, err := runRootCommand(fixture.configPath, "search", "--mode", "literal", "omp_search_boundary", "--format", "json")
	if err != nil {
		t.Fatalf("OMP search: %v %s", err, diagnostic)
	}
	var result struct {
		Results []struct {
			Session struct {
				Harness string `json:"harness"`
			} `json:"session"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(answer), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || result.Results[0].Session.Harness != "omp" {
		t.Fatalf("wrong OMP search answer: %s", answer)
	}
}

//go:build pgintegration

package catalog

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"
)

// A stream can pass its own manifest checksum and still carry a hash that no
// column can hold. Validation must refuse it before the transaction opens.
func TestNonHexRetainedHashLeavesTheTargetUntouched(t *testing.T) {
	catalog := newTestCatalog(t, testEndpoint(t, primaryEndpointEnv))
	mustInit(t, catalog)
	stream := fixtureStream(t)
	stream.revisions[0].RevisionHash = notHex
	stream.checkpoints[0].RevisionHash = notHex
	_, err := catalog.ImportState(
		context.Background(),
		bytes.NewReader(renderStream(t, stream)),
	)
	requireStateCorrupt(t, err, "malformed revision hash")
	for _, table := range retainedTables {
		rows := queryInt(t, catalog, fmt.Sprintf(
			"SELECT count(*) FROM %s.%s",
			catalog.schema,
			quoteIdentifier(table),
		))
		if rows != 0 {
			t.Fatalf("the refused import wrote %d rows into %s", rows, table)
		}
	}
}

// The same stream with hex hashes must still import, so the new check rejects
// exactly the malformed field and nothing else.
func TestHexRetainedHashesStillImport(t *testing.T) {
	catalog := newTestCatalog(t, testEndpoint(t, primaryEndpointEnv))
	mustInit(t, catalog)
	stream := fixtureStream(t)
	summary, err := catalog.ImportState(
		context.Background(),
		bytes.NewReader(renderStream(t, stream)),
	)
	if err != nil {
		t.Fatalf("import state: %v", err)
	}
	if summary.Counts != stream.counts() {
		t.Fatalf("imported %+v, want %+v", summary.Counts, stream.counts())
	}
	rows := queryInt(t, catalog, fmt.Sprintf(
		"SELECT count(*) FROM %s.session_revision WHERE octet_length(revision_hash) = 32",
		catalog.schema,
	))
	if rows != 1 {
		t.Fatalf("full-length revision hashes = %d, want 1", rows)
	}
}

func TestSessionDatesRoundTripThroughCatalogState(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(fmt.Sprintf("known=%t", known), func(t *testing.T) {
			opened := newTestCatalog(t, testEndpoint(t, primaryEndpointEnv))
			mustInit(t, opened)
			stream := fixtureStream(t)
			created := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
			last := created.Add(3 * time.Hour)
			if known {
				stream.revisions[0].CreatedAt = &created
				stream.revisions[0].LastMessageAt = &last
			}
			if _, err := opened.ImportState(context.Background(), bytes.NewReader(renderStream(t, stream))); err != nil {
				t.Fatal(err)
			}
			var exported bytes.Buffer
			if _, err := opened.ExportState(context.Background(), &exported, created); err != nil {
				t.Fatal(err)
			}
			_, decoded, err := decodeState(bytes.NewReader(exported.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			revision := decoded.revisions[0]
			if known {
				if revision.CreatedAt == nil || !revision.CreatedAt.Equal(created) || revision.LastMessageAt == nil || !revision.LastMessageAt.Equal(last) {
					t.Fatalf("catalog dates = %#v", revision)
				}
			} else if revision.CreatedAt != nil || revision.LastMessageAt != nil {
				t.Fatalf("unknown catalog dates were substituted: %#v", revision)
			}
		})
	}
}

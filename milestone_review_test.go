package sessionio_test

import (
	"context"
	"io"
	"path/filepath"
	"reflect"
	"testing"

	sessionio "github.com/nikitatsym/agent-session-io"
	"github.com/nikitatsym/agent-session-io/adapters/claude"
	"github.com/nikitatsym/agent-session-io/adapters/codex"
)

func TestReaderFixtureEvidenceResolves(t *testing.T) {
	t.Run("codex rich session", func(t *testing.T) {
		root := fixtureRoot(t, "codex")
		adapter, err := codex.New(codex.Config{
			Home:           root,
			MaxRecordBytes: codex.DefaultMaxRecordBytes,
		})
		if err != nil {
			t.Fatal(err)
		}
		sessions := fixtureSessions(t, adapter)
		for _, session := range sessions {
			if hasNativeIdentity(session, "session-rich") {
				assertSessionEvidenceResolves(t, adapter, session)
				return
			}
		}
		t.Fatal("Codex rich fixture session was not discovered")
	})

	t.Run("all Claude sessions", func(t *testing.T) {
		root := fixtureRoot(t, "claude")
		adapter, err := claude.New(claude.Config{
			ConfigDir:      root,
			MaxRecordBytes: claude.DefaultMaxRecordBytes,
		})
		if err != nil {
			t.Fatal(err)
		}
		sessions := fixtureSessions(t, adapter)
		if len(sessions) == 0 {
			t.Fatal("Claude fixture sessions were not discovered")
		}
		for _, session := range sessions {
			assertSessionEvidenceResolves(t, adapter, session)
		}
	})
}

func fixtureSessions(
	t *testing.T,
	adapter sessionio.Adapter,
) []sessionio.SessionRef {
	t.Helper()
	return readStream(t, func() (sessionio.Stream[sessionio.SessionRef], error) {
		return adapter.Sessions(context.Background(), sessionio.SessionRequest{})
	})
}

func assertSessionEvidenceResolves(
	t *testing.T,
	adapter sessionio.Adapter,
	session sessionio.SessionRef,
) {
	t.Helper()
	items := readStream(t, func() (sessionio.Stream[sessionio.ReadItem], error) {
		return adapter.Read(context.Background(), session)
	})
	observations := make(map[sessionio.ObservationID]sessionio.SourceLocator, len(items))
	for _, item := range items {
		if _, exists := observations[item.Observation.ID]; exists {
			t.Fatalf("session %q repeats observation %q", session.ID, item.Observation.ID)
		}
		observations[item.Observation.ID] = item.Observation.Locator
	}
	for _, item := range items {
		for _, event := range item.Events {
			assertEvidenceReferences(t, session.ID, event.Evidence, observations)
		}
		for _, relation := range item.Relations {
			assertEvidenceReferences(t, session.ID, relation.Evidence, observations)
		}
	}
}

func assertEvidenceReferences(
	t *testing.T,
	sessionID sessionio.SessionID,
	evidence []sessionio.EvidenceRef,
	observations map[sessionio.ObservationID]sessionio.SourceLocator,
) {
	t.Helper()
	for _, reference := range evidence {
		locator, exists := observations[reference.Observation]
		if !exists {
			t.Fatalf(
				"session %q evidence references absent observation %q",
				sessionID,
				reference.Observation,
			)
		}
		if !reflect.DeepEqual(reference.Locator, locator) {
			t.Fatalf(
				"session %q evidence locator for %q does not match its observation",
				sessionID,
				reference.Observation,
			)
		}
	}
}

func fixtureRoot(t *testing.T, name string) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func hasNativeIdentity(session sessionio.SessionRef, value string) bool {
	for _, identity := range session.Native.Identities {
		if identity.Value == value {
			return true
		}
	}
	return false
}

func readStream[T any](
	t *testing.T,
	open func() (sessionio.Stream[T], error),
) []T {
	t.Helper()
	stream, err := open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := stream.Close(); err != nil {
			t.Error(err)
		}
	}()
	var values []T
	for {
		value, err := stream.Next(context.Background())
		if err == io.EOF {
			return values
		}
		if err != nil {
			t.Fatal(err)
		}
		values = append(values, value)
	}
}

package cli

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	sessionio "github.com/nikitatsym/agent-session-io"
)

func TestListPrintsShortIDsThatShowResolves(t *testing.T) {
	at := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	session := func(harness sessionio.Harness, id string, listed bool) sessionio.SessionRef {
		value := testSession(harness, id)
		value.Title = id
		if listed {
			value.LastMessageAt = &at
		}
		return value
	}
	codex := &fakeReaderAdapter{
		descriptor: testDescriptor(sessionio.HarnessCodex),
		sessions: []sessionio.SessionRef{
			session(sessionio.HarnessCodex, "session:sha256:0123456789abc111", true),
			// Excluded by the time filter, yet still a show candidate.
			session(sessionio.HarnessCodex, "session:sha256:0123456789abc222", false),
			session(sessionio.HarnessCodex, "session:sha256:ffffffffffffffff", true),
			session(sessionio.HarnessCodex, "session:sha256:aaaaaaaaaaaa1111", true),
			session(sessionio.HarnessCodex, "session:sha256:aaaaaaaaaaaa2222", false),
			session(sessionio.HarnessCodex, "session:sha256:bbbbbbbbbbbb1111", true),
			// An exact opaque ID wins resolution over any digest prefix.
			session(sessionio.HarnessCodex, "bbbbbbbbbbbb", true),
		},
	}

	root, output, _ := testReaderRoot(t, time.Now, codex)
	root.SetArgs([]string{
		"list",
		"--time-field", "last_message_at",
		"--since", at.Format(time.RFC3339),
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute list: %v", err)
	}
	printed := make(map[string]string)
	for _, row := range tableCells(t, output.String())[1:] {
		printed[row[4]] = row[3]
	}
	want := map[string]string{
		"session:sha256:0123456789abc111": "0123456789abc1",
		"session:sha256:ffffffffffffffff": "ffffffffffff",
		"session:sha256:aaaaaaaaaaaa1111": "aaaaaaaaaaaa1",
		"session:sha256:bbbbbbbbbbbb1111": "bbbbbbbbbbbb1",
		"bbbbbbbbbbbb":                    "bbbbbbbbbbbb",
	}
	if !reflect.DeepEqual(printed, want) {
		t.Fatalf("printed IDs = %q, want %q\n%s", printed, want, output.String())
	}
	for id, selector := range printed {
		showRoot, showOutput, _ := testReaderRoot(t, time.Now, codex)
		showRoot.SetArgs([]string{"show", selector})
		if err := showRoot.Execute(); err != nil {
			t.Fatalf("show printed ID %q: %v", selector, err)
		}
		if !strings.Contains(showOutput.String(), "session: "+id+"\n") {
			t.Fatalf("show %q resolved another session than %q:\n%s", selector, id, showOutput.String())
		}
	}
}

func TestIDAbbreviatorKeepsFullIDsWhenDigestsCollide(t *testing.T) {
	ids := newIDAbbreviator([]string{"session:sha256:abc", "other:sha256:abc", "session:sha256:abc"})
	if got := ids.abbreviate("other:sha256:abc"); got != "other:sha256:abc" {
		t.Fatalf("abbreviate = %q, want the full ID", got)
	}
}

func TestHumanDiagnosticsFoldRepeatedCodesButKeepErrors(t *testing.T) {
	repeated := sessionio.Diagnostic{
		Code:     "synthetic_repeated",
		Severity: sessionio.DiagnosticSeverityWarning,
		Message:  "repeated notice",
	}
	var sessions []sessionio.SessionRef
	for _, id := range []string{"session:sha256:aaaa", "session:sha256:bbbb", "session:sha256:cccc"} {
		session := testSession(sessionio.HarnessCodex, id)
		session.Diagnostics = []sessionio.Diagnostic{repeated}
		sessions = append(sessions, session)
	}
	single := sessionio.Diagnostic{
		Code:     "synthetic_single",
		Severity: sessionio.DiagnosticSeverityWarning,
		Message:  "single notice",
	}
	// Repeats inside one session count once and do not fold on their own.
	sessions[0].Diagnostics = append(sessions[0].Diagnostics, repeated, single, single)
	for _, index := range []int{1, 2} {
		sessions[index].Diagnostics = append(sessions[index].Diagnostics, sessionio.Diagnostic{
			Code:     "synthetic_failure",
			Severity: sessionio.DiagnosticSeverityError,
			Message:  "failure",
		})
	}
	adapter := &fakeReaderAdapter{
		descriptor: testDescriptor(sessionio.HarnessCodex),
		sessions:   sessions,
	}

	root, _, diagnostic := testReaderRoot(t, time.Now, adapter)
	root.SetArgs([]string{"list"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute list: %v", err)
	}
	want := []string{
		"session bbbb: error synthetic_failure: failure",
		"session cccc: error synthetic_failure: failure",
		"warning synthetic_repeated in 3 sessions (4 diagnostics): repeated notice",
		"session aaaa: warning synthetic_single: single notice",
		"session aaaa: warning synthetic_single: single notice",
		"repeated diagnostics are folded; --format json reports each one",
	}
	got := strings.Split(strings.TrimSpace(diagnostic.String()), "\n")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func TestShowFoldsAWarningRepeatedAcrossObservations(t *testing.T) {
	session := testSession(sessionio.HarnessCodex, "session-show-warnings")
	var items []sessionio.ReadItem
	for index := range 3 {
		item := testReadItem(session, nil, []byte(`{}`))
		item.Observation.ID = sessionio.ObservationID(fmt.Sprintf("observation-%d", index))
		item.Diagnostics = []sessionio.Diagnostic{{
			Code:     "synthetic_unknown_kind",
			Severity: sessionio.DiagnosticSeverityWarning,
			Message:  fmt.Sprintf("record kind %d has no projection", index),
		}}
		items = append(items, item)
	}
	adapter := &fakeReaderAdapter{
		descriptor:     testDescriptor(sessionio.HarnessCodex),
		sessions:       []sessionio.SessionRef{session},
		itemsBySession: map[sessionio.SessionID][]sessionio.ReadItem{session.ID: items},
	}

	root, _, diagnostic := testReaderRoot(t, time.Now, adapter)
	root.SetArgs([]string{"show", string(session.ID)})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute show: %v", err)
	}
	want := []string{
		"warning synthetic_unknown_kind in 3 observations",
		"repeated diagnostics are folded; --format json reports each one",
	}
	if got := strings.Split(strings.TrimSpace(diagnostic.String()), "\n"); !reflect.DeepEqual(got, want) {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func TestListTimeStyles(t *testing.T) {
	clock := testClock{value: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)}
	lastMessage := clock.value.Add(-2 * time.Hour)
	created := clock.value.Add(-3 * 24 * time.Hour)
	session := testSession(sessionio.HarnessCodex, "session-times")
	session.LastMessageAt = &lastMessage
	session.CreatedAt = &created
	adapter := &fakeReaderAdapter{
		descriptor: testDescriptor(sessionio.HarnessCodex),
		sessions:   []sessionio.SessionRef{session},
	}
	for _, testCase := range []struct {
		style string
		want  []string
	}{
		{"absolute", []string{lastMessage.Local().Format("2006-01-02 15:04"), created.Local().Format("2006-01-02 15:04")}},
		{"relative", []string{"2h ago", "3d ago"}},
	} {
		t.Run(testCase.style, func(t *testing.T) {
			root, output, _ := testReaderRoot(t, clock.Now, adapter)
			root.SetArgs([]string{"list", "--time-style", testCase.style})
			if err := root.Execute(); err != nil {
				t.Fatalf("execute list: %v", err)
			}
			rows := tableCells(t, output.String())
			if got := rows[1][:2]; !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("times = %q, want %q", got, testCase.want)
			}
		})
	}

	root, _, _ := testReaderRoot(t, clock.Now, adapter)
	root.SetArgs([]string{"list", "--time-style", "relative", "--format", "json"})
	if err := root.Execute(); ExitCode(err) != exitInvalid {
		t.Fatalf("machine --time-style error = %v, code=%d", err, ExitCode(err))
	}
}

func TestRelativeTimeBoundaries(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	const day = 24 * time.Hour
	for _, testCase := range []struct {
		age  time.Duration
		want string
	}{
		{59 * time.Second, "now"},
		{time.Minute, "1m ago"},
		{59 * time.Minute, "59m ago"},
		{time.Hour, "1h ago"},
		{13 * day, "13d ago"},
		{14 * day, "2w ago"},
		{59 * day, "8w ago"},
		{60 * day, "2mo ago"},
		{365 * day, "1y ago"},
		{-5 * time.Minute, "in 5m"},
	} {
		at := now.Add(-testCase.age)
		if got := formatHumanTime(&at, timeStyleRelative, now); got != testCase.want {
			t.Errorf("age %s = %q, want %q", testCase.age, got, testCase.want)
		}
	}
}

func TestTruncationCutsOnlyTheLastColumnToTheWidth(t *testing.T) {
	headers := []string{"HARNESS", "ID", "TITLE"}
	// HARNESS (7) and ID (12) each take their width plus a two-space gap: 23 cells.
	for _, testCase := range []struct {
		name  string
		title string
		width int
		want  int
	}{
		{"wide terminal", strings.Repeat("title ", 20), 50, 27},
		{"few cells left", strings.Repeat("title ", 20), 27, 4},
		{"wide characters", strings.Repeat("Обновление 会话 ", 10), 40, 17},
	} {
		rows := [][]string{{"codex", "0123456789ab", testCase.title}}
		truncateLastColumn(headers, rows, testCase.width)
		title := rows[0][2]
		if rows[0][1] != "0123456789ab" || lipgloss.Width(title) > testCase.want ||
			lipgloss.Width(title) < testCase.want-1 || !strings.HasSuffix(title, truncationTail) {
			t.Fatalf("%s: truncated row = %q (title width %d, want %d)", testCase.name, rows[0], lipgloss.Width(title), testCase.want)
		}
	}

	// Room for the tail alone cuts nothing; the row wraps instead.
	rows := [][]string{{"codex", "0123456789ab", strings.Repeat("title ", 20)}}
	truncateLastColumn(headers, rows, 26)
	if rows[0][2] != strings.Repeat("title ", 20) {
		t.Fatalf("tail-only room cut the title to %q", rows[0][2])
	}
}

func TestTableEscapesControlCharactersInsteadOfBreakingRows(t *testing.T) {
	var output bytes.Buffer
	rows := [][]string{{"claude", "/tmp/claude\ncontrol-root"}}
	if err := writeTable(&output, []string{"HARNESS", "LOCATION"}, rows); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(output.String(), "\n"), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[1], `"/tmp/claude\ncontrol-root"`) {
		t.Fatalf("table = %q", lines)
	}
}

func tableCells(t *testing.T, output string) [][]string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	header := []rune(lines[0])
	var starts []int
	for index, character := range header {
		if character != ' ' && (index == 0 || index >= 2 && header[index-1] == ' ' && header[index-2] == ' ') {
			starts = append(starts, index)
		}
	}
	cells := make([][]string, len(lines))
	for row, line := range lines {
		runes := []rune(line)
		for column, start := range starts {
			end := len(runes)
			if column+1 < len(starts) {
				end = min(starts[column+1], len(runes))
			}
			cell := ""
			if start < len(runes) {
				cell = strings.TrimSpace(string(runes[start:end]))
			}
			cells[row] = append(cells[row], cell)
		}
	}
	return cells
}

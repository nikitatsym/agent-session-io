package cli

import (
	"errors"
	"fmt"

	sessionio "github.com/nikitatsym/agent-session-io"
	"github.com/nikitatsym/agent-session-io/internal/catalog"
	"github.com/nikitatsym/agent-session-io/internal/readercache"
	"github.com/spf13/cobra"
)

// Why the gate exists: an answer must come from a catalog that presents the
// corpus as it is now. A stale or dirty catalog is caught up before the answer
// instead of being served with a warning.
const (
	refreshReasonStale       = "stale"
	refreshReasonUnreclaimed = "unreclaimed"
)

// searchRefresh records whether the freshness gate had to scan before the
// answer, and why.
type searchRefresh struct {
	Ran            bool   `json:"ran"`
	Reason         string `json:"reason,omitempty"`
	SessionsBehind int    `json:"sessions_behind"`
}

// ensureFresh answers only from a quiescent catalog. It reads stat identity and
// catalog rows, never a transcript: a catalog that is already current costs one
// warm listing and nothing else.
func ensureFresh(
	cmd *cobra.Command,
	opened *catalog.Catalog,
	registry *sessionio.Registry,
	cache *readercache.Store,
) (searchRefresh, error) {
	ctx := cmd.Context()
	if _, err := opened.Status(ctx); err != nil {
		return searchRefresh{}, err
	}
	active, present, err := opened.ActiveGeneration(ctx)
	if err != nil {
		return searchRefresh{}, err
	}
	// Quiescence comes before the missing-generation return: a search during
	// the explicit first scan reports the running scan, not a bare missing
	// generation.
	if err := opened.RequireQuiescentWriter(ctx); err != nil {
		return searchRefresh{}, err
	}
	if !present {
		// Nothing to catch up with: the retrieval leg reports the missing
		// generation with its own typed failure.
		return searchRefresh{}, nil
	}
	failures, err := opened.FailedSources(ctx, active)
	if err != nil {
		return searchRefresh{}, err
	}
	refresh, err := catalogRefresh(
		cmd,
		opened,
		registry,
		cache,
		active,
		failedIdentities(failures),
	)
	if err != nil || !refresh.Ran {
		return searchRefresh{}, err
	}
	if _, err := fmt.Fprintln(
		cmd.ErrOrStderr(),
		refreshMessage(refresh),
	); err != nil {
		return searchRefresh{}, fmt.Errorf("write the refresh diagnostic: %w", err)
	}
	record, err := runScan(cmd, opened, registry, cache, tolerateDeclared(failures))
	if err != nil {
		return searchRefresh{}, err
	}
	if err := reportRefreshed(cmd, record); err != nil {
		return searchRefresh{}, err
	}
	// A catch-up that stayed partial is not a failure of the search: it declares
	// exactly the sources the active generation already declared, and the answer
	// reports catalog_complete:false as that generation's answers already did.
	return refresh, reclaimOutcome(record)
}

// catalogRefresh decides whether this catalog can answer as it stands. Dirty
// beats stale: unreclaimed rows are swept by the same scan a stale catalog
// needs, so the listing is not paid for twice.
func catalogRefresh(
	cmd *cobra.Command,
	opened *catalog.Catalog,
	registry *sessionio.Registry,
	cache *readercache.Store,
	active catalog.GenerationID,
	failed failedSet,
) (searchRefresh, error) {
	ctx := cmd.Context()
	unreclaimed, err := opened.UnreclaimedGenerations(ctx)
	if err != nil {
		return searchRefresh{}, err
	}
	if unreclaimed > 0 {
		return searchRefresh{Ran: true, Reason: refreshReasonUnreclaimed}, nil
	}
	behind, err := sessionsBehind(cmd, opened, registry, cache, active, failed)
	if err != nil || behind == 0 {
		return searchRefresh{}, err
	}
	return searchRefresh{
		Ran:            true,
		Reason:         refreshReasonStale,
		SessionsBehind: behind,
	}, nil
}

// sessionsBehind counts the occurrences the active generation does not present
// at the revision the sources carry now. What the generation knowingly failed
// to read is not behind: rescanning it would fail the same way, and every
// answer from that generation already reports it as incomplete.
func sessionsBehind(
	cmd *cobra.Command,
	opened *catalog.Catalog,
	registry *sessionio.Registry,
	cache *readercache.Store,
	active catalog.GenerationID,
	failed failedSet,
) (int, error) {
	ctx := cmd.Context()
	presented, err := opened.PresentedRevisions(ctx, active)
	if err != nil {
		return 0, err
	}
	harnesses, err := selectHarnesses(registry, nil)
	if err != nil {
		return 0, err
	}
	listed, skipped, err := listForGate(cmd, registry, cache, harnesses, failed)
	if err != nil {
		return 0, err
	}
	revisions := make(map[string]string, len(presented))
	for _, revision := range presented {
		revisions[revision.OccurrenceID] = revision.DiscoveryRevision
	}
	behind := 0
	seen := make(map[string]bool, len(listed))
	for _, session := range listed {
		occurrence := string(session.Occurrence.ID)
		seen[occurrence] = true
		if failed.sources[string(session.Occurrence.SourceID)] {
			continue
		}
		if revisions[occurrence] != string(session.DiscoveryRevision) {
			behind++
		}
	}
	for _, revision := range presented {
		if !seen[revision.OccurrenceID] && skipped[revision.Harness] == nil {
			behind++
		}
	}
	return behind, nil
}

// failedSet is what one generation declares it could not read.
type failedSet struct {
	sources   map[string]bool
	harnesses map[string]bool
}

func failedIdentities(failures []catalog.SourceFailure) failedSet {
	failed := failedSet{
		sources:   make(map[string]bool, len(failures)),
		harnesses: make(map[string]bool, len(failures)),
	}
	for _, failure := range failures {
		failed.sources[failure.SourceID] = true
		failed.harnesses[failure.Harness] = true
	}
	return failed
}

// listForGate lists one harness at a time, because a harness the active
// generation already failed on may not be listable at all: a source whose first
// record is unreadable fails discovery, not only the read. Such a harness is
// skipped, and any other listing failure fails the command.
func listForGate(
	cmd *cobra.Command,
	registry *sessionio.Registry,
	cache *readercache.Store,
	harnesses []sessionio.Harness,
	failed failedSet,
) ([]sessionio.SessionRef, map[string]error, error) {
	var listed []sessionio.SessionRef
	// A skipped harness keeps its cause: it is what the diagnostic reports and
	// what tells the caller which presented sessions cannot be checked.
	skipped := map[string]error{}
	for _, harness := range harnesses {
		sessions, err := collectSessions(
			cmd.Context(),
			registry,
			[]sessionio.Harness{harness},
			false,
			cache,
		)
		if err != nil && !failed.harnesses[string(harness)] {
			return nil, nil, err
		}
		if err == nil {
			listed = append(listed, sessions...)
			continue
		}
		cause := fmt.Errorf("list harness %s: %w", harness, err)
		skipped[string(harness)] = cause
		if _, writeErr := fmt.Fprintf(
			cmd.ErrOrStderr(),
			"harness %s is missing from the active generation and still"+
				" cannot be listed: %v\n",
			harness,
			cause,
		); writeErr != nil {
			return nil, nil, errors.Join(cause, fmt.Errorf(
				"write the skipped harness diagnostic: %w",
				writeErr,
			))
		}
	}
	return listed, skipped, nil
}

func refreshMessage(refresh searchRefresh) string {
	if refresh.Reason == refreshReasonUnreclaimed {
		return "index holds unreclaimed rows of an interrupted scan, scanning"
	}
	if refresh.SessionsBehind == 1 {
		return "index is 1 session behind, scanning"
	}
	return fmt.Sprintf("index is %d sessions behind, scanning", refresh.SessionsBehind)
}

// reportRefreshed names the generation the gate published. The scan record
// itself belongs to the scan command, but a reclaim that failed after that
// publication has no other channel and is reported here too.
func reportRefreshed(cmd *cobra.Command, record scanRecord) error {
	if _, err := fmt.Fprintf(
		cmd.ErrOrStderr(),
		"index refreshed into generation %d (%s)\n",
		record.Generation,
		record.State,
	); err != nil {
		return fmt.Errorf("write the refresh result: %w", err)
	}
	return writeReclaimDiagnostic(cmd, record)
}

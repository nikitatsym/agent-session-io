package cli

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nikitatsym/agent-session-io/internal/catalog"
	"github.com/nikitatsym/agent-session-io/internal/passage"
	"github.com/spf13/cobra"
)

const searchSchema = "sessionio.search/v1"

// exitNoMatch is the catalog-command status for a valid search with no result.
const exitNoMatch = 1

// searchDisplayRunes bounds the human passage excerpt.
const searchDisplayRunes = 160

type searchRecord struct {
	Schema        string         `json:"schema"`
	Query         string         `json:"query"`
	Mode          string         `json:"mode"`
	Limit         int            `json:"limit"`
	CatalogSchema string         `json:"catalog_schema"`
	Generation    int64          `json:"catalog_generation"`
	State         string         `json:"catalog_generation_state"`
	Complete      bool           `json:"catalog_complete"`
	LiteralPath   string         `json:"literal_path,omitempty"`
	Matched       int            `json:"matched"`
	Results       []searchResult `json:"results"`
}

type searchResult struct {
	Rank       int           `json:"rank"`
	MatchedLeg string        `json:"matched_leg"`
	BM25Score  *float64      `json:"bm25_score"`
	Session    searchSession `json:"session"`
	Passage    searchPassage `json:"passage"`
	// Limitations is always present: an empty list states that this projection
	// is byte-exact, which an absent field could not.
	Limitations []catalog.ProjectionLimitation `json:"projection_limitations"`
	Evidence    []searchLocator                `json:"evidence"`
}

type searchSession struct {
	Ref               string `json:"session_ref"`
	Harness           string `json:"harness"`
	NativeID          string `json:"native_id"`
	Title             string `json:"title,omitempty"`
	SourceID          string `json:"source_id"`
	OccurrenceID      string `json:"occurrence_id"`
	DiscoveryRevision string `json:"discovery_revision"`
	Locator           string `json:"locator"`
	StartedAt         string `json:"started_at,omitempty"`
	UpdatedAt         string `json:"updated_at,omitempty"`
}

type searchPassage struct {
	ID                int64    `json:"id"`
	Ordinal           int      `json:"ordinal"`
	Kind              string   `json:"kind"`
	BuilderVersion    string   `json:"builder_version"`
	ProjectionKind    string   `json:"projection_kind"`
	ProjectionVersion string   `json:"projection_version"`
	OccurredAt        string   `json:"occurred_at,omitempty"`
	EventKeys         []string `json:"event_keys"`
	Text              string   `json:"text"`
}

type searchLocator struct {
	Observation string `json:"observation"`
	Locator     string `json:"locator"`
}

func newSearchCommand(configPath *string) *cobra.Command {
	var formatValue string
	var modeValue string
	var limit int
	cmd := &cobra.Command{
		Use:               "search QUERY",
		Short:             "Search the active catalog generation",
		Args:              invalidArgs(cobra.ExactArgs(1)),
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, args []string) error {
			mode, err := parseSearchMode(modeValue)
			if err != nil {
				return err
			}
			return withCatalog(
				cmd,
				*configPath,
				formatValue,
				"search",
				func(format outputFormat, opened *catalog.Catalog) error {
					result, err := opened.Search(
						cmd.Context(),
						catalog.SearchRequest{
							Query: args[0],
							Mode:  mode,
							Limit: limit,
						},
					)
					if err != nil {
						return typedFailure(cmd.OutOrStdout(), format, err)
					}
					return writeSearchRecord(cmd, format, searchRecordFrom(
						args[0],
						mode,
						limit,
						opened.SchemaName(),
						result,
					))
				},
			)
		},
	}
	addFormatFlag(
		cmd,
		&formatValue,
		string(formatHuman),
		"output format: human or json",
		"human",
		"json",
	)
	cmd.Flags().StringVar(
		&modeValue,
		"mode",
		string(catalog.SearchModeLexical),
		"retrieval mode: lexical or literal",
	)
	registerFixedFlagCompletion(cmd, "mode", "lexical", "literal")
	cmd.Flags().IntVar(
		&limit,
		"limit",
		catalog.DefaultSearchLimit,
		"maximum number of results",
	)
	return cmd
}

func parseSearchMode(value string) (catalog.SearchMode, error) {
	mode := catalog.SearchMode(value)
	switch mode {
	case catalog.SearchModeLexical, catalog.SearchModeLiteral:
		return mode, nil
	default:
		return "", invalidUsage(fmt.Errorf(
			"invalid mode %q (expected lexical or literal)",
			value,
		))
	}
}

func searchRecordFrom(
	query string,
	mode catalog.SearchMode,
	limit int,
	schemaName string,
	result catalog.SearchResult,
) searchRecord {
	record := searchRecord{
		Schema:        searchSchema,
		Query:         query,
		Mode:          string(mode),
		Limit:         limit,
		CatalogSchema: schemaName,
		Generation:    int64(result.Generation),
		State:         result.GenerationState,
		Complete:      result.Complete,
		LiteralPath:   result.LiteralPath,
		Matched:       len(result.Hits),
		Results:       make([]searchResult, 0, len(result.Hits)),
	}
	for _, hit := range result.Hits {
		record.Results = append(record.Results, searchResultFrom(hit))
	}
	return record
}

func searchResultFrom(hit catalog.SearchHit) searchResult {
	converted := searchResult{
		Rank:       hit.Rank,
		MatchedLeg: hit.MatchedLeg,
		BM25Score:  hit.BM25Score,
		Session: searchSession{
			Ref:               hit.SessionKey,
			Harness:           hit.Harness,
			NativeID:          hit.NativeID,
			Title:             hit.Title,
			SourceID:          hit.SourceID,
			OccurrenceID:      hit.OccurrenceID,
			DiscoveryRevision: hit.DiscoveryRevision,
			Locator:           formatCatalogLocator(hit.SessionLocator),
			StartedAt:         formatOptionalTime(hit.SessionStartedAt),
			UpdatedAt:         formatOptionalTime(hit.SessionUpdatedAt),
		},
		Passage: searchPassage{
			ID:                hit.PassageID,
			Ordinal:           hit.PassageOrdinal,
			Kind:              hit.PassageKind,
			BuilderVersion:    hit.PassageBuilderVersion,
			ProjectionKind:    hit.ProjectionKind,
			ProjectionVersion: hit.ProjectionVersion,
			OccurredAt:        formatOptionalTime(hit.PassageOccurredAt),
			EventKeys:         hit.EventKeys,
			Text:              hit.Body,
		},
		Limitations: hit.Limitations,
		Evidence:    make([]searchLocator, 0, len(hit.Evidence)),
	}
	if converted.Passage.EventKeys == nil {
		converted.Passage.EventKeys = []string{}
	}
	if converted.Limitations == nil {
		converted.Limitations = []catalog.ProjectionLimitation{}
	}
	for _, evidence := range hit.Evidence {
		converted.Evidence = append(converted.Evidence, searchLocator{
			Observation: evidence.Observation,
			Locator:     formatCatalogLocator(evidence.Locator),
		})
	}
	return converted
}

func writeSearchRecord(
	cmd *cobra.Command,
	format outputFormat,
	record searchRecord,
) error {
	if format == formatJSON {
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(record); err != nil {
			return fmt.Errorf("write search record: %w", err)
		}
	} else if err := writeSearchHuman(cmd, record); err != nil {
		return err
	}
	if record.Matched > 0 {
		return nil
	}
	return &commandError{
		code:     exitNoMatch,
		err:      fmt.Errorf("no passage matched %q", record.Query),
		reported: true,
	}
}

func writeSearchHuman(cmd *cobra.Command, record searchRecord) error {
	header := fmt.Sprintf(
		"generation %d %s complete=%t mode=%s matched=%d",
		record.Generation,
		record.State,
		record.Complete,
		record.Mode,
		record.Matched,
	)
	if record.LiteralPath != "" {
		header += " literal_path=" + record.LiteralPath
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), header); err != nil {
		return fmt.Errorf("write search header: %w", err)
	}
	for _, result := range record.Results {
		if _, err := fmt.Fprintf(
			cmd.OutOrStdout(),
			"%d. [%s]%s %s %s passage %d %s\n   %s\n",
			result.Rank,
			result.MatchedLeg,
			formatScore(result.BM25Score),
			result.Session.Harness,
			result.Session.Ref,
			result.Passage.Ordinal,
			result.Passage.Kind,
			passage.Display(result.Passage.Text, searchDisplayRunes),
		); err != nil {
			return fmt.Errorf("write search result: %w", err)
		}
		for _, limitation := range result.Limitations {
			if _, err := fmt.Fprintf(
				cmd.OutOrStdout(),
				"   projection limitation %s: %d bytes removed\n",
				limitation.Kind,
				limitation.RemovedBytes,
			); err != nil {
				return fmt.Errorf("write projection limitation: %w", err)
			}
		}
		for _, evidence := range result.Evidence {
			if _, err := fmt.Fprintf(
				cmd.OutOrStdout(),
				"   evidence observation=%s locator=%s\n",
				evidence.Observation,
				evidence.Locator,
			); err != nil {
				return fmt.Errorf("write search evidence: %w", err)
			}
		}
	}
	return nil
}

func formatScore(score *float64) string {
	if score == nil {
		return ""
	}
	return " score=" + strconv.FormatFloat(*score, 'g', 6, 64)
}

func formatOptionalTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

// formatCatalogLocator mirrors the reader locator rendering so a search hit and
// show --detail provenance name the same evidence in the same way.
func formatCatalogLocator(locator catalog.Locator) string {
	if locator.Kind == "" {
		return ""
	}
	parts := []string{fmt.Sprintf(
		"%s:root=%q path=%q",
		locator.Kind,
		locator.Root,
		locator.Path,
	)}
	if locator.Record != nil {
		parts = append(parts, "record="+strconv.FormatInt(*locator.Record, 10))
	}
	if locator.Line != nil {
		parts = append(parts, "line="+strconv.FormatInt(*locator.Line, 10))
	}
	if locator.ByteStart != nil && locator.ByteEnd != nil {
		parts = append(parts, fmt.Sprintf(
			"bytes=%d-%d",
			*locator.ByteStart,
			*locator.ByteEnd,
		))
	}
	return strings.Join(parts, " ")
}

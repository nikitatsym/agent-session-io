package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	sessionio "github.com/nikitatsym/agent-session-io"
	"github.com/nikitatsym/agent-session-io/internal/catalog"
	"github.com/nikitatsym/agent-session-io/internal/passage"
	"github.com/spf13/cobra"
)

const scanSchema = "sessionio.scan/v1"

type scanRecord struct {
	Schema          string             `json:"schema"`
	CatalogSchema   string             `json:"catalog_schema"`
	Generation      int64              `json:"catalog_generation"`
	State           string             `json:"catalog_generation_state"`
	Sources         []string           `json:"sources"`
	Counts          catalog.ScanCounts `json:"counts"`
	BuilderVersions map[string]string  `json:"builder_versions"`
	Reclaimed       int                `json:"reclaimed_generations"`
	Retained        int                `json:"retained_generations"`
}

func newScanCommand(
	configPath *string,
	newRegistry registryFactory,
) *cobra.Command {
	var formatValue string
	cmd := &cobra.Command{
		Use:               "scan",
		Short:             "Reconcile sessions into a new catalog generation",
		Args:              invalidArgs(cobra.NoArgs),
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withCatalog(
				cmd,
				*configPath,
				formatValue,
				"scan",
				func(format outputFormat, opened *catalog.Catalog) error {
					record, err := runScan(cmd, opened, newRegistry)
					if err != nil {
						return typedFailure(cmd.OutOrStdout(), format, err)
					}
					return writeScanRecord(cmd, format, record)
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
	return cmd
}

func runScan(
	cmd *cobra.Command,
	opened *catalog.Catalog,
	newRegistry registryFactory,
) (scanRecord, error) {
	ctx := cmd.Context()
	if _, err := opened.Status(ctx); err != nil {
		return scanRecord{}, err
	}
	registry, err := openRegistry(newRegistry)
	if err != nil {
		return scanRecord{}, err
	}
	harnesses, err := selectHarnesses(registry, nil)
	if err != nil {
		return scanRecord{}, err
	}
	sources, err := collectSources(ctx, registry, harnesses)
	if err != nil {
		return scanRecord{}, err
	}
	active, present, err := opened.ActiveGeneration(ctx)
	if err != nil {
		return scanRecord{}, err
	}
	var parent *catalog.GenerationID
	if present {
		parent = &active
	}
	generation, err := opened.BeginCandidate(ctx, parent)
	if err != nil {
		return scanRecord{}, err
	}
	record, err := fillGeneration(
		ctx,
		opened,
		registry,
		harnesses,
		sources,
		generation,
	)
	if err != nil {
		// The candidate never becomes visible; marking it failed makes it
		// reclaimable by the next successful scan.
		return scanRecord{}, errors.Join(err, opened.MarkFailed(ctx, generation))
	}
	record.CatalogSchema = opened.SchemaName()
	record.Reclaimed, record.Retained, err = opened.Reclaim(ctx)
	if err != nil {
		return scanRecord{}, err
	}
	return record, nil
}

func fillGeneration(
	ctx context.Context,
	opened *catalog.Catalog,
	registry *sessionio.Registry,
	harnesses []sessionio.Harness,
	sources []sessionio.Source,
	generation catalog.GenerationID,
) (scanRecord, error) {
	writer := opened.NewGenerationWriter(generation)
	sessions, err := collectSessions(ctx, registry, harnesses, false)
	if err != nil {
		return scanRecord{}, err
	}
	for _, session := range sessions {
		adapter, found := registry.Adapter(session.Occurrence.Harness)
		if !found {
			return scanRecord{}, fmt.Errorf(
				"registered adapter %q disappeared",
				session.Occurrence.Harness,
			)
		}
		items, err := readItems(ctx, adapter, session)
		if err != nil {
			return scanRecord{}, err
		}
		retained, err := scanSession(session, passage.Build(items))
		if err != nil {
			return scanRecord{}, err
		}
		if err := writer.WriteSession(ctx, retained); err != nil {
			return scanRecord{}, err
		}
	}
	facts := catalog.BuildFacts{
		Sources:         sourceIdentifiers(sources),
		BuilderVersions: builderVersions(),
		Counts:          writer.Counts(),
	}
	if err := opened.RecordBuild(ctx, generation, facts); err != nil {
		return scanRecord{}, err
	}
	if err := opened.BuildIndexes(ctx, generation); err != nil {
		return scanRecord{}, err
	}
	if err := opened.Publish(ctx, generation); err != nil {
		return scanRecord{}, err
	}
	return scanRecord{
		Schema:          scanSchema,
		Generation:      int64(generation),
		State:           catalog.StateComplete,
		Sources:         facts.Sources,
		Counts:          facts.Counts,
		BuilderVersions: facts.BuilderVersions,
	}, nil
}

func builderVersions() map[string]string {
	return map[string]string{
		"passage":    passage.BuilderVersion,
		"projection": passage.ProjectionVersion,
	}
}

func sourceIdentifiers(sources []sessionio.Source) []string {
	identifiers := make([]string, 0, len(sources))
	for _, source := range sources {
		identifiers = append(identifiers, string(source.ID))
	}
	return identifiers
}

func scanSession(
	session sessionio.SessionRef,
	built passage.Session,
) (catalog.ScanSession, error) {
	sessionLocator, err := scanLocator(session.Occurrence.Locator)
	if err != nil {
		return catalog.ScanSession{}, err
	}
	scan := catalog.ScanSession{
		Key:               string(session.ID),
		Harness:           string(session.Occurrence.Harness),
		NativeID:          session.NativeID,
		Title:             session.Title,
		SourceID:          string(session.Occurrence.SourceID),
		OccurrenceID:      string(session.Occurrence.ID),
		DiscoveryRevision: string(session.DiscoveryRevision),
		Locator:           sessionLocator,
		StartedAt:         session.StartedAt,
		UpdatedAt:         session.UpdatedAt,
	}
	for _, event := range built.Events {
		scanned := catalog.ScanEvent{
			Key:         event.Key,
			Kind:        event.Kind,
			Role:        event.Role,
			Observation: event.Observation,
			OccurredAt:  event.OccurredAt,
		}
		for _, evidence := range event.Evidence {
			locator, err := scanLocator(evidence.Locator)
			if err != nil {
				return catalog.ScanSession{}, err
			}
			scanned.Evidence = append(scanned.Evidence, catalog.ScanEvidence{
				Observation: evidence.Observation,
				Locator:     locator,
			})
		}
		scan.Events = append(scan.Events, scanned)
	}
	for _, built := range built.Passages {
		limitations := make([]catalog.ProjectionLimitation, 0, len(built.Limitations))
		for _, limitation := range built.Limitations {
			limitations = append(limitations, catalog.ProjectionLimitation{
				Kind:         limitation.Kind,
				RemovedBytes: limitation.RemovedBytes,
			})
		}
		scan.Passages = append(scan.Passages, catalog.ScanPassage{
			Kind:              string(built.Kind),
			BuilderVersion:    passage.BuilderVersion,
			ProjectionKind:    catalog.ProjectionKindLexical,
			ProjectionVersion: passage.ProjectionVersion,
			Events:            built.Events,
			Body:              built.Body,
			ContentHash:       built.ContentHash,
			OccurredAt:        built.OccurredAt,
			Limitations:       limitations,
			Facets: []catalog.FacetFilter{{
				Namespace: "source",
				Key:       "harness",
				Value:     string(session.Occurrence.Harness),
			}},
		})
	}
	return scan, nil
}

// scanLocator keeps every locator variant addressable through the same
// retained columns; a database or opaque locator stores its table or scheme in
// the root column.
func scanLocator(
	locator sessionio.SourceLocator,
) (catalog.Locator, error) {
	switch {
	case locator.File != nil:
		built := catalog.Locator{
			Kind: string(locator.Kind),
			Root: locator.File.Root,
			Path: locator.File.Path,
		}
		record, err := ordinal(locator.File.Record, "record")
		if err != nil {
			return catalog.Locator{}, err
		}
		built.Record = record
		line, err := ordinal(locator.File.Line, "line")
		if err != nil {
			return catalog.Locator{}, err
		}
		built.Line = line
		if span := locator.File.ByteRange; span != nil {
			start, end := span.Start, span.End
			built.ByteStart = &start
			built.ByteEnd = &end
		}
		return built, nil
	case locator.Database != nil:
		return catalog.Locator{
			Kind: string(locator.Kind),
			Root: locator.Database.Table,
			Path: locator.Database.Path,
		}, nil
	case locator.Opaque != nil:
		return catalog.Locator{
			Kind: string(locator.Kind),
			Root: locator.Opaque.Scheme,
			Path: locator.Opaque.Value,
		}, nil
	default:
		return catalog.Locator{Kind: string(locator.Kind)}, nil
	}
}

func ordinal(value *uint64, name string) (*int64, error) {
	if value == nil {
		return nil, nil
	}
	if *value > math.MaxInt64 {
		return nil, fmt.Errorf(
			"evidence locator %s %d does not fit a catalog bigint",
			name,
			*value,
		)
	}
	converted := int64(*value)
	return &converted, nil
}

func writeScanRecord(
	cmd *cobra.Command,
	format outputFormat,
	record scanRecord,
) error {
	if format == formatJSON {
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(record); err != nil {
			return fmt.Errorf("write scan record: %w", err)
		}
		return nil
	}
	if _, err := fmt.Fprintf(
		cmd.OutOrStdout(),
		"catalog schema %q published generation %d (%s)\n"+
			"sources: %d\n"+
			"sessions: %d\n"+
			"events: %d\n"+
			"evidence: %d\n"+
			"passages: %d\n"+
			"projections: %d (%d with a limitation)\n"+
			"reclaimed generations: %d (retained %d)\n",
		record.CatalogSchema,
		record.Generation,
		record.State,
		len(record.Sources),
		record.Counts.Sessions,
		record.Counts.Events,
		record.Counts.Evidence,
		record.Counts.Passages,
		record.Counts.Projections,
		record.Counts.Limitations,
		record.Reclaimed,
		record.Retained,
	); err != nil {
		return fmt.Errorf("write scan result: %w", err)
	}
	return nil
}

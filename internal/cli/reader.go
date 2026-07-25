package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	sessionio "github.com/nikitatsym/agent-session-io"
	"github.com/nikitatsym/agent-session-io/internal/buildinfo"
	runtimepresence "github.com/nikitatsym/agent-session-io/internal/presence"
	"github.com/spf13/cobra"
)

const (
	exitRuntime  = 1
	exitInvalid  = 2
	exitNotFound = 3
)

type commandError struct {
	code int
	err  error
	// reported marks failures already written as a machine error record.
	reported bool
}

func (commandError *commandError) Error() string {
	if commandError == nil || commandError.err == nil {
		return "sessionio: command failed"
	}
	return commandError.err.Error()
}

func (commandError *commandError) Unwrap() error {
	if commandError == nil {
		return nil
	}
	return commandError.err
}

// ExitCode maps command failures to the stable sessionio CLI exit contract.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var commandError *commandError
	if errors.As(err, &commandError) {
		return commandError.code
	}
	return exitRuntime
}

func invalidUsage(err error) error {
	if err == nil {
		err = errors.New("invalid command usage")
	}
	return &commandError{code: exitInvalid, err: err}
}

func invalidArgs(validate cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := validate(cmd, args); err != nil {
			return invalidUsage(err)
		}
		return nil
	}
}

func missingSession(id string) error {
	return &commandError{
		code: exitNotFound,
		err:  fmt.Errorf("session %q was not found", id),
	}
}

type outputFormat string

const (
	formatHuman  outputFormat = "human"
	formatJSON   outputFormat = "json"
	formatNDJSON outputFormat = "ndjson"
)

func parseOutputFormat(value string, allowed ...outputFormat) (outputFormat, error) {
	format := outputFormat(value)
	for _, candidate := range allowed {
		if format == candidate {
			return format, nil
		}
	}
	values := make([]string, len(allowed))
	for index, candidate := range allowed {
		values[index] = string(candidate)
	}
	return "", invalidUsage(fmt.Errorf(
		"invalid format %q (expected %s)",
		value,
		strings.Join(values, ", "),
	))
}

type registryFactory func() (*sessionio.Registry, error)
type presenceProviderFactory func(
	[]sessionio.Harness,
) ([]runtimepresence.Provider, error)

func openRegistry(factory registryFactory) (*sessionio.Registry, error) {
	if factory == nil {
		return nil, errors.New("configure reader: registry factory is unavailable")
	}
	registry, err := factory()
	if err != nil {
		return nil, fmt.Errorf("configure reader: %w", err)
	}
	if registry == nil {
		return nil, errors.New("configure reader: registry factory returned nil")
	}
	return registry, nil
}

func newReaderCommand(
	use string,
	short string,
	args cobra.PositionalArgs,
	run func(*cobra.Command, []string) error,
) *cobra.Command {
	return &cobra.Command{
		Use:               use,
		Short:             short,
		Args:              invalidArgs(args),
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE:              run,
	}
}

func newSourcesCommand(
	info buildinfo.Info,
	newRegistry registryFactory,
) *cobra.Command {
	var harnesses []string
	var formatValue string
	cmd := newReaderCommand(
		"sources",
		"List discovered coding-agent sources",
		cobra.NoArgs,
		func(cmd *cobra.Command, _ []string) error {
			format, err := parseOutputFormat(
				formatValue,
				formatHuman,
				formatJSON,
				formatNDJSON,
			)
			if err != nil {
				return err
			}
			registry, err := openRegistry(newRegistry)
			if err != nil {
				return err
			}
			selected, err := selectHarnesses(registry, harnesses)
			if err != nil {
				return err
			}
			sources, err := collectSources(cmd.Context(), registry, selected)
			if err != nil {
				return err
			}
			if format == formatHuman {
				if err := writeSourcesHuman(cmd.OutOrStdout(), sources); err != nil {
					return err
				}
				return writeSourceDiagnostics(cmd.ErrOrStderr(), sources)
			}
			return writeRecords(
				cmd.OutOrStdout(),
				producer(info),
				format,
				mapRecords(sources, sourceRecord),
			)
		},
	)
	addHarnessFlag(cmd, &harnesses)
	addFormatFlag(
		cmd,
		&formatValue,
		string(formatHuman),
		"output format: human, json, or ndjson",
		"human",
		"json",
		"ndjson",
	)
	return cmd
}

func newListCommand(
	info buildinfo.Info,
	newRegistry registryFactory,
	newPresenceProviders presenceProviderFactory,
	now func() time.Time,
) *cobra.Command {
	var harnesses []string
	var sinceValue string
	var untilValue string
	var formatValue string
	var currentValue string
	cmd := newReaderCommand(
		"list",
		"List coding-agent sessions",
		cobra.NoArgs,
		func(cmd *cobra.Command, _ []string) error {
			format, err := parseOutputFormat(
				formatValue,
				formatHuman,
				formatJSON,
				formatNDJSON,
			)
			if err != nil {
				return err
			}
			currentMode, current, err := parseCurrentMode(currentValue)
			if err != nil {
				return err
			}
			if current && (sinceValue != "" || untilValue != "") {
				return invalidUsage(errors.New(
					"--current cannot be combined with --since or --until",
				))
			}
			filter, err := parseTimeFilter(sinceValue, untilValue, now)
			if err != nil {
				return err
			}
			registry, err := openRegistry(newRegistry)
			if err != nil {
				return err
			}
			selected, err := selectHarnesses(registry, harnesses)
			if err != nil {
				return err
			}
			if current {
				sessions, err := collectSessions(
					cmd.Context(),
					registry,
					selected,
					false,
				)
				if err != nil {
					return err
				}
				providers, err := openPresenceProviders(
					newPresenceProviders,
					selected,
				)
				if err != nil {
					return err
				}
				if now == nil {
					return errors.New("configure runtime presence: clock is unavailable")
				}
				snapshot, err := runtimepresence.Observe(
					cmd.Context(),
					runtimepresence.Request{
						ObservedAt: now(),
						Mode:       currentMode,
						Sessions:   sessions,
						Providers:  providers,
					},
				)
				if err != nil {
					return err
				}
				if err := writePresence(
					cmd.OutOrStdout(),
					producer(info),
					format,
					snapshot,
				); err != nil {
					return err
				}
				if format == formatHuman {
					if err := writePresenceDiagnostics(
						cmd.ErrOrStderr(),
						snapshot,
					); err != nil {
						return err
					}
					return writeSessionDiagnostics(
						cmd.ErrOrStderr(),
						sessions,
					)
				}
				return nil
			}
			sessions, err := collectSessions(
				cmd.Context(),
				registry,
				selected,
				filter.active(),
			)
			if err != nil {
				return err
			}
			sessions = filter.apply(sessions)
			sortSessions(sessions)
			if format == formatHuman {
				if err := writeSessionsHuman(cmd.OutOrStdout(), sessions); err != nil {
					return err
				}
				return writeSessionDiagnostics(cmd.ErrOrStderr(), sessions)
			}
			return writeRecords(
				cmd.OutOrStdout(),
				producer(info),
				format,
				mapRecords(sessions, sessionRecord),
			)
		},
	)
	addHarnessFlag(cmd, &harnesses)
	cmd.Flags().StringVar(
		&sinceValue,
		"since",
		"",
		"include activity at or after RFC3339 time or age such as 7d",
	)
	cmd.Flags().StringVar(
		&untilValue,
		"until",
		"",
		"include activity at or before RFC3339 time or age such as 1h",
	)
	cmd.Flags().StringVar(
		&currentValue,
		"current",
		"",
		"list currently running sessions: all or exact",
	)
	cmd.Flags().Lookup("current").NoOptDefVal = string(runtimepresence.ModeAll)
	registerFixedFlagCompletion(
		cmd,
		"current",
		string(runtimepresence.ModeAll),
		string(runtimepresence.ModeExact),
	)
	addFormatFlag(
		cmd,
		&formatValue,
		string(formatHuman),
		"output format: human, json, or ndjson",
		"human",
		"json",
		"ndjson",
	)
	return cmd
}

func parseCurrentMode(value string) (runtimepresence.Mode, bool, error) {
	switch runtimepresence.Mode(value) {
	case "":
		return "", false, nil
	case runtimepresence.ModeAll:
		return runtimepresence.ModeAll, true, nil
	case runtimepresence.ModeExact:
		return runtimepresence.ModeExact, true, nil
	default:
		return "", false, invalidUsage(fmt.Errorf(
			"invalid --current value %q (expected all or exact)",
			value,
		))
	}
}

func openPresenceProviders(
	factory presenceProviderFactory,
	harnesses []sessionio.Harness,
) ([]runtimepresence.Provider, error) {
	if factory == nil {
		return nil, errors.New(
			"configure runtime presence: provider factory is unavailable",
		)
	}
	providers, err := factory(append([]sessionio.Harness(nil), harnesses...))
	if err != nil {
		return nil, err
	}
	if len(providers) != len(harnesses) {
		return nil, fmt.Errorf(
			"configure runtime presence: provider factory returned %d providers for %d harnesses",
			len(providers),
			len(harnesses),
		)
	}
	return providers, nil
}

type showDetail string

const (
	detailNormalized showDetail = "normalized"
	detailNative     showDetail = "native"
	detailProvenance showDetail = "provenance"
)

func newShowCommand(newRegistry registryFactory) *cobra.Command {
	var detailValue string
	cmd := newReaderCommand(
		"show SESSION_ID",
		"Show one coding-agent session",
		cobra.ExactArgs(1),
		func(cmd *cobra.Command, args []string) error {
			detail, err := parseShowDetail(detailValue)
			if err != nil {
				return err
			}
			found, err := openSelectedSession(
				cmd.Context(),
				newRegistry,
				args[0],
			)
			if err != nil {
				return err
			}
			items, err := readItems(
				cmd.Context(),
				found.adapter,
				found.session,
			)
			if err != nil {
				return err
			}
			if err := writeShowHuman(
				cmd.OutOrStdout(),
				found.session,
				items,
				detail,
			); err != nil {
				return err
			}
			return writeReadDiagnostics(
				cmd.ErrOrStderr(),
				found.session,
				items,
			)
		},
	)
	cmd.Flags().StringVar(
		&detailValue,
		"detail",
		string(detailNormalized),
		"detail view: normalized, native, or provenance",
	)
	registerFixedFlagCompletion(
		cmd,
		"detail",
		"normalized",
		"native",
		"provenance",
	)
	return cmd
}

func parseShowDetail(value string) (showDetail, error) {
	detail := showDetail(value)
	switch detail {
	case detailNormalized, detailNative, detailProvenance:
		return detail, nil
	default:
		return "", invalidUsage(fmt.Errorf(
			"invalid detail %q (expected normalized, native, or provenance)",
			value,
		))
	}
}

func newExportCommand(
	info buildinfo.Info,
	newRegistry registryFactory,
) *cobra.Command {
	var formatValue string
	cmd := newReaderCommand(
		"export SESSION_ID",
		"Export one coding-agent session",
		cobra.ExactArgs(1),
		func(cmd *cobra.Command, args []string) error {
			format, err := parseOutputFormat(
				formatValue,
				formatJSON,
				formatNDJSON,
			)
			if err != nil {
				return err
			}
			found, err := openSelectedSession(
				cmd.Context(),
				newRegistry,
				args[0],
			)
			if err != nil {
				return err
			}
			source, err := findSource(
				cmd.Context(),
				found.adapter,
				found.session.Occurrence.SourceID,
			)
			if err != nil {
				return err
			}
			if format == formatNDJSON {
				return streamExport(
					cmd.Context(),
					cmd.OutOrStdout(),
					producer(info),
					source,
					found.session,
					found.adapter,
				)
			}
			items, err := readItems(
				cmd.Context(),
				found.adapter,
				found.session,
			)
			if err != nil {
				return err
			}
			records := make([]sessionio.Record, 0, 2+len(items))
			records = append(
				records,
				sourceRecord(&source),
				sessionRecord(&found.session),
			)
			for index := range items {
				records = append(records, readItemRecord(&items[index]))
			}
			return writeRecords(
				cmd.OutOrStdout(),
				producer(info),
				formatJSON,
				records,
			)
		},
	)
	addFormatFlag(
		cmd,
		&formatValue,
		string(formatNDJSON),
		"output format: json or ndjson",
		"json",
		"ndjson",
	)
	return cmd
}

func addHarnessFlag(cmd *cobra.Command, harnesses *[]string) {
	cmd.Flags().StringArrayVar(
		harnesses,
		"harness",
		nil,
		"include a registered harness (repeatable)",
	)
	registerFixedFlagCompletion(cmd, "harness", "codex", "claude")
}

func addFormatFlag(
	cmd *cobra.Command,
	value *string,
	defaultValue string,
	usage string,
	completions ...string,
) {
	cmd.Flags().StringVar(value, "format", defaultValue, usage)
	registerFixedFlagCompletion(cmd, "format", completions...)
}

func registerFixedFlagCompletion(
	cmd *cobra.Command,
	name string,
	values ...string,
) {
	completions := make([]cobra.Completion, len(values))
	for index, value := range values {
		completions[index] = value
	}
	if err := cmd.RegisterFlagCompletionFunc(
		name,
		cobra.FixedCompletions(
			completions,
			cobra.ShellCompDirectiveNoFileComp,
		),
	); err != nil {
		panic(err)
	}
}

func mapRecords[T any](
	values []T,
	record func(*T) sessionio.Record,
) []sessionio.Record {
	records := make([]sessionio.Record, 0, len(values))
	for index := range values {
		records = append(records, record(&values[index]))
	}
	return records
}

func producer(info buildinfo.Info) sessionio.Producer {
	return sessionio.Producer{Name: "sessionio", Version: info.Version}
}

func selectHarnesses(
	registry *sessionio.Registry,
	requested []string,
) ([]sessionio.Harness, error) {
	descriptors := registry.Descriptors()
	registered := make(map[sessionio.Harness]struct{}, len(descriptors))
	for _, descriptor := range descriptors {
		registered[descriptor.Harness] = struct{}{}
	}
	if len(requested) == 0 {
		selected := make([]sessionio.Harness, len(descriptors))
		for index, descriptor := range descriptors {
			selected[index] = descriptor.Harness
		}
		return selected, nil
	}
	seen := make(map[sessionio.Harness]struct{}, len(requested))
	selected := make([]sessionio.Harness, 0, len(requested))
	for _, value := range requested {
		harness := sessionio.Harness(value)
		if _, found := registered[harness]; !found {
			return nil, invalidUsage(fmt.Errorf(
				"harness %q is not registered",
				value,
			))
		}
		if _, duplicate := seen[harness]; duplicate {
			continue
		}
		seen[harness] = struct{}{}
		selected = append(selected, harness)
	}
	sort.Slice(selected, func(left, right int) bool {
		return selected[left] < selected[right]
	})
	return selected, nil
}

func collectSources(
	ctx context.Context,
	registry *sessionio.Registry,
	harnesses []sessionio.Harness,
) ([]sessionio.Source, error) {
	var sources []sessionio.Source
	for _, harness := range harnesses {
		adapter, found := registry.Adapter(harness)
		if !found {
			return nil, fmt.Errorf("registered adapter %q disappeared", harness)
		}
		stream, err := adapter.Sources(ctx)
		if err != nil {
			return nil, err
		}
		if err := consumeStream(ctx, stream, func(source sessionio.Source) error {
			sources = append(sources, source)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(sources, func(left, right int) bool {
		if sources[left].Harness != sources[right].Harness {
			return sources[left].Harness < sources[right].Harness
		}
		if sources[left].Kind != sources[right].Kind {
			return sourceKindRank(sources[left].Kind) <
				sourceKindRank(sources[right].Kind)
		}
		return sources[left].ID < sources[right].ID
	})
	return sources, nil
}

func sourceKindRank(kind sessionio.SourceKind) int {
	if kind == sessionio.SourceKindCanonical {
		return 0
	}
	return 1
}

func collectSessions(
	ctx context.Context,
	registry *sessionio.Registry,
	harnesses []sessionio.Harness,
	resolveActivity bool,
) ([]sessionio.SessionRef, error) {
	var sessions []sessionio.SessionRef
	for _, harness := range harnesses {
		adapter, found := registry.Adapter(harness)
		if !found {
			return nil, fmt.Errorf("registered adapter %q disappeared", harness)
		}
		stream, err := adapter.Sessions(ctx, sessionio.SessionRequest{})
		if err != nil {
			return nil, err
		}
		if err := consumeStream(
			ctx,
			stream,
			func(session sessionio.SessionRef) error {
				if resolveActivity && session.UpdatedAt == nil {
					updated, err := observedActivity(ctx, adapter, session)
					if err != nil {
						return err
					}
					session.UpdatedAt = updated
				}
				sessions = append(sessions, session)
				return nil
			},
		); err != nil {
			return nil, err
		}
	}
	return sessions, nil
}

func observedActivity(
	ctx context.Context,
	adapter sessionio.Adapter,
	session sessionio.SessionRef,
) (*time.Time, error) {
	stream, err := adapter.Read(ctx, session)
	if err != nil {
		return nil, err
	}
	var latest *time.Time
	err = consumeStream(ctx, stream, func(item sessionio.ReadItem) error {
		if item.Observation.Timestamp == nil {
			return nil
		}
		if latest == nil || item.Observation.Timestamp.After(*latest) {
			value := *item.Observation.Timestamp
			latest = &value
		}
		return nil
	})
	return latest, err
}

func sortSessions(sessions []sessionio.SessionRef) {
	sort.SliceStable(sessions, func(left, right int) bool {
		leftTime := sessionActivity(sessions[left])
		rightTime := sessionActivity(sessions[right])
		switch {
		case leftTime != nil && rightTime != nil &&
			!leftTime.Equal(*rightTime):
			return leftTime.After(*rightTime)
		case leftTime != nil && rightTime == nil:
			return true
		case leftTime == nil && rightTime != nil:
			return false
		case sessions[left].Occurrence.Harness !=
			sessions[right].Occurrence.Harness:
			return sessions[left].Occurrence.Harness <
				sessions[right].Occurrence.Harness
		default:
			return sessions[left].ID < sessions[right].ID
		}
	})
}

func sessionActivity(session sessionio.SessionRef) *time.Time {
	if session.UpdatedAt != nil {
		return session.UpdatedAt
	}
	return session.StartedAt
}

type timeFilter struct {
	since *time.Time
	until *time.Time
}

func (filter timeFilter) active() bool {
	return filter.since != nil || filter.until != nil
}

func (filter timeFilter) apply(
	sessions []sessionio.SessionRef,
) []sessionio.SessionRef {
	if !filter.active() {
		return sessions
	}
	selected := make([]sessionio.SessionRef, 0, len(sessions))
	for _, session := range sessions {
		activity := sessionActivity(session)
		if activity == nil {
			continue
		}
		if filter.since != nil && activity.Before(*filter.since) {
			continue
		}
		if filter.until != nil && activity.After(*filter.until) {
			continue
		}
		selected = append(selected, session)
	}
	return selected
}

func parseTimeFilter(
	sinceValue string,
	untilValue string,
	now func() time.Time,
) (timeFilter, error) {
	var filter timeFilter
	if sinceValue == "" && untilValue == "" {
		return filter, nil
	}
	if now == nil {
		return filter, errors.New("configure reader: clock is unavailable")
	}
	capturedNow := now()
	var err error
	if sinceValue != "" {
		filter.since, err = parseTimeBound(sinceValue, capturedNow)
		if err != nil {
			return timeFilter{}, invalidUsage(fmt.Errorf(
				"invalid --since: %w",
				err,
			))
		}
	}
	if untilValue != "" {
		filter.until, err = parseTimeBound(untilValue, capturedNow)
		if err != nil {
			return timeFilter{}, invalidUsage(fmt.Errorf(
				"invalid --until: %w",
				err,
			))
		}
	}
	if filter.since != nil &&
		filter.until != nil &&
		filter.since.After(*filter.until) {
		return timeFilter{}, invalidUsage(errors.New(
			"--since must not be later than --until",
		))
	}
	return filter, nil
}

func parseTimeBound(value string, now time.Time) (*time.Time, error) {
	if absolute, err := time.Parse(time.RFC3339, value); err == nil {
		return &absolute, nil
	}
	if len(value) < 2 {
		return nil, errors.New("expected RFC3339 or a positive integer plus m, h, d, or w")
	}
	digits := value[:len(value)-1]
	for _, character := range digits {
		if character < '0' || character > '9' {
			return nil, errors.New("expected RFC3339 or a positive integer plus m, h, d, or w")
		}
	}
	count, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse positive relative time: %w", err)
	}
	if count <= 0 {
		return nil, errors.New("expected RFC3339 or a positive integer plus m, h, d, or w")
	}
	var unit time.Duration
	switch value[len(value)-1] {
	case 'm':
		unit = time.Minute
	case 'h':
		unit = time.Hour
	case 'd':
		unit = 24 * time.Hour
	case 'w':
		unit = 7 * 24 * time.Hour
	default:
		return nil, errors.New("expected RFC3339 or a positive integer plus m, h, d, or w")
	}
	if count > int64((time.Duration(1<<63-1))/unit) {
		return nil, errors.New("relative time is too large")
	}
	result := now.Add(-time.Duration(count) * unit)
	return &result, nil
}

type foundSession struct {
	adapter sessionio.Adapter
	session sessionio.SessionRef
}

func openSelectedSession(
	ctx context.Context,
	newRegistry registryFactory,
	id string,
) (foundSession, error) {
	registry, err := openRegistry(newRegistry)
	if err != nil {
		return foundSession{}, err
	}
	return findSession(ctx, registry, id)
}

func findSession(
	ctx context.Context,
	registry *sessionio.Registry,
	id string,
) (foundSession, error) {
	var matches []foundSession
	for _, descriptor := range registry.Descriptors() {
		adapter, found := registry.Adapter(descriptor.Harness)
		if !found {
			return foundSession{}, fmt.Errorf(
				"registered adapter %q disappeared",
				descriptor.Harness,
			)
		}
		stream, err := adapter.Sessions(ctx, sessionio.SessionRequest{})
		if err != nil {
			return foundSession{}, err
		}
		if err := consumeStream(
			ctx,
			stream,
			func(session sessionio.SessionRef) error {
				if string(session.ID) == id {
					matches = append(matches, foundSession{
						adapter: adapter,
						session: session,
					})
				}
				return nil
			},
		); err != nil {
			return foundSession{}, err
		}
	}
	switch len(matches) {
	case 0:
		return foundSession{}, missingSession(id)
	case 1:
		return matches[0], nil
	default:
		return foundSession{}, fmt.Errorf(
			"session ID %q is ambiguous across %d occurrences",
			id,
			len(matches),
		)
	}
}

func findSource(
	ctx context.Context,
	adapter sessionio.Adapter,
	id sessionio.SourceID,
) (sessionio.Source, error) {
	stream, err := adapter.Sources(ctx)
	if err != nil {
		return sessionio.Source{}, err
	}
	var matches []sessionio.Source
	if err := consumeStream(ctx, stream, func(source sessionio.Source) error {
		if source.ID == id {
			matches = append(matches, source)
		}
		return nil
	}); err != nil {
		return sessionio.Source{}, err
	}
	switch len(matches) {
	case 1:
		if matches[0].Kind != sessionio.SourceKindCanonical {
			return sessionio.Source{}, fmt.Errorf(
				"source %q for selected session is not canonical",
				id,
			)
		}
		return matches[0], nil
	case 0:
		return sessionio.Source{}, fmt.Errorf(
			"source %q for selected session was not found",
			id,
		)
	default:
		return sessionio.Source{}, fmt.Errorf(
			"source %q is duplicated by its adapter",
			id,
		)
	}
}

func readItems(
	ctx context.Context,
	adapter sessionio.Adapter,
	session sessionio.SessionRef,
) ([]sessionio.ReadItem, error) {
	stream, err := adapter.Read(ctx, session)
	if err != nil {
		return nil, err
	}
	var items []sessionio.ReadItem
	if err := consumeStream(ctx, stream, func(item sessionio.ReadItem) error {
		items = append(items, item)
		return nil
	}); err != nil {
		return nil, err
	}
	return items, nil
}

func consumeStream[T any](
	ctx context.Context,
	stream sessionio.Stream[T],
	consume func(T) error,
) error {
	if nilStream(stream) {
		return errors.New("adapter returned a nil stream")
	}
	for {
		value, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			return stream.Close()
		}
		if err != nil {
			return errors.Join(err, stream.Close())
		}
		if err := consume(value); err != nil {
			return errors.Join(err, stream.Close())
		}
	}
}

func nilStream[T any](stream sessionio.Stream[T]) bool {
	if stream == nil {
		return true
	}
	value := reflect.ValueOf(stream)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func sourceRecord(source *sessionio.Source) sessionio.Record {
	return sessionio.Record{
		Kind:   sessionio.RecordKindSource,
		Source: source,
	}
}

func sessionRecord(session *sessionio.SessionRef) sessionio.Record {
	return sessionio.Record{
		Kind:    sessionio.RecordKindSession,
		Session: session,
	}
}

func readItemRecord(item *sessionio.ReadItem) sessionio.Record {
	return sessionio.Record{
		Kind:     sessionio.RecordKindReadItem,
		ReadItem: item,
	}
}

func writeRecords(
	writer io.Writer,
	producer sessionio.Producer,
	format outputFormat,
	records []sessionio.Record,
) error {
	if format == formatJSON {
		return sessionio.WriteJSON(writer, producer, records)
	}
	encoder, err := sessionio.NewNDJSONEncoder(writer, producer)
	if err != nil {
		return err
	}
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			return err
		}
	}
	return nil
}

func streamExport(
	ctx context.Context,
	writer io.Writer,
	producer sessionio.Producer,
	source sessionio.Source,
	session sessionio.SessionRef,
	adapter sessionio.Adapter,
) error {
	encoder, err := sessionio.NewNDJSONEncoder(writer, producer)
	if err != nil {
		return err
	}
	if err := encoder.Encode(sourceRecord(&source)); err != nil {
		return err
	}
	if err := encoder.Encode(sessionRecord(&session)); err != nil {
		return err
	}
	stream, err := adapter.Read(ctx, session)
	if err != nil {
		return err
	}
	return consumeStream(ctx, stream, func(item sessionio.ReadItem) error {
		return encoder.Encode(readItemRecord(&item))
	})
}

func writeSourcesHuman(writer io.Writer, sources []sessionio.Source) error {
	if _, err := fmt.Fprintln(writer, "HARNESS\tKIND\tSTATUS\tID\tLOCATOR"); err != nil {
		return fmt.Errorf("write sources heading: %w", err)
	}
	for _, source := range sources {
		if _, err := fmt.Fprintf(
			writer,
			"%s\t%s\t%s\t%s\t%s\n",
			source.Harness,
			source.Kind,
			source.Status,
			source.ID,
			formatLocator(source.Locator),
		); err != nil {
			return fmt.Errorf("write source: %w", err)
		}
	}
	return nil
}

func writeSessionsHuman(
	writer io.Writer,
	sessions []sessionio.SessionRef,
) error {
	if _, err := fmt.Fprintln(writer, "ACTIVITY\tHARNESS\tID\tTITLE"); err != nil {
		return fmt.Errorf("write sessions heading: %w", err)
	}
	for _, session := range sessions {
		if _, err := fmt.Fprintf(
			writer,
			"%s\t%s\t%s\t%s\n",
			formatTime(sessionActivity(session)),
			session.Occurrence.Harness,
			session.ID,
			oneLine(session.Title),
		); err != nil {
			return fmt.Errorf("write session: %w", err)
		}
	}
	return nil
}

func writeShowHuman(
	writer io.Writer,
	session sessionio.SessionRef,
	items []sessionio.ReadItem,
	detail showDetail,
) error {
	if _, err := fmt.Fprintf(
		writer,
		"session: %s\n"+
			"harness: %s\n"+
			"native_id: %s\n"+
			"title: %s\n"+
			"started_at: %s\n"+
			"updated_at: %s\n"+
			"source_id: %s\n"+
			"detail: %s\n",
		session.ID,
		session.Occurrence.Harness,
		session.NativeID,
		oneLine(session.Title),
		formatTime(session.StartedAt),
		formatTime(session.UpdatedAt),
		session.Occurrence.SourceID,
		detail,
	); err != nil {
		return fmt.Errorf("write session heading: %w", err)
	}
	if detail == detailProvenance {
		if _, err := fmt.Fprintf(
			writer,
			"discovery_revision: %s\n"+
				"occurrence_id: %s\n"+
				"occurrence_locator: %s\n",
			session.DiscoveryRevision,
			session.Occurrence.ID,
			formatLocator(session.Occurrence.Locator),
		); err != nil {
			return fmt.Errorf("write session provenance: %w", err)
		}
	}
	for _, item := range items {
		var err error
		switch detail {
		case detailNormalized:
			err = writeNormalizedItem(writer, item)
		case detailNative:
			err = writeNativeItem(writer, item)
		case detailProvenance:
			err = writeProvenanceItem(writer, item)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func writeNormalizedItem(writer io.Writer, item sessionio.ReadItem) error {
	if _, err := fmt.Fprintf(
		writer,
		"\nobservation %s %s %s\n",
		item.Observation.ID,
		item.Observation.NativeKind,
		formatTime(item.Observation.Timestamp),
	); err != nil {
		return fmt.Errorf("write observation: %w", err)
	}
	for _, event := range item.Events {
		if _, err := fmt.Fprintf(writer, "  event %s %s", event.ID, event.Kind); err != nil {
			return fmt.Errorf("write event: %w", err)
		}
		switch {
		case event.Message != nil:
			if _, err := fmt.Fprintf(writer, " role=%s\n", event.Message.Role); err != nil {
				return err
			}
			if err := writeContent(writer, "    ", event.Message.Content); err != nil {
				return err
			}
		case event.Reasoning != nil:
			if _, err := fmt.Fprintln(writer); err != nil {
				return err
			}
			if err := writeContent(writer, "    reasoning ", event.Reasoning.Content); err != nil {
				return err
			}
			if err := writeContent(writer, "    summary ", event.Reasoning.Summary); err != nil {
				return err
			}
		case event.ToolCall != nil:
			if _, err := fmt.Fprintf(
				writer,
				" name=%s call_id=%s input_media_type=%s input_bytes=%d\n",
				event.ToolCall.Name,
				event.ToolCall.CallID,
				event.ToolCall.Input.MediaType,
				len(event.ToolCall.Input.Data),
			); err != nil {
				return err
			}
		case event.ToolResult != nil:
			if _, err := fmt.Fprintf(
				writer,
				" call_id=%s status=%s output_media_type=%s output_bytes=%d\n",
				event.ToolResult.CallID,
				event.ToolResult.Status,
				event.ToolResult.Output.MediaType,
				len(event.ToolResult.Output.Data),
			); err != nil {
				return err
			}
		default:
			data, err := json.Marshal(event)
			if err != nil {
				return fmt.Errorf("encode normalized event: %w", err)
			}
			if _, err := fmt.Fprintf(writer, " %s\n", data); err != nil {
				return err
			}
		}
	}
	for _, relation := range item.Relations {
		if _, err := fmt.Fprintf(
			writer,
			"  relation %s %s %s:%s -> %s:%s origin=%s\n",
			relation.ID,
			relation.Kind,
			relation.From.Kind,
			relation.From.ID,
			relation.To.Kind,
			relation.To.ID,
			relation.Origin,
		); err != nil {
			return fmt.Errorf("write relation: %w", err)
		}
	}
	return nil
}

func writeContent(
	writer io.Writer,
	prefix string,
	content []sessionio.ContentBlock,
) error {
	for _, block := range content {
		switch {
		case block.Text != nil:
			if _, err := fmt.Fprintf(
				writer,
				"%s%s\n",
				prefix,
				block.Text.Text,
			); err != nil {
				return fmt.Errorf("write text content: %w", err)
			}
		case block.Media != nil:
			if _, err := fmt.Fprintf(
				writer,
				"%smedia %s availability=%s bytes=%d reference=%s\n",
				prefix,
				block.Media.MediaType,
				block.Availability,
				len(block.Media.Data),
				block.Media.Reference,
			); err != nil {
				return fmt.Errorf("write media content: %w", err)
			}
		case block.Opaque != nil:
			if _, err := fmt.Fprintf(
				writer,
				"%sopaque %s availability=%s bytes=%d\n",
				prefix,
				block.Opaque.NativeType,
				block.Availability,
				len(block.Opaque.Data),
			); err != nil {
				return fmt.Errorf("write opaque content: %w", err)
			}
		default:
			if _, err := fmt.Fprintf(
				writer,
				"%s%s availability=%s\n",
				prefix,
				block.Kind,
				block.Availability,
			); err != nil {
				return fmt.Errorf("write unavailable content: %w", err)
			}
		}
	}
	return nil
}

func writeNativeItem(writer io.Writer, item sessionio.ReadItem) error {
	observation := item.Observation
	if _, err := fmt.Fprintf(
		writer,
		"\nobservation: %s\n"+
			"native_kind: %s\n"+
			"native_version: %s\n"+
			"timestamp: %s\n"+
			"locator: %s\n"+
			"revision: %s:%s\n"+
			"capture: %s\n"+
			"codec: %s\n"+
			"media_type: %s\n"+
			"data_base64: %s\n"+
			"framing_base64: %s\n",
		observation.ID,
		observation.NativeKind,
		observation.NativeVersion,
		formatTime(observation.Timestamp),
		formatLocator(observation.Locator),
		observation.Revision.Kind,
		observation.Revision.Value,
		observation.Representation.Capture,
		observation.Representation.Codec,
		observation.Representation.MediaType,
		base64.StdEncoding.EncodeToString(observation.Representation.Data),
		base64.StdEncoding.EncodeToString(observation.Representation.Framing),
	); err != nil {
		return fmt.Errorf("write native observation: %w", err)
	}
	return nil
}

func writeProvenanceItem(writer io.Writer, item sessionio.ReadItem) error {
	observation := item.Observation
	if _, err := fmt.Fprintf(
		writer,
		"\nobservation: %s\n"+
			"locator: %s\n"+
			"revision: %s:%s\n",
		observation.ID,
		formatLocator(observation.Locator),
		observation.Revision.Kind,
		observation.Revision.Value,
	); err != nil {
		return fmt.Errorf("write observation provenance: %w", err)
	}
	for _, limitation := range observation.Limitations {
		if _, err := fmt.Fprintf(
			writer,
			"  limitation %s %s\n",
			limitation.Kind,
			oneLine(limitation.Detail),
		); err != nil {
			return fmt.Errorf("write limitation: %w", err)
		}
	}
	for _, event := range item.Events {
		if _, err := fmt.Fprintf(writer, "  event %s %s\n", event.ID, event.Kind); err != nil {
			return fmt.Errorf("write event provenance: %w", err)
		}
		for _, evidence := range event.Evidence {
			if _, err := fmt.Fprintf(
				writer,
				"    evidence observation=%s locator=%s\n",
				evidence.Observation,
				formatLocator(evidence.Locator),
			); err != nil {
				return fmt.Errorf("write event evidence: %w", err)
			}
		}
	}
	for _, relation := range item.Relations {
		if _, err := fmt.Fprintf(
			writer,
			"  relation %s %s origin=%s\n",
			relation.ID,
			relation.Kind,
			relation.Origin,
		); err != nil {
			return fmt.Errorf("write relation provenance: %w", err)
		}
		for _, evidence := range relation.Evidence {
			if _, err := fmt.Fprintf(
				writer,
				"    evidence observation=%s locator=%s\n",
				evidence.Observation,
				formatLocator(evidence.Locator),
			); err != nil {
				return fmt.Errorf("write relation evidence: %w", err)
			}
		}
	}
	return nil
}

func writeSourceDiagnostics(
	writer io.Writer,
	sources []sessionio.Source,
) error {
	for _, source := range sources {
		if err := writeDiagnostics(
			writer,
			fmt.Sprintf("source %s", source.ID),
			source.Diagnostics,
		); err != nil {
			return err
		}
	}
	return nil
}

func writeSessionDiagnostics(
	writer io.Writer,
	sessions []sessionio.SessionRef,
) error {
	for _, session := range sessions {
		if err := writeDiagnostics(
			writer,
			fmt.Sprintf("session %s", session.ID),
			session.Diagnostics,
		); err != nil {
			return err
		}
	}
	return nil
}

func writeReadDiagnostics(
	writer io.Writer,
	session sessionio.SessionRef,
	items []sessionio.ReadItem,
) error {
	if err := writeDiagnostics(
		writer,
		fmt.Sprintf("session %s", session.ID),
		session.Diagnostics,
	); err != nil {
		return err
	}
	for _, item := range items {
		if err := writeDiagnostics(
			writer,
			fmt.Sprintf("observation %s", item.Observation.ID),
			item.Diagnostics,
		); err != nil {
			return err
		}
	}
	return nil
}

func writeDiagnostics(
	writer io.Writer,
	context string,
	diagnostics []sessionio.Diagnostic,
) error {
	for _, diagnostic := range diagnostics {
		locator := ""
		if diagnostic.Locator != nil {
			locator = " locator=" + formatLocator(*diagnostic.Locator)
		}
		if _, err := fmt.Fprintf(
			writer,
			"%s: %s %s: %s%s\n",
			context,
			diagnostic.Severity,
			diagnostic.Code,
			diagnostic.Message,
			locator,
		); err != nil {
			return fmt.Errorf("write diagnostic: %w", err)
		}
	}
	return nil
}

func formatTime(value *time.Time) string {
	if value == nil {
		return "-"
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func formatLocator(locator sessionio.SourceLocator) string {
	switch locator.Kind {
	case sessionio.LocatorKindFile:
		if locator.File == nil {
			return string(locator.Kind)
		}
		return fmt.Sprintf(
			"%s:root=%q path=%q%s%s%s",
			locator.Kind,
			locator.File.Root,
			locator.File.Path,
			optionalUint(" record", locator.File.Record),
			optionalUint(" line", locator.File.Line),
			optionalRange(locator.File.ByteRange),
		)
	case sessionio.LocatorKindDatabase:
		if locator.Database == nil {
			return string(locator.Kind)
		}
		parts := []string{
			fmt.Sprintf(
				"%s:path=%q table=%q",
				locator.Kind,
				locator.Database.Path,
				locator.Database.Table,
			),
		}
		for _, key := range locator.Database.Keys {
			parts = append(
				parts,
				fmt.Sprintf("%s=%q", key.Name, key.Value),
			)
		}
		return strings.Join(parts, " ")
	case sessionio.LocatorKindOpaque:
		if locator.Opaque == nil {
			return string(locator.Kind)
		}
		return fmt.Sprintf(
			"%s:scheme=%q value=%q",
			locator.Kind,
			locator.Opaque.Scheme,
			locator.Opaque.Value,
		)
	default:
		return string(locator.Kind)
	}
}

func optionalUint(label string, value *uint64) string {
	if value == nil {
		return ""
	}
	return label + "=" + strconv.FormatUint(*value, 10)
}

func optionalRange(value *sessionio.ByteRange) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf(" bytes=%d-%d", value.Start, value.End)
}

func oneLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

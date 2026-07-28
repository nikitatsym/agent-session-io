package catalog

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/nikitatsym/agent-session-io/internal/config"
)

// Revision is the single current draft revision of the catalog schema.
const Revision = 5

// SupportedPostgresMajor is the only accepted PostgreSQL major version.
const SupportedPostgresMajor = 18

// BM25TextConfig is fixed at review from the Step 2 measurement evidence:
// pg_catalog.russian stems Cyrillic and English words and drops both stopword
// sets, so it is the only measured configuration with mixed-corpus recall.
const BM25TextConfig = "russian"

// TrigramIndex is fixed at review from the Step 2 measurement evidence: the
// planner never chose gist at realistic selectivity and gin needs no recheck.
const TrigramIndex = "gin"

// ProjectionKindLexical is the exhaustive text projection served by both the
// BM25 and the literal leg; other projection kinds are additional rows.
const ProjectionKindLexical = "lexical"

// advisoryLockKey keeps sessionio catalog maintenance single-writer.
const advisoryLockKey = 0x5e5510

// Derived tables are shared and immutable. A row belongs to one session
// revision built by one builder-version set; a generation presents it through
// generation_member and never owns a copy of it.
const (
	tableDerivedSession      = "derived_session"
	tableDerivedEvent        = "derived_event"
	tableDerivedEvidence     = "derived_evidence"
	tableDerivedRelation     = "derived_relation"
	tableDerivedPassage      = "derived_passage"
	tableDerivedPassageEvent = "derived_passage_event"
	tableSearchDocument      = "search_document"
	tableProjectionLimit     = "projection_limitation"
	tableSearchFacet         = "search_facet"
)

// Shared index names no longer carry a generation number.
const (
	bm25IndexName    = tableSearchDocument + "_bm25"
	trigramIndexName = tableSearchDocument + "_trgm"
)

func vectorIndexName(space int64) string {
	return fmt.Sprintf("%s_hnsw_s%d", tableSearchDocument, space)
}

// derivedSequences feed the writer-assigned identifiers of the shared tables.
var derivedSequences = map[string]string{
	tableDerivedSession:  "derived_session_id",
	tableDerivedEvent:    "derived_event_id",
	tableDerivedEvidence: "derived_evidence_id",
	tableDerivedRelation: "derived_relation_id",
	tableDerivedPassage:  "derived_passage_id",
	tableSearchDocument:  "search_document_id",
}

// derivedTables are deleted by reclaim in dependency order.
var derivedTables = []string{
	tableSearchFacet,
	tableProjectionLimit,
	tableSearchDocument,
	tableDerivedPassageEvent,
	tableDerivedPassage,
	tableDerivedRelation,
	tableDerivedEvidence,
	tableDerivedEvent,
	tableDerivedSession,
}

var schemaNameExpression = regexp.MustCompile(config.SchemaNamePattern)

// substrateTables are created by Init and verified on every later Init.
var substrateTables = []string{
	"catalog_meta",
	"generation",
	"active_generation",
	"embedding_space",
	"embedding_cache",
	"source",
	"source_occurrence",
	"snapshot_blob",
	"session_revision",
	"scan_checkpoint",
	"generation_member",
	tableDerivedSession,
	tableDerivedEvent,
	tableDerivedEvidence,
	tableDerivedRelation,
	tableDerivedPassage,
	tableDerivedPassageEvent,
	tableSearchDocument,
	tableProjectionLimit,
	tableSearchFacet,
}

// retainedTables hold state class 1: observations, immutable revisions, and
// checkpoints. They survive a reindex and are the state stream's payload.
var retainedTables = []string{
	"source",
	"source_occurrence",
	"snapshot_blob",
	"session_revision",
	"scan_checkpoint",
}

// BuilderKey canonicalizes the builder-version set that produced a derived row.
// Reuse keys on it, so rows of a superseded builder are never presented again.
func BuilderKey(versions map[string]string) string {
	names := make([]string, 0, len(versions))
	for name := range versions {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+versions[name])
	}
	return strings.Join(parts, ";")
}

// ValidateSchemaName rejects every identifier the catalog refuses to quote.
func ValidateSchemaName(name string) error {
	if schemaNameExpression.MatchString(name) {
		return nil
	}
	return &Error{
		Kind: KindConfigInvalid,
		Message: fmt.Sprintf(
			"catalog schema name %q is invalid, expected %s",
			name,
			config.SchemaNamePattern,
		),
		Remediation: "set search.schema_name to a lower-case PostgreSQL" +
			" identifier such as sessionio",
		Details: map[string]any{"schema_name": name},
	}
}

func quoteIdentifier(name string) string {
	return pgx.Identifier{name}.Sanitize()
}

// table qualifies one catalog table for direct interpolation into SQL.
func (catalog *Catalog) table(name string) string {
	return catalog.schema + "." + quoteIdentifier(name)
}

func substrateStatements(schema string) []string {
	statements := []string{
		fmt.Sprintf(`CREATE TABLE %s.catalog_meta (
	one boolean PRIMARY KEY DEFAULT true CHECK (one),
	revision integer NOT NULL,
	initialized_at timestamptz NOT NULL
)`, schema),
		fmt.Sprintf(`CREATE TABLE %s.generation (
	id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	state text NOT NULL CHECK (state IN (
		'building', 'complete', 'partial', 'failed', 'superseded')),
	parent_id bigint REFERENCES %s.generation (id),
	source_set jsonb,
	builder_versions jsonb,
	counts jsonb,
	diagnostics jsonb,
	created_at timestamptz NOT NULL DEFAULT now(),
	published_at timestamptz,
	reclaimed_at timestamptz
)`, schema, schema),
		fmt.Sprintf(`CREATE TABLE %s.active_generation (
	one boolean PRIMARY KEY DEFAULT true CHECK (one),
	generation_id bigint NOT NULL REFERENCES %s.generation (id)
)`, schema, schema),
		fmt.Sprintf(`CREATE TABLE %s.embedding_space (
	id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	name text NOT NULL UNIQUE,
	provider text NOT NULL,
	model text NOT NULL,
	dimensions integer NOT NULL CHECK (dimensions > 0),
	distance text NOT NULL CHECK (distance IN ('cosine'))
)`, schema),
		fmt.Sprintf(`CREATE TABLE %s.embedding_cache (
	space_id bigint NOT NULL REFERENCES %s.embedding_space (id),
	content_hash bytea NOT NULL,
	embedding vector NOT NULL,
	PRIMARY KEY (space_id, content_hash)
)`, schema, schema),
		// disappeared_at is the source tombstone: the row is retained evidence
		// that the source was once observed, not a record of a live source.
		fmt.Sprintf(`CREATE TABLE %s.source (
	source_id text PRIMARY KEY,
	harness text NOT NULL,
	locator_kind text NOT NULL,
	locator_root text NOT NULL,
	locator_path text NOT NULL,
	first_seen_at timestamptz NOT NULL,
	last_seen_at timestamptz NOT NULL,
	disappeared_at timestamptz
)`, schema),
		fmt.Sprintf(`CREATE TABLE %s.source_occurrence (
	occurrence_id text PRIMARY KEY,
	source_id text NOT NULL REFERENCES %s.source (source_id),
	harness text NOT NULL,
	locator_kind text NOT NULL,
	locator_root text NOT NULL,
	locator_path text NOT NULL,
	first_seen_at timestamptz NOT NULL,
	last_seen_at timestamptz NOT NULL,
	disappeared_at timestamptz
)`, schema, schema),
		// content_hash addresses the uncompressed snapshot, so equal bytes from
		// distinct occurrences share exactly one compressed blob.
		fmt.Sprintf(`CREATE TABLE %s.snapshot_blob (
	content_hash bytea PRIMARY KEY,
	codec text NOT NULL,
	codec_version text NOT NULL,
	uncompressed_size bigint NOT NULL CHECK (uncompressed_size >= 0),
	compressed_size bigint NOT NULL CHECK (compressed_size >= 0),
	checksum bytea NOT NULL,
	data bytea NOT NULL,
	created_at timestamptz NOT NULL
)`, schema),
		fmt.Sprintf(`CREATE TABLE %s.session_revision (
	revision_hash bytea PRIMARY KEY,
	session_key text NOT NULL,
	occurrence_id text NOT NULL REFERENCES %s.source_occurrence (occurrence_id),
	harness text NOT NULL,
	native_id text NOT NULL,
	title text NOT NULL,
	discovery_revision text NOT NULL,
	source_revision_kind text NOT NULL,
	source_revision_value text NOT NULL,
	snapshot_hash bytea NOT NULL REFERENCES %s.snapshot_blob (content_hash),
	locator_kind text NOT NULL,
	locator_root text NOT NULL,
	locator_path text NOT NULL,
	started_at timestamptz,
	updated_at timestamptz,
	event_count bigint NOT NULL,
	observed_at timestamptz NOT NULL
)`, schema, schema, schema),
		fmt.Sprintf(
			`CREATE INDEX ON %s.session_revision (occurrence_id)`,
			schema,
		),
		fmt.Sprintf(`CREATE TABLE %s.scan_checkpoint (
	occurrence_id text PRIMARY KEY REFERENCES %s.source_occurrence (occurrence_id),
	revision_hash bytea NOT NULL REFERENCES %s.session_revision (revision_hash),
	discovery_revision text NOT NULL,
	source_revision_value text NOT NULL,
	snapshot_hash bytea NOT NULL,
	snapshot_size bigint NOT NULL,
	source_size bigint NOT NULL,
	record_count bigint NOT NULL,
	file_identity text NOT NULL,
	tail_kind text NOT NULL CHECK (tail_kind IN ('clean', 'pending')),
	change_kind text NOT NULL CHECK (change_kind IN (
		'initial', 'unchanged', 'grown', 'truncated', 'rewritten', 'replaced')),
	observed_at timestamptz NOT NULL
)`, schema, schema, schema),
	}
	statements = append(statements, derivedStatements(schema)...)
	return statements
}

// derivedStatements create the shared immutable derived tables, the generation
// membership that presents them, and the single set of retrieval indexes.
func derivedStatements(schema string) []string {
	statements := make([]string, 0, 32)
	for _, sequence := range sortedSequences() {
		statements = append(statements, fmt.Sprintf(
			"CREATE SEQUENCE %s.%s AS bigint",
			schema,
			quoteIdentifier(sequence),
		))
	}
	table := func(name string) string {
		return schema + "." + quoteIdentifier(name)
	}
	return append(statements,
		// builder_key is the canonical builder-version set behind these rows, so
		// a builder bump produces a new derived session instead of relabelling
		// the old one.
		fmt.Sprintf(`CREATE TABLE %s (
	id bigint PRIMARY KEY,
	revision_hash bytea NOT NULL REFERENCES %s.session_revision (revision_hash),
	builder_key text NOT NULL,
	session_key text NOT NULL,
	harness text NOT NULL,
	native_id text NOT NULL,
	title text NOT NULL,
	source_id text NOT NULL,
	occurrence_id text NOT NULL,
	discovery_revision text NOT NULL,
	source_revision_kind text NOT NULL,
	source_revision_value text NOT NULL,
	locator_kind text NOT NULL,
	locator_root text NOT NULL,
	locator_path text NOT NULL,
	started_at timestamptz,
	updated_at timestamptz,
	UNIQUE (revision_hash, builder_key)
)`, table(tableDerivedSession), schema),
		fmt.Sprintf(`CREATE TABLE %s.generation_member (
	generation_id bigint NOT NULL REFERENCES %s.generation (id),
	derived_id bigint NOT NULL REFERENCES %s (id),
	PRIMARY KEY (generation_id, derived_id)
)`, schema, schema, table(tableDerivedSession)),
		fmt.Sprintf(
			"CREATE INDEX ON %s.generation_member (derived_id)",
			schema,
		),
		fmt.Sprintf(`CREATE TABLE %s (
	id bigint PRIMARY KEY,
	derived_id bigint NOT NULL REFERENCES %s (id),
	ordinal integer NOT NULL,
	event_key text NOT NULL,
	native_key text NOT NULL,
	kind text NOT NULL,
	role text NOT NULL,
	observation_id text NOT NULL,
	occurred_at timestamptz
)`, table(tableDerivedEvent), table(tableDerivedSession)),
		fmt.Sprintf(`CREATE TABLE %s (
	id bigint PRIMARY KEY,
	event_id bigint NOT NULL REFERENCES %s (id),
	position integer NOT NULL,
	observation_id text NOT NULL,
	locator_kind text NOT NULL,
	locator_root text NOT NULL,
	locator_path text NOT NULL,
	locator_record bigint,
	locator_line bigint,
	byte_start bigint,
	byte_end bigint
)`, table(tableDerivedEvidence), table(tableDerivedEvent)),
		// A relation target names an identity, never a row: resolution depends on
		// which revisions a generation presents, so it is computed per generation
		// and never stored on the immutable row.
		fmt.Sprintf(`CREATE TABLE %s (
	id bigint PRIMARY KEY,
	derived_id bigint NOT NULL REFERENCES %s (id),
	ordinal integer NOT NULL,
	kind text NOT NULL,
	origin text NOT NULL,
	from_kind text NOT NULL,
	from_ref text NOT NULL,
	to_kind text NOT NULL,
	to_ref text NOT NULL,
	observation_id text NOT NULL
)`, table(tableDerivedRelation), table(tableDerivedSession)),
		fmt.Sprintf(`CREATE TABLE %s (
	id bigint PRIMARY KEY,
	derived_id bigint NOT NULL REFERENCES %s (id),
	ordinal integer NOT NULL,
	kind text NOT NULL,
	builder_version text NOT NULL,
	part integer NOT NULL,
	parts integer NOT NULL CHECK (parts > 0),
	started_at timestamptz
)`, table(tableDerivedPassage), table(tableDerivedSession)),
		fmt.Sprintf(`CREATE TABLE %s (
	passage_id bigint NOT NULL REFERENCES %s (id),
	event_id bigint NOT NULL REFERENCES %s (id),
	position integer NOT NULL,
	PRIMARY KEY (passage_id, position)
)`, table(tableDerivedPassageEvent), table(tableDerivedPassage), table(tableDerivedEvent)),
		fmt.Sprintf(`CREATE TABLE %s (
	doc_id bigint PRIMARY KEY,
	derived_id bigint NOT NULL REFERENCES %s (id),
	session_ref text NOT NULL,
	harness text NOT NULL,
	passage_id bigint REFERENCES %s (id),
	projection_kind text NOT NULL DEFAULT '%s',
	projection_version text NOT NULL DEFAULT '',
	body text NOT NULL,
	content_hash bytea NOT NULL,
	embedding_space_id bigint REFERENCES %s.embedding_space (id),
	embedding vector
)`,
			table(tableSearchDocument),
			table(tableDerivedSession),
			table(tableDerivedPassage),
			ProjectionKindLexical,
			schema,
		),
		// A projection row is byte-exact unless it has a limitation row here, so
		// the table stays empty for a corpus without NUL bytes.
		fmt.Sprintf(`CREATE TABLE %s (
	doc_id bigint NOT NULL REFERENCES %s (doc_id),
	kind text NOT NULL,
	removed_bytes bigint NOT NULL CHECK (removed_bytes > 0),
	PRIMARY KEY (doc_id, kind)
)`, table(tableProjectionLimit), table(tableSearchDocument)),
		// derived_id is repeated here so the eligible set of one generation is a
		// single join away from a facet predicate.
		fmt.Sprintf(`CREATE TABLE %s (
	doc_id bigint NOT NULL REFERENCES %s (doc_id),
	derived_id bigint NOT NULL REFERENCES %s (id),
	namespace text NOT NULL,
	"key" text NOT NULL,
	value text NOT NULL
)`, table(tableSearchFacet), table(tableSearchDocument), table(tableDerivedSession)),
		fmt.Sprintf("CREATE INDEX ON %s (doc_id)", table(tableSearchFacet)),
		fmt.Sprintf(
			`CREATE INDEX ON %s (derived_id, namespace, "key", value)`,
			table(tableSearchFacet),
		),
		fmt.Sprintf("CREATE INDEX ON %s (derived_id)", table(tableSearchDocument)),
		fmt.Sprintf("CREATE INDEX ON %s (passage_id)", table(tableSearchDocument)),
		// Reclaim deletes events; without this index every deleted row makes
		// PostgreSQL scan the whole link table to check the foreign key.
		fmt.Sprintf(
			"CREATE INDEX ON %s (event_id)",
			table(tableDerivedPassageEvent),
		),
		fmt.Sprintf("CREATE INDEX ON %s (derived_id, ordinal)", table(tableDerivedEvent)),
		fmt.Sprintf("CREATE INDEX ON %s (observation_id)", table(tableDerivedEvent)),
		fmt.Sprintf("CREATE INDEX ON %s (event_key)", table(tableDerivedEvent)),
		// A harness without record identifiers indexes nothing.
		fmt.Sprintf(
			"CREATE INDEX ON %s (native_key) WHERE native_key <> ''",
			table(tableDerivedEvent),
		),
		fmt.Sprintf("CREATE INDEX ON %s (revision_hash)", table(tableDerivedSession)),
		fmt.Sprintf("CREATE INDEX ON %s (native_id)", table(tableDerivedSession)),
		fmt.Sprintf("CREATE INDEX ON %s (event_id, position)", table(tableDerivedEvidence)),
		fmt.Sprintf("CREATE INDEX ON %s (derived_id, ordinal)", table(tableDerivedPassage)),
		fmt.Sprintf("CREATE INDEX ON %s (derived_id, ordinal)", table(tableDerivedRelation)),
		fmt.Sprintf(
			"CREATE INDEX %s ON %s USING bm25 (body) WITH (text_config='%s')",
			quoteIdentifier(bm25IndexName),
			table(tableSearchDocument),
			BM25TextConfig,
		),
		fmt.Sprintf(
			"CREATE INDEX %s ON %s USING %s (body %s_trgm_ops)",
			quoteIdentifier(trigramIndexName),
			table(tableSearchDocument),
			TrigramIndex,
			TrigramIndex,
		),
	)
}

func sortedSequences() []string {
	names := make([]string, 0, len(derivedSequences))
	for _, sequence := range derivedSequences {
		names = append(names, sequence)
	}
	sort.Strings(names)
	return names
}

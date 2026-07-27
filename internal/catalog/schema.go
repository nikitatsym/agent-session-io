package catalog

import (
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/nikitatsym/agent-session-io/internal/config"
)

// Revision is the single current draft revision of the catalog schema.
const Revision = 3

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

func substrateStatements(schema string) []string {
	return []string{
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
	published_at timestamptz
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
		fmt.Sprintf(`CREATE TABLE %s.generation_member (
	generation_id bigint NOT NULL REFERENCES %s.generation (id),
	revision_hash bytea NOT NULL REFERENCES %s.session_revision (revision_hash),
	PRIMARY KEY (generation_id, revision_hash)
)`, schema, schema, schema),
	}
}

func documentTable(generation GenerationID) string {
	return fmt.Sprintf("search_document_g%d", generation)
}

func facetTable(generation GenerationID) string {
	return fmt.Sprintf("search_facet_g%d", generation)
}

func limitationTable(generation GenerationID) string {
	return fmt.Sprintf("projection_limitation_g%d", generation)
}

func sessionTable(generation GenerationID) string {
	return fmt.Sprintf("session_g%d", generation)
}

func eventTable(generation GenerationID) string {
	return fmt.Sprintf("event_g%d", generation)
}

func evidenceTable(generation GenerationID) string {
	return fmt.Sprintf("evidence_g%d", generation)
}

func passageTable(generation GenerationID) string {
	return fmt.Sprintf("passage_g%d", generation)
}

func passageEventTable(generation GenerationID) string {
	return fmt.Sprintf("passage_event_g%d", generation)
}

func relationTable(generation GenerationID) string {
	return fmt.Sprintf("relation_g%d", generation)
}

// generationTables are dropped by Cleanup in dependency order.
func generationTables(generation GenerationID) []string {
	return []string{
		facetTable(generation),
		limitationTable(generation),
		documentTable(generation),
		passageEventTable(generation),
		passageTable(generation),
		relationTable(generation),
		evidenceTable(generation),
		eventTable(generation),
		sessionTable(generation),
	}
}

func bm25IndexName(generation GenerationID) string {
	return documentTable(generation) + "_bm25"
}

func trigramIndexName(generation GenerationID) string {
	return documentTable(generation) + "_trgm"
}

func vectorIndexName(generation GenerationID, space int64) string {
	return fmt.Sprintf("%s_hnsw_s%d", documentTable(generation), space)
}

func generationStatements(schema string, generation GenerationID) []string {
	document := quoteIdentifier(documentTable(generation))
	facet := quoteIdentifier(facetTable(generation))
	session := quoteIdentifier(sessionTable(generation))
	event := quoteIdentifier(eventTable(generation))
	evidence := quoteIdentifier(evidenceTable(generation))
	passage := quoteIdentifier(passageTable(generation))
	passageEvent := quoteIdentifier(passageEventTable(generation))
	limitation := quoteIdentifier(limitationTable(generation))
	relation := quoteIdentifier(relationTable(generation))
	return []string{
		fmt.Sprintf(`CREATE TABLE %s.%s (
	id bigint PRIMARY KEY,
	session_key text NOT NULL UNIQUE,
	harness text NOT NULL,
	native_id text NOT NULL,
	title text NOT NULL,
	source_id text NOT NULL,
	occurrence_id text NOT NULL,
	discovery_revision text NOT NULL,
	revision_hash bytea NOT NULL,
	source_revision_kind text NOT NULL,
	source_revision_value text NOT NULL,
	locator_kind text NOT NULL,
	locator_root text NOT NULL,
	locator_path text NOT NULL,
	started_at timestamptz,
	updated_at timestamptz
)`, schema, session),
		fmt.Sprintf(`CREATE TABLE %s.%s (
	id bigint PRIMARY KEY,
	session_id bigint NOT NULL REFERENCES %s.%s (id),
	ordinal integer NOT NULL,
	event_key text NOT NULL,
	kind text NOT NULL,
	role text NOT NULL,
	observation_id text NOT NULL,
	occurred_at timestamptz
)`, schema, event, schema, session),
		fmt.Sprintf(`CREATE TABLE %s.%s (
	id bigint PRIMARY KEY,
	event_id bigint NOT NULL REFERENCES %s.%s (id),
	position integer NOT NULL,
	observation_id text NOT NULL,
	locator_kind text NOT NULL,
	locator_root text NOT NULL,
	locator_path text NOT NULL,
	locator_record bigint,
	locator_line bigint,
	byte_start bigint,
	byte_end bigint
)`, schema, evidence, schema, event),
		// A relation whose target is another session's native identity or
		// observation is resolved after retention, from the revisions retained
		// in this same generation, never by reopening a peer transcript.
		fmt.Sprintf(`CREATE TABLE %s.%s (
	id bigint PRIMARY KEY,
	session_id bigint NOT NULL REFERENCES %s.%s (id),
	ordinal integer NOT NULL,
	kind text NOT NULL,
	origin text NOT NULL,
	from_kind text NOT NULL,
	from_ref text NOT NULL,
	to_kind text NOT NULL,
	to_ref text NOT NULL,
	resolved_kind text,
	resolved_ref text,
	observation_id text NOT NULL
)`, schema, relation, schema, session),
		fmt.Sprintf(`CREATE TABLE %s.%s (
	id bigint PRIMARY KEY,
	session_id bigint NOT NULL REFERENCES %s.%s (id),
	ordinal integer NOT NULL,
	kind text NOT NULL,
	builder_version text NOT NULL,
	part integer NOT NULL,
	parts integer NOT NULL CHECK (parts > 0),
	started_at timestamptz
)`, schema, passage, schema, session),
		fmt.Sprintf(`CREATE TABLE %s.%s (
	passage_id bigint NOT NULL REFERENCES %s.%s (id),
	event_id bigint NOT NULL REFERENCES %s.%s (id),
	position integer NOT NULL,
	PRIMARY KEY (passage_id, position)
)`, schema, passageEvent, schema, passage, schema, event),
		// doc_id is supplied by the scan writer and generated for substrate rows.
		fmt.Sprintf(`CREATE TABLE %s.%s (
	doc_id bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
	session_ref text NOT NULL,
	harness text NOT NULL,
	passage_id bigint REFERENCES %s.%s (id),
	projection_kind text NOT NULL DEFAULT '%s',
	projection_version text NOT NULL DEFAULT '',
	body text NOT NULL,
	content_hash bytea NOT NULL,
	embedding_space_id bigint REFERENCES %s.embedding_space (id),
	embedding vector
)`, schema, document, schema, passage, ProjectionKindLexical, schema),
		// A projection row is byte-exact unless it has a limitation row here, so
		// the table stays empty for a corpus without NUL bytes.
		fmt.Sprintf(`CREATE TABLE %s.%s (
	doc_id bigint NOT NULL REFERENCES %s.%s (doc_id),
	kind text NOT NULL,
	removed_bytes bigint NOT NULL CHECK (removed_bytes > 0),
	PRIMARY KEY (doc_id, kind)
)`, schema, limitation, schema, document),
		fmt.Sprintf(`CREATE TABLE %s.%s (
	doc_id bigint NOT NULL REFERENCES %s.%s (doc_id),
	namespace text NOT NULL,
	"key" text NOT NULL,
	value text NOT NULL
)`, schema, facet, schema, document),
		fmt.Sprintf(
			`CREATE INDEX ON %s.%s (doc_id)`,
			schema,
			facet,
		),
		fmt.Sprintf(
			`CREATE INDEX ON %s.%s (namespace, "key", value)`,
			schema,
			facet,
		),
		// Reuse copies a session by joining its projections through passage_id;
		// without this index every copied session sequentially scans the whole
		// document table.
		fmt.Sprintf(`CREATE INDEX ON %s.%s (passage_id)`, schema, document),
		fmt.Sprintf(`CREATE INDEX ON %s.%s (session_id, ordinal)`, schema, event),
		fmt.Sprintf(`CREATE INDEX ON %s.%s (observation_id)`, schema, event),
		fmt.Sprintf(`CREATE INDEX ON %s.%s (native_id)`, schema, session),
		fmt.Sprintf(`CREATE INDEX ON %s.%s (event_id, position)`, schema, evidence),
		fmt.Sprintf(`CREATE INDEX ON %s.%s (session_id, ordinal)`, schema, passage),
		fmt.Sprintf(`CREATE INDEX ON %s.%s (session_id, ordinal)`, schema, relation),
	}
}

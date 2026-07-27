package catalog

import (
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/nikitatsym/agent-session-io/internal/config"
)

// Revision is the single current draft revision of the catalog schema.
const Revision = 2

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

// generationTables are dropped by Cleanup in dependency order.
func generationTables(generation GenerationID) []string {
	return []string{
		facetTable(generation),
		limitationTable(generation),
		documentTable(generation),
		passageEventTable(generation),
		passageTable(generation),
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
		fmt.Sprintf(`CREATE TABLE %s.%s (
	id bigint PRIMARY KEY,
	session_id bigint NOT NULL REFERENCES %s.%s (id),
	ordinal integer NOT NULL,
	kind text NOT NULL,
	builder_version text NOT NULL,
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
		fmt.Sprintf(`CREATE INDEX ON %s.%s (session_id, ordinal)`, schema, event),
		fmt.Sprintf(`CREATE INDEX ON %s.%s (event_id, position)`, schema, evidence),
		fmt.Sprintf(`CREATE INDEX ON %s.%s (session_id, ordinal)`, schema, passage),
	}
}

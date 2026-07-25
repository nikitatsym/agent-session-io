package catalog

import (
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/nikitatsym/agent-session-io/internal/config"
)

// Revision is the single current draft revision of the catalog schema.
const Revision = 1

// SupportedPostgresMajor is the only accepted PostgreSQL major version.
const SupportedPostgresMajor = 18

// BM25TextConfig is fixed at review from the Step 2 measurement evidence:
// pg_catalog.russian stems Cyrillic and English words and drops both stopword
// sets, so it is the only measured configuration with mixed-corpus recall.
const BM25TextConfig = "russian"

// TrigramIndex is fixed at review from the Step 2 measurement evidence: the
// planner never chose gist at realistic selectivity and gin needs no recheck.
const TrigramIndex = "gin"

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
	return []string{
		fmt.Sprintf(`CREATE TABLE %s.%s (
	doc_id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	session_ref text NOT NULL,
	harness text NOT NULL,
	body text NOT NULL,
	content_hash bytea NOT NULL,
	embedding_space_id bigint REFERENCES %s.embedding_space (id),
	embedding vector
)`, schema, document, schema),
		fmt.Sprintf(`CREATE TABLE %s.%s (
	doc_id bigint NOT NULL REFERENCES %s.%s (doc_id),
	namespace text NOT NULL,
	"key" text NOT NULL,
	value text NOT NULL
)`, schema, facet, schema, document),
	}
}

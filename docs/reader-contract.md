# Reader contract
<!-- tackbox: chars=ascii -->

The reader discovers and streams source-native coding-agent session data
without requiring a catalog, search index, model provider, or background
service.

This document defines behavioral invariants. Exact Go type names may evolve
within the module compatibility policy, but implementations must preserve
these semantics.

## Scope

The first complete adapters are Codex and Claude Code. OMP and OpenCode
fixtures challenge the common model before their adapters become completeness
gates.

The initial reader covers:

- source discovery;
- distinct source-occurrence and native-session identity;
- streaming source-native observations;
- normalized events and structural relations;
- source fidelity, limitations, and provenance;
- versioned JSON and NDJSON export.

Catalog storage, annotations, search, embeddings, and generated analysis
compose the reader later and are not reader prerequisites.

## Identity

A source occurrence is one observed file, database, or other native container.
Copied containers, repeated native session IDs, and equal content remain
distinct occurrences by default.

A session reference identifies one native session within one source
occurrence. Native identity is preserved as a fact but is not a globally
unique sessionio identity.

Session listing returns one row per source occurrence. It may expose native
relationship hints, but it does not group copies, archives, or repeated native
IDs automatically.

Content-addressed storage may deduplicate retained bytes later. It must not
merge source occurrences, session references, provenance, or annotation
subjects.

Launch directories, repositories, and semantic projects are separate facts.
The reader never treats a directory as a semantic project.

## Source-native observations

A source-native observation is the smallest independently locatable unit
provided by a source:

- a complete JSONL record;
- a database row or append-only database event read in one consistent
  transaction;
- an external native blob referenced by another observation;
- another adapter-declared native unit.

The reader exposes the source-native representation before normalization.
Normalization may produce zero or more events and relations from one native
observation, but it never replaces that observation.

Each read item carries:

- source occurrence and session reference;
- source locator;
- source revision;
- native kind and optional schema or record version;
- native representation;
- capture kind and source limitations;
- normalized events and relations;
- adapter identity and version.

## Capture and source limitations

Capture kind and source completeness are separate axes.

`byte_exact` means sessionio exposes the exact source bytes for the native
unit, including framing information needed to reproduce it.

`structured_snapshot` means the source has no stable per-record byte sequence.
Sessionio exposes native logical values read at one consistent source
revision. Raw JSON column values remain byte-exact where the source provides
them, but sessionio does not claim an original serialized row that never
existed.

Source limitations describe facts such as:

- the harness truncated a value before persistence;
- payload data was externalized to a referenced blob;
- the referenced blob is unavailable;
- only a mutable materialized state is available;
- a capability is absent from the native source.

Byte-exact capture does not imply that the harness persisted every value seen
during live execution.

## Provenance and revisions

A locator identifies a native unit without pretending every source is a
file. File-backed locators include a root-relative path, line or record
ordinal, and byte range. Database-backed locators include database identity,
table or event stream, native keys, and transaction or event-sequence
revision.

Every normalized event and relation references its source-native evidence.
Derived IDs remain stable only while their documented identity inputs remain
stable.

The reader records observed paths. It does not hide moves, symlink aliases,
copies, or archived occurrences by silently reconciling them.

## Streaming behavior

Discovery and reading use pull-based streams that:

- accept cancellation;
- surface source and record context on every error;
- release file handles or database transactions through an explicit close;
- permit early termination without leaks;
- do not require a complete session or large tool result in memory.

An implementation may provide iterator helpers above this contract. Channels
are not the ownership boundary for reader resources.

## Normalized events

The initial event vocabulary covers:

- messages and rich content blocks;
- reasoning or thinking when the source exposes it;
- tool calls and tool results;
- usage and timing observations;
- environment, model, and repository facts;
- compaction, branch, continuation, and operational markers;
- unknown native observations.

The model does not require every harness to expose every event kind. Missing,
encrypted, summarized, externalized, and unavailable content remain distinct
states.

Operational records are not silently converted into chat messages. Parallel
native representations of one action remain separate observations and may be
linked by deterministic relations.

## Structural relations

The first relation vocabulary includes:

- previous and next within a native ordering;
- native reply or record parent;
- native session branch or fork parent;
- containment of content blocks or parts;
- tool call and tool result;
- materialization or update when a source exposes both history and current
  state.

Relations record whether they are native or deterministically derived.
Semantic relations inferred by a model are outside the reader milestone.

## Unknown and malformed data

A well-formed unknown record kind is returned as an explicit unknown native
event with its exact native representation and a capability diagnostic.

A malformed complete record, known-format violation, or unsupported declared
container version fails with source occurrence, locator, and adapter-version
context. It is not skipped.

An incomplete final record in a concurrently growing append source is pending,
not malformed. The reader retries from the previous committed checkpoint.
Malformed interior records fail immediately.

Adapters detect truncation, atomic replacement, and source disappearance
instead of assuming every source is append-only.

## Initial source boundaries

The Codex adapter discovers active and archived rollout files. Its SQLite
state is auxiliary and is not a lossless transcript source.

The Claude Code adapter discovers primary project transcripts and subagent
transcripts. Command history, mutable process state, shell snapshots, and
file-history stores are separate auxiliary capabilities and are not silently
merged into canonical transcripts. Discovery reports known auxiliary sources
with an explicit capability and status even when canonical import is disabled.

OMP and OpenCode initially provide synthetic compatibility fixtures. Their
known constraints remain part of contract tests:

- OMP has parent-linked entry trees, active leaves, external blobs, and
  atomic rewrites;
- OpenCode uses mutable SQLite materializations, rich parts, an event stream,
  WAL-aware transactions, and schema migrations.

## Machine output

JSON uses a versioned envelope. Each NDJSON record is self-describing and
contains a schema identifier, record kind, and producer version.

Diagnostics and progress go to stderr. Machine records go to stdout.

Human `show` output uses normalized events by default. Native observations and
full provenance remain available through explicit detail options.

Before 1.0, an incompatible serialized change creates a new explicit schema
version. It does not silently change an existing schema.

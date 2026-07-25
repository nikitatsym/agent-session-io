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

## Runtime presence

Runtime presence is an ephemeral snapshot layered over persisted reader
occurrences. It does not change reader identity, merge copies, or make live
state canonical.

`sessionio list --current` groups validated live observations by
`(harness, native_session_id)` and retains every persisted occurrence with
that native identity. It resolves a selector only when there is one candidate
or one exact runtime locator. Otherwise the group is explicitly ambiguous and
has no representative occurrence. No title, path similarity, working
directory, timestamp, or recency heuristic may resolve ambiguity.

An exact-locator observation identifies one occurrence but does not remove
other copied occurrences from the group. If runtime state claims a native
identity for which no persisted occurrence was discovered, the process is
reported as unmatched. One process may support several native-session groups,
and one group may contain several process instances.

`--current=exact` removes probable observations and their process instances
before groups and selections are rebuilt. `--current` cannot be combined with
`--since` or `--until`: filtering persisted occurrences by historical activity
could otherwise turn a known live session into an artificial unmatched
process.

Presence providers report exact and probable capability status separately as
`supported`, `unavailable`, or `unsupported`. Expected conditions such as a
missing prerequisite, an inaccessible process, an authentication requirement,
or a native/WSL boundary are typed snapshot data. Malformed state owned by a
validated live process fails the observation instead of being skipped.

The initial production providers cover the registered Codex and Claude
adapters:

- Codex exact presence is a validated same-user Codex process holding the
  exact rollout path already discovered and validated by the adapter. It does
  not require `state_5.sqlite`.
- Claude exact presence joins a validated same-user Claude process to
  `.claude/sessions/<pid>.json` using both PID and process start time, then
  uses its native session ID. A stale or reused PID is not matched.

macOS and Linux use same-user process inspection plus `lsof` for exact file
ownership and loopback listener ownership. Windows uses process tokens and
creation times, Restart Manager for exact file ownership, and the owner-PID
TCP tables for loopback listeners. Native Windows and WSL are separate
execution boundaries; the platform itself is not treated as unsupported.

Presence inspection does not read target-process argv or environment and does
not scan arbitrary ports.

## Identity

A source occurrence is one observed file, database, or other native container.
Copied containers, repeated native session IDs, and equal content remain
distinct occurrences by default.

A session reference identifies one native session within one source
occurrence. Native identity is preserved as a fact but is not a globally
unique sessionio identity.

`SessionRef.Title` is optional observed source metadata, not generated
analysis. Claude Code selects the last non-empty `custom-title`, otherwise
the last non-empty `ai-title`; `last-prompt` is never a title.

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
- a fresh discovery revision and an immutable acquired source revision;
- native kind and optional schema or record version;
- native representation;
- capture kind and source limitations;
- normalized events and relations.

The selected adapter descriptor supplies the harness identity, adapter
version, and capability matrix for the stream. A read item preserves the
harness again through its source occurrence; it does not repeat the adapter
version.

## Capture and source limitations

Capture kind and source completeness are separate axes.

`byte_exact` means sessionio exposes the exact source bytes for the native
unit, including framing information needed to reproduce it.

`structured_snapshot` means the source has no stable per-record byte sequence.
Sessionio exposes native logical values read at one consistent source
revision. Raw JSON column values remain byte-exact where the source provides
them, but sessionio does not claim an original serialized row that never
existed.

`decoded_stream` means sessionio exposes exact decoded record data and
framing. Its non-empty codec identifies the physical compressed container;
recombining decoded records does not recreate that container.

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

Discovery revisions are cheap non-authoritative change hints returned by
listing and refreshed when an occurrence is opened. A previously listed
session reference remains a valid read selector. Native observations carry
the authoritative immutable revision of the acquired generation.

## Streaming behavior

Discovery and reading use pull-based streams that:

- accept cancellation;
- surface source and record context on every error;
- release file handles or database transactions through an explicit close;
- permit early termination without leaks;
- do not require a complete session in memory.

The initial byte-exact model stores one native observation in `Data []byte`,
so a reader materializes one observation at a time. File adapters choose an
explicit per-record size policy and fail with source and record context when
the configured limit is exceeded. Shared framing code has no implicit scanner
token limit.

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

`active_leaf` is a deterministic `session -> observation` projection for a
source-selected leaf of a persisted branch tree. It does not replace native
`reply_to` parent links.

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

File-backed JSONL reads use a size-bounded generation from an opened handle.
The generation revision is the SHA-256 of every bounded byte, including exact
line framing and a pending tail. Complete records are indexed first and their
bytes are verified before return. Bytes appended past the generation boundary
belong to the next generation.

Decoded JSONL generations read and decode a bounded physical container twice
without a decoded transcript spool. The acquisition pass retains physical
SHA-256 plus decoded-record digests. The emission pass verifies each decoded
record before return and verifies physical identity, byte count, and full
container SHA-256 before clean EOF. A late physical-only change may therefore
follow already verified records with a terminal error, but never with clean
completion.

Growing sources do not emit any unterminated tail, even when its current bytes
form valid JSON. A final source may emit a valid unterminated last record with
empty framing. Record checkpoints advance through framing only after the
complete record has been returned.

Container change and checkpoint safety are separate results. A rewritten or
replaced container may retain a byte-safe confirmed prefix, while an altered
confirmed prefix requires replay.

Adapters detect truncation, atomic replacement, and source disappearance
instead of assuming every source is append-only.

## Initial source boundaries

The Codex adapter discovers active `sessions/YYYY/MM/DD/rollout-*.jsonl` and
archived `archived_sessions/rollout-*.jsonl` occurrences, including `.jsonl.zst`
variants. A configured home is one canonical source and uses its resolved
absolute literal path as provenance. Active plain files use growing-tail
semantics; archived files and compressed occurrences are final. A compressed
locator identifies the physical `.jsonl.zst` container plus decoded record and
line ordinals, never a byte range. Compressed observations use `decoded_stream`
capture with codec `zstd`, decoded framing, and a physical-container SHA-256
revision. A plain sibling wins over a compressed sibling.
Codex metadata preserves separate session identity, fork and control-parent
hints, agent metadata, and history bounds.
Session listing decodes only the first complete metadata record; full
container validation and malformed-interior detection occur when that
occurrence is read.

The Claude Code adapter discovers primary project transcripts, direct
subagents, and workflow subagents under `projects/`. Adjacent agent metadata
sidecars are canonical byte-exact evidence and precede transcript observations
when present. Workflow journals, command history, mutable process state, shell
snapshots, task state, session environment, and file-history stores are
separate auxiliary sources and are not silently merged into canonical
transcripts. Discovery reports known auxiliary sources with an explicit
capability and status even when canonical import is disabled.

Claude tool results may name external persisted output. The adapter retains
the reference and reports whether that payload exists, but does not import its
bytes in the reader milestone.

OMP and OpenCode initially provide synthetic compatibility fixtures. Their
known constraints remain part of contract tests:

- OMP has parent-linked entry trees, active leaves, external blobs, and
  atomic rewrites;
- OpenCode uses mutable SQLite materializations, rich parts, an event stream,
  WAL-aware transactions, and schema migrations.

## Machine output

JSON uses a versioned envelope. Each NDJSON record is self-describing and
contains a schema identifier, record kind, and producer version.

Runtime presence uses a separate `sessionio.presence/v1` envelope containing
one atomic, time-bounded snapshot. Its provider capabilities, matches,
occurrences, selections, process identities, evidence, and unmatched
processes must be interpreted together. JSON and NDJSON each encode the whole
snapshot; presence records are not `sessionio.reader/v1` records.

Diagnostics and progress go to stderr. Machine records go to stdout.

Human `show` output uses normalized events by default. Native observations and
full provenance remain available through explicit detail options.

`sessionio.reader/v1` is the current draft schema. Until the repository makes
an explicit contract-freeze decision, incompatible reader changes update it
in place without compatibility shims.

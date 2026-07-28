// Package readercache retains an advisory per-source listing cache, so a warm
// listing reopens no source container whose stat identity is unchanged. Every
// operation is best effort: an absent, stale, corrupt, or unwritable cache
// changes no output byte and fails no command.
package readercache

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	sessionio "github.com/nikitatsym/agent-session-io"
)

// Schema versions every record of a cache file. A record that does not carry
// it discards the whole file.
const Schema = "sessionio.readercache/v1"

const (
	kindSource = "source"
	kindEntry  = "entry"
)

// maxCacheBytes bounds one cache file, so a corrupt size never decodes.
const maxCacheBytes = 1 << 30

// Diagnostic reports what the advisory cache could not do. It is never a
// command failure.
type Diagnostic struct {
	Path    string
	Message string
	Cause   error
}

func (diagnostic Diagnostic) String() string {
	return fmt.Sprintf(
		"reader cache: %s %s: %v",
		diagnostic.Message,
		diagnostic.Path,
		diagnostic.Cause,
	)
}

func discardedFile(path string, cause error) Diagnostic {
	return Diagnostic{Path: path, Message: "discarded", Cause: cause}
}

func unwritableFile(path string, cause error) Diagnostic {
	return Diagnostic{Path: path, Message: "could not write", Cause: cause}
}

// Settings selects the cache directory and whether the cache is used at all.
type Settings struct {
	Dir     string
	Enabled bool
}

// Store owns one cache file per source under one directory.
type Store struct {
	mutex       sync.Mutex
	settings    Settings
	sources     map[string]*sourceCache
	diagnostics []Diagnostic
}

// Open never fails: a store that cannot be used is a store that caches nothing.
func Open(settings Settings) *Store {
	if settings.Dir == "" {
		settings.Enabled = false
	}
	return &Store{settings: settings, sources: map[string]*sourceCache{}}
}

// Enabled reports whether this store reads and writes cache files.
func (store *Store) Enabled() bool {
	return store != nil && store.settings.Enabled
}

// ListingCache returns the advisory cache of one source, absent while the
// cache is disabled.
func (store *Store) ListingCache(id string) (sessionio.ListingCache, bool) {
	source, found := store.source(id)
	if !found {
		return nil, false
	}
	return source, true
}

func (store *Store) source(id string) (*sourceCache, bool) {
	if !store.Enabled() || id == "" {
		return nil, false
	}
	store.mutex.Lock()
	defer store.mutex.Unlock()
	if existing, found := store.sources[id]; found {
		return existing, true
	}
	source := &sourceCache{
		store:   store,
		id:      id,
		path:    filepath.Join(store.settings.Dir, fileName(id)),
		entries: map[string]*record{},
		touched: map[string]struct{}{},
	}
	store.sources[id] = source
	return source, true
}

// Activity returns the retained resolved activity of one occurrence. The
// discovery revision is the validity token: a changed occurrence never reuses
// the activity resolved for its previous revision.
func (store *Store) Activity(
	sourceID string,
	key string,
	revision string,
) (*time.Time, bool) {
	source, found := store.source(sourceID)
	if !found || revision == "" {
		return nil, false
	}
	source.mutex.Lock()
	defer source.mutex.Unlock()
	source.load()
	entry, present := source.entries[key]
	if !present || !entry.ActivityResolved || entry.Revision != revision {
		return nil, false
	}
	return cloneTime(entry.Activity), true
}

// RetainActivity stores the activity resolved for one occurrence revision.
func (store *Store) RetainActivity(
	sourceID string,
	key string,
	revision string,
	activity *time.Time,
) {
	source, found := store.source(sourceID)
	if !found || revision == "" {
		return
	}
	source.mutex.Lock()
	defer source.mutex.Unlock()
	source.load()
	entry, present := source.entries[key]
	if !present {
		return
	}
	entry.Revision = revision
	entry.Activity = cloneTime(activity)
	entry.ActivityResolved = true
	source.dirty = true
}

// Diagnostics drains what the store could not do.
func (store *Store) Diagnostics() []Diagnostic {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	drained := store.diagnostics
	store.diagnostics = nil
	return drained
}

func (store *Store) report(diagnostic Diagnostic) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	store.diagnostics = append(store.diagnostics, diagnostic)
}

// Flush writes every source file this process changed. A rewritten file holds
// exactly the entries this process listed, so an occurrence that disappeared
// leaves no entry behind.
func (store *Store) Flush() {
	if !store.Enabled() {
		return
	}
	store.mutex.Lock()
	sources := make([]*sourceCache, 0, len(store.sources))
	for _, source := range store.sources {
		sources = append(sources, source)
	}
	store.mutex.Unlock()
	sort.Slice(sources, func(left, right int) bool {
		return sources[left].id < sources[right].id
	})
	for _, source := range sources {
		source.flush()
	}
}

// sourceCache is the advisory listing cache of exactly one source.
type sourceCache struct {
	mutex   sync.Mutex
	store   *Store
	id      string
	path    string
	loaded  bool
	used    bool
	dirty   bool
	entries map[string]*record
	touched map[string]struct{}
}

// Lookup returns the retained listing record while its stamp is unchanged.
func (source *sourceCache) Lookup(
	key string,
	stamp string,
) (sessionio.SessionRef, bool) {
	source.mutex.Lock()
	defer source.mutex.Unlock()
	source.load()
	source.used = true
	entry, found := source.entries[key]
	if !found || entry.Stamp != stamp || entry.Session == nil {
		return sessionio.SessionRef{}, false
	}
	source.touched[key] = struct{}{}
	return cloneRef(*entry.Session), true
}

// Retain stores the listing record that key produced under stamp.
func (source *sourceCache) Retain(
	key string,
	stamp string,
	ref sessionio.SessionRef,
) {
	source.mutex.Lock()
	defer source.mutex.Unlock()
	source.load()
	source.used = true
	retained := cloneRef(ref)
	source.entries[key] = &record{
		Schema:  Schema,
		Kind:    kindEntry,
		Key:     key,
		Stamp:   stamp,
		Session: &retained,
	}
	source.touched[key] = struct{}{}
	source.dirty = true
}

// load reads the cache file once. Any problem discards the whole file.
func (source *sourceCache) load() {
	if source.loaded {
		return
	}
	source.loaded = true
	entries, err := readFile(source.path)
	if err == nil {
		source.entries = entries
		return
	}
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	source.store.report(discardedFile(source.path, err))
	// The retained file is unusable, so the next flush must replace it.
	source.dirty = true
}

func (source *sourceCache) flush() {
	source.mutex.Lock()
	defer source.mutex.Unlock()
	if !source.used {
		return
	}
	stale := len(source.entries) != len(source.touched)
	if !source.dirty && !stale {
		return
	}
	keys := make([]string, 0, len(source.touched))
	for key := range source.touched {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	records := make([]*record, 0, len(keys)+1)
	records = append(records, &record{
		Schema: Schema,
		Kind:   kindSource,
		Source: source.id,
	})
	for _, key := range keys {
		records = append(records, source.entries[key])
	}
	err := writeFile(source.path, records)
	if err == nil {
		return
	}
	source.store.report(unwritableFile(source.path, err))
}

type record struct {
	Schema           string                `json:"schema"`
	Kind             string                `json:"kind"`
	Source           string                `json:"source,omitempty"`
	Key              string                `json:"key,omitempty"`
	Stamp            string                `json:"stamp,omitempty"`
	Session          *sessionio.SessionRef `json:"session,omitempty"`
	Revision         string                `json:"revision,omitempty"`
	Activity         *time.Time            `json:"activity,omitempty"`
	ActivityResolved bool                  `json:"activity_resolved,omitempty"`
}

func readFile(path string) (map[string]*record, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxCacheBytes {
		return nil, fmt.Errorf("cache file is %d bytes", info.Size())
	}
	entries := map[string]*record{}
	decoder := json.NewDecoder(bufio.NewReader(io.LimitReader(file, maxCacheBytes)))
	header := false
	for {
		var decoded record
		decodeErr := decoder.Decode(&decoded)
		if errors.Is(decodeErr, io.EOF) {
			break
		}
		if decodeErr != nil {
			return nil, decodeErr
		}
		if decoded.Schema != Schema {
			return nil, fmt.Errorf(
				"record schema %q is not %q",
				decoded.Schema,
				Schema,
			)
		}
		if !header {
			if decoded.Kind != kindSource {
				return nil, fmt.Errorf("first record is a %q record", decoded.Kind)
			}
			header = true
			continue
		}
		if decoded.Kind != kindEntry || decoded.Key == "" || decoded.Session == nil {
			return nil, fmt.Errorf("unusable %q record", decoded.Kind)
		}
		stored := decoded
		entries[decoded.Key] = &stored
	}
	if !header {
		return nil, errors.New("cache file carries no source record")
	}
	return entries, nil
}

// writeFile replaces the cache file through a temporary file and a rename, so
// a concurrent reader never observes a torn file.
func writeFile(path string, records []*record) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	if err := encodeRecords(temporary, records); err != nil {
		return errors.Join(err, temporary.Close(), os.Remove(name))
	}
	if err := temporary.Close(); err != nil {
		return errors.Join(err, os.Remove(name))
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return errors.Join(err, os.Remove(name))
	}
	if err := os.Rename(name, path); err != nil {
		return errors.Join(err, os.Remove(name))
	}
	return nil
}

func encodeRecords(file *os.File, records []*record) error {
	writer := bufio.NewWriter(file)
	encoder := json.NewEncoder(writer)
	for _, value := range records {
		if err := encoder.Encode(value); err != nil {
			return err
		}
	}
	return writer.Flush()
}

// fileName keeps a cache file addressable on every platform: a source ID
// carries separators that Windows rejects in a file name.
func fileName(id string) string {
	var builder strings.Builder
	for _, character := range id {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '-', character == '_':
			builder.WriteRune(character)
		default:
			builder.WriteByte('-')
		}
	}
	return builder.String() + ".ndjson"
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return value
	}
	copied := *value
	return &copied
}

// cloneRef keeps a retained record independent of the caller, so a consumer
// that mutates a listing record cannot poison the next lookup.
func cloneRef(ref sessionio.SessionRef) sessionio.SessionRef {
	ref.StartedAt = cloneTime(ref.StartedAt)
	ref.UpdatedAt = cloneTime(ref.UpdatedAt)
	ref.Diagnostics = append([]sessionio.Diagnostic(nil), ref.Diagnostics...)
	ref.Native.Identities = append(
		[]sessionio.NativeIdentity(nil),
		ref.Native.Identities...,
	)
	ref.Native.Relationships = append(
		[]sessionio.NativeRelationshipHint(nil),
		ref.Native.Relationships...,
	)
	return ref
}

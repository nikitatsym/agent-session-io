package sessionio

// ListingCache is an advisory per-source cache of session listing records. It
// never changes what an adapter lists: a miss, a changed stamp, or an
// unusable cache only costs the source read that produced the record before.
//
// The stamp is opaque to the cache and is built by the adapter from the size,
// modification time, and file identity of every container the listing record
// was read from.
type ListingCache interface {
	// Lookup returns the retained listing record for key while stamp matches.
	Lookup(key string, stamp string) (SessionRef, bool)
	// Retain stores the listing record that key produced under stamp.
	Retain(key string, stamp string, ref SessionRef)
}

// ListingCacheSource hands an adapter the advisory cache of one source. An
// adapter resolves its own source identity, so the cache file of one source
// never serves another.
type ListingCacheSource interface {
	ListingCache(sourceID string) (ListingCache, bool)
}

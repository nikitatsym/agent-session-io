// Package fileid produces a persistable file identity, so a later run can tell
// an in-place rewrite from an atomic replacement at the same path.
package fileid

// Unavailable is the identity of a platform that exposes no file identity.
// A rewrite and an atomic replacement are then indistinguishable.
const Unavailable = ""

// Token returns an opaque identity for the file at path.
func Token(path string) (string, error) {
	return token(path)
}

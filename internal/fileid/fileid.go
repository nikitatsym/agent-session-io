// Package fileid produces a persistable file identity, so a later run can tell
// an in-place rewrite from an atomic replacement at the same path.
package fileid

import (
	"fmt"
	"os"
)

// Unavailable is the identity of a platform that exposes no file identity.
// A rewrite and an atomic replacement are then indistinguishable.
const Unavailable = ""

// Token returns an opaque identity for the file at path.
func Token(path string) (string, error) {
	return token(path)
}

// Stamp reports the size, modification time, and identity of the file at path.
// Two equal stamps mean the same container bytes, so a reader may reuse what it
// derived from them.
func Stamp(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	identity, err := Token(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"size=%d mtime=%d id=%s",
		info.Size(),
		info.ModTime().UnixNano(),
		identity,
	), nil
}

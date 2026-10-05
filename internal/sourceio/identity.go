package sourceio

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

// DerivedID frames identity inputs identically across source adapters.
func DerivedID(kind string, values ...string) string {
	hash := sha256.New()
	var length [4]byte
	write := func(value string) {
		binary.BigEndian.PutUint32(length[:], uint32(len(value)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(value))
	}
	write("sessionio/id/v1")
	write(kind)
	for _, value := range values {
		write(value)
	}
	return fmt.Sprintf("%s:sha256:%x", kind, hash.Sum(nil))
}

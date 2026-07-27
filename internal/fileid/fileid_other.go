//go:build !unix && !windows

package fileid

func token(string) (string, error) {
	return Unavailable, nil
}

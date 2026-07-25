//go:build !windows && !darwin && !linux

package runtimeprobe

func NewInspector() (Inspector, error) {
	return nil, ErrUnsupported
}

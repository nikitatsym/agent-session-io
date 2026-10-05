package omp

import (
	"path/filepath"
	"strings"

	sessionio "github.com/nikitatsym/agent-session-io"
)

func resolveRelationships(refs []sessionio.SessionRef) {
	byPath := map[string]string{}
	for _, ref := range refs {
		file := ref.Occurrence.Locator.File
		byPath[filepath.Clean(filepath.Join(file.Root, filepath.FromSlash(file.Path)))] = ref.NativeID
	}
	for index := range refs {
		ref := &refs[index]
		for hintIndex := range ref.Native.Relationships {
			hint := &ref.Native.Relationships[hintIndex]
			target := hint.TargetNativeID
			if !filepath.IsAbs(target) && !strings.Contains(target, "/") && !strings.Contains(target, "\\") {
				continue
			}
			path := target
			if !filepath.IsAbs(path) {
				path = filepath.Join(ref.Occurrence.Locator.File.Root, filepath.FromSlash(path))
			}
			if nativeID, found := byPath[filepath.Clean(path)]; found {
				hint.TargetNativeID = nativeID
			} else {
				ref.Diagnostics = append(ref.Diagnostics, diagnostic("omp_session_parent_unresolved", "opaque session parent path is not a discovered transcript", ref.Occurrence.Locator, nil))
			}
		}
	}
}

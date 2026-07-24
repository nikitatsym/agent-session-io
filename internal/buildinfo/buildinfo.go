package buildinfo

import "runtime/debug"

var (
	version = "dev"
	commit  = ""
	date    = ""
)

// Info describes the binary build.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	Date    string `json:"date,omitempty"`
	Dirty   bool   `json:"dirty"`
}

// Current returns linker-provided release data with Go build metadata as a
// fallback for binaries installed with go install.
func Current() Info {
	result := Info{
		Version: version,
		Commit:  commit,
		Date:    date,
	}

	goInfo, ok := debug.ReadBuildInfo()
	if !ok {
		return result
	}
	if result.Version == "dev" && goInfo.Main.Version != "" && goInfo.Main.Version != "(devel)" {
		result.Version = goInfo.Main.Version
	}
	for _, setting := range goInfo.Settings {
		switch setting.Key {
		case "vcs.revision":
			if result.Commit == "" {
				result.Commit = setting.Value
			}
		case "vcs.time":
			if result.Date == "" {
				result.Date = setting.Value
			}
		case "vcs.modified":
			result.Dirty = setting.Value == "true"
		}
	}
	return result
}

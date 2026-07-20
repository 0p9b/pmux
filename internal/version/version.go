package version

import "runtime/debug"

var (
	Version string
	Commit  string
	Date    string
)

type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	Date    string `json:"date,omitempty"`
}

func Current() Info {
	out := Info{Version: Version, Commit: Commit, Date: Date}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		if out.Version == "" {
			out.Version = "dev"
		}
		return out
	}
	if out.Version == "" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		out.Version = bi.Main.Version
	}
	for _, setting := range bi.Settings {
		switch setting.Key {
		case "vcs.revision":
			if out.Commit == "" {
				out.Commit = setting.Value
			}
		case "vcs.time":
			if out.Date == "" {
				out.Date = setting.Value
			}
		}
	}
	if out.Version == "" {
		out.Version = "dev"
	}
	return out
}

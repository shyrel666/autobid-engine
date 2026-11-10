// Package buildinfo reports Go's embedded build identity without linker flags.
package buildinfo

import (
	"runtime"
	"runtime/debug"
)

type Version struct {
	Revision  string `json:"revision"`
	Modified  bool   `json:"modified"`
	GoVersion string `json:"go_version"`
}

func Current() Version {
	v := Version{Revision: "unknown", GoVersion: runtime.Version()}
	if b, ok := debug.ReadBuildInfo(); ok {
		for _, s := range b.Settings {
			switch s.Key {
			case "vcs.revision":
				v.Revision = s.Value
			case "vcs.modified":
				v.Modified = s.Value == "true"
			}
		}
	}
	return v
}

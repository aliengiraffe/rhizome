package main

import (
	"fmt"
	"runtime/debug"
)

// Release builds set these through -ldflags -X. Local builds report version
// "dev" and take commit and date from the Go toolchain's VCS stamp when present.
var (
	version = ""
	commit  = ""
	date    = ""
)

// versionLine reports the build identity without touching configuration,
// runtime state, the network or an agent daemon.
func versionLine() string {
	v, c, d := version, commit, date
	if bi, ok := debug.ReadBuildInfo(); ok {
		var dirty bool
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if c == "" {
					c = s.Value
				}
			case "vcs.time":
				if d == "" {
					d = s.Value
				}
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if dirty && commit == "" && c != "" {
			c += "-dirty"
		}
	}
	if v == "" {
		v = "dev"
	}
	if c == "" {
		c = "unknown"
	}
	if d == "" {
		d = "unknown"
	}
	return fmt.Sprintf("rhizome %s (commit %s, built %s)", v, c, d)
}

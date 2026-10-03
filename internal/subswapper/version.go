package subswapper

import "runtime/debug"

// Version is the module version this binary was built from, as `go install`
// records it, or "(devel)" for a local build.
func Version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "unknown"
}

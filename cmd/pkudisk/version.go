package main

import "runtime/debug"

// GoReleaser sets version for distributed binaries. go install records the
// selected module version in the executable's build information instead.
var version = "dev"

func installedVersion() string {
	if version != "" && version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

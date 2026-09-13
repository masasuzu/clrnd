package cmd

import "runtime/debug"

// version is embedded at release build time through goreleaser's ldflags
// (-X github.com/masasuzu/clrnd/cmd.version=...).
var version = ""

// buildVersion resolves in the order embedded value > build info (via go install) > "(devel)".
func buildVersion() string {
	if version != "" {
		return version
	}
	// When installed through the module, as in go install github.com/masasuzu/clrnd@v1.2.3, the
	// build info carries the version. go build and other local builds do not.
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "(devel)"
}

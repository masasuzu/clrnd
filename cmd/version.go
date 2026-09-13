package cmd

import "runtime/debug"

// version is embedded at release build time through goreleaser's ldflags
// (-X github.com/masasuzu/clrnd/cmd.version=...).
var version = ""

// buildVersion resolves in the order embedded value > build info > "(devel)".
func buildVersion() string {
	if version != "" {
		return version
	}
	// When installed through the module, as in go install github.com/masasuzu/clrnd@v1.2.3, the
	// build info carries the version. Since Go 1.24, go build in a git checkout stamps one too (the
	// tag, or a pseudo-version with +dirty for uncommitted changes); a build without VCS
	// information still reports (devel).
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "(devel)"
}

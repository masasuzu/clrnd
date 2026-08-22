package cmd

import "runtime/debug"

// version はリリースビルド時に goreleaser の ldflags
// (-X github.com/masasuzu/clrnd/cmd.version=...) から埋め込まれる。
var version = ""

// buildVersion は埋め込み値 > ビルド情報 (go install 経由) > "(devel)" の順に解決する。
func buildVersion() string {
	if version != "" {
		return version
	}
	// go install github.com/masasuzu/clrnd@v1.2.3 のようにモジュール経由で入れた場合は
	// ビルド情報にバージョンが載る。go build やローカルビルドでは載らない。
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "(devel)"
}

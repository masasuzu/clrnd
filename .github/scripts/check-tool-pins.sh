#!/usr/bin/env bash
#
# 固定したツールのバージョンが、書かれているすべての場所で一致しているかを確かめる。
#
# Dependabot が見るのはアクションの SHA と go.mod だけで、アクションに渡す version
# 入力や go run ...@vX、自前で入れる ShellCheck の版までは追ってくれない。つまり
# これらは手で上げるしかなく、手で上げる以上どこかを直し忘れる。issue #73 がまさに
# それで、CI は latest を走らせているのにドキュメントは v2.6.2 と書いていた。
# 「ローカルと CI で同じ検査を回している」という前提が静かに崩れるので、ここで落とす。
set -euo pipefail

cd "$(dirname "$0")/../.."

status=0

# pin <ラベル> <値>... : すべて同じでなければエラーにする。空文字は「抽出できなかった」
# ことを意味するので、これも失敗として扱う (書式を変えて検査が素通りするのを防ぐ)。
pin() {
  local label=$1 first=$2 value
  shift
  for value in "$@"; do
    if [ -z "$value" ] || [ "$value" != "$first" ]; then
      printf '::error::%s: pinned versions disagree (or could not be read): %s\n' "$label" "$*"
      status=1
      return
    fi
  done
  printf 'ok  %-14s %s\n' "$label" "$first"
}

# version_in <ファイル> <grep -E の式> : 最初にマッチした行から x.y.z を取り出す。
version_in() {
  grep -hoE "$2" "$1" | head -n 1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' || true
}

# golangci-lint: CI (action の version 入力) と、README / CLAUDE.md の go run。
pin golangci-lint \
  "$(grep -A8 'golangci-lint-action@' .github/workflows/verify.yml |
    grep -hoE 'version: v?[0-9]+\.[0-9]+\.[0-9]+' | head -n 1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+')" \
  "$(version_in README.md 'golangci-lint@v[0-9]+\.[0-9]+\.[0-9]+')" \
  "$(version_in CLAUDE.md 'golangci-lint@v[0-9]+\.[0-9]+\.[0-9]+')"

# GoReleaser: PR のクロスビルド (verify.yml) と実際のリリース (release.yml) が同じ版で
# なければ、PR で通した設定と本番でビルドする GoReleaser が食い違う。
pin goreleaser \
  "$(grep -A8 'goreleaser-action@' .github/workflows/verify.yml |
    grep -hoE 'version: "[0-9]+\.[0-9]+\.[0-9]+"' | head -n 1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+')" \
  "$(grep -A8 'goreleaser-action@' .github/workflows/release.yml |
    grep -hoE 'version: "[0-9]+\.[0-9]+\.[0-9]+"' | head -n 1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+')" \
  "$(version_in README.md 'goreleaser/v2@v[0-9]+\.[0-9]+\.[0-9]+')"

# actionlint と ShellCheck: CI とローカル再現手順 (README) が同じ版を指しているか。
pin actionlint \
  "$(version_in .github/workflows/verify.yml 'actionlint@v[0-9]+\.[0-9]+\.[0-9]+')" \
  "$(version_in README.md 'actionlint@v[0-9]+\.[0-9]+\.[0-9]+')"

pin shellcheck \
  "$(version_in .github/workflows/verify.yml 'SHELLCHECK_VERSION: v[0-9]+\.[0-9]+\.[0-9]+')" \
  "$(version_in README.md 'ShellCheck v[0-9]+\.[0-9]+\.[0-9]+')"

exit "$status"

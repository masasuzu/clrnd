#!/usr/bin/env bash
#
# End-to-end test for clrnd against a real Cloud Run project.
#
# This is NOT part of `go test ./...`: it creates and deletes real Cloud Run
# services, needs Application Default Credentials, and cannot run in CI.
# Run it by hand when changing anything that talks to the Cloud Run API.
#
# Usage:
#   ./run.sh                       # run against the current working tree
#   ./run.sh --cleanup-orphans     # delete leftover clrnd-e2e-* services and exit
#   KEEP=1 ./run.sh                # keep the service instead of deleting it
#   OLD_REF=<git-ref> ./run.sh     # also build that ref and compare its behaviour
#   ONLY=current ./run.sh          # only the current-binary phase
#   ONLY=old OLD_REF=<ref> ./run.sh
#   PROJECT=<id> REGION=<region> ./run.sh
#   WORK_ROOT=<dir> ./run.sh       # parent of the build/scratch dir (default: $TMPDIR)
#
# The project ID is deliberately not stored in this script. Provide it via
# $PROJECT or a local (git-ignored) project.env file next to this script.
set -uo pipefail

# 解決に失敗したまま進むと HERE が空になり、WORK が /work を指してしまう
# (直後に rm -rf する)。ここで必ず止める。
HERE="$(cd "$(dirname "$0")" && pwd)" || { echo "error: cannot resolve the script directory" >&2; exit 1; }
REPO="${REPO:-$(cd "$HERE/../.." && pwd)}"
[ -n "$REPO" ] || { echo "error: cannot resolve the repository root" >&2; exit 1; }
# 成果物はリポジトリの外に置く。リポジトリがクラウド同期フォルダ (Dropbox, iCloud,
# OneDrive ...) の下にあると、同期クライアントがビルド中のバイナリを古い版に差し戻したり
# "conflicted copy" を作ったりする。そうなると古いバイナリを黙ってテストしてしまう。
#
# WORK_ROOT は「置き場所の親ディレクトリ」。実際に使うのは必ずその下の専用ディレクトリで、
# 消すのもそこだけにする (WORK 自体を上書き可能にすると、後段の rm -rf が利用者の
# 指定したディレクトリを丸ごと消してしまう)。
WORK_DIR_NAME="clrnd-e2e-work"
WORK_ROOT="${WORK_ROOT:-${TMPDIR:-/tmp}}"
WORK="${WORK_ROOT%/}/$WORK_DIR_NAME"
BIN="$WORK/bin"

# 使い捨てサービスの共通プレフィクス。--cleanup-orphans の対象でもある。
SERVICE_PREFIX="clrnd-e2e-"
SERVICE="${SERVICE_PREFIX}$(date +%Y%m%d%H%M%S)"

REGION="${REGION:-asia-northeast1}"
IMAGE="${IMAGE:-us-docker.pkg.dev/cloudrun/container/hello}"
# 過去の ref との比較は任意。指定が無ければフェーズ 2 をまるごと飛ばす。
OLD_REF="${OLD_REF:-}"

PASS=0
FAIL=0

# ---------- output helpers ----------
c() { printf '\033[%sm%s\033[0m\n' "$1" "$2"; }
step() { echo; c '1;36' "==== $* ===="; }
info() { echo "     $*"; }
ok()   { PASS=$((PASS + 1)); c '32' "  PASS  $*"; }
ng()   { FAIL=$((FAIL + 1)); c '31' "  FAIL  $*"; }
die()  { c '31' "error: $*"; exit 1; }

# ---------- project resolution ----------
# プロジェクト ID をスクリプト本文にも実行ログにも出さない (漏洩防止)。
resolve_project() {
  if [ -n "${PROJECT:-}" ]; then
    return
  fi
  if [ -f "$HERE/project.env" ]; then
    PROJECT="$(head -n1 "$HERE/project.env" | tr -d '[:space:]')"
  fi
  [ -n "${PROJECT:-}" ] || die "no project configured. Set \$PROJECT, or write the project ID into $HERE/project.env (git-ignored)."
}

# ---------- orphan cleanup ----------
delete_service() { # <name>
  gcloud run services delete "$1" --project "$PROJECT" --region "$REGION" --quiet >/dev/null 2>&1
}

cleanup_orphans() {
  step "Deleting leftover $SERVICE_PREFIX* services in $REGION"
  local names name found=0
  names="$(gcloud run services list --project "$PROJECT" --region "$REGION" \
    --format='value(metadata.name)' 2>/dev/null | grep "^$SERVICE_PREFIX" || true)"
  if [ -z "$names" ]; then
    info "nothing to delete"
    return 0
  fi
  while IFS= read -r name; do
    [ -n "$name" ] || continue
    found=1
    info "deleting $name"
    delete_service "$name" && info "  deleted" || c '31' "  failed to delete $name"
  done <<< "$names"
  [ "$found" -eq 1 ] || info "nothing to delete"
}

# 通常終了・失敗・Ctrl-C のいずれでも今回作ったサービスを消す。
# kill -9 では trap が動かないので、その場合は --cleanup-orphans を使う。
cleanup() {
  local rc=$?
  if [ "${KEEP:-}" = "1" ]; then
    step "Skipping cleanup (KEEP=1)"
    info "service left behind; remove it with: $0 --cleanup-orphans"
    return $rc
  fi
  step "Cleanup"
  if gcloud run services describe "$SERVICE" --project "$PROJECT" --region "$REGION" >/dev/null 2>&1; then
    info "deleting the test service"
    delete_service "$SERVICE" && info "deleted" \
      || c '31' "     failed to delete; run: $0 --cleanup-orphans"
  else
    info "nothing to delete"
  fi
  return $rc
}

# ---------- assertions ----------
OUT=""
RC=0

# run_cmd はコマンドを実行し、出力を OUT に、終了コードを RC に入れる (失敗しても止めない)。
run_cmd() {
  info "\$ $(basename "$1") ${*:2}"
  OUT="$("$@" 2>&1)"
  RC=$?
  [ -z "$OUT" ] || printf '%s\n' "$OUT" | sed 's/^/       | /'
  return 0
}

assert_rc_zero()    { if [ "$RC" -eq 0 ]; then ok "$1"; else ng "$1 (exit=$RC)"; fi; }
assert_empty()      { if [ -z "$OUT" ]; then ok "$1"; else ng "$1 (unexpected output)"; fi; }
assert_contains()   { if printf '%s' "$OUT" | grep -q -- "$2"; then ok "$1"; else ng "$1 (missing: $2)"; fi; }
assert_missing()    { if printf '%s' "$OUT" | grep -q -- "$2"; then ng "$1 (unexpected: $2)"; else ok "$1"; fi; }
assert_file_has()   { if grep -q -- "$3" "$2"; then ok "$1"; else ng "$1 (missing in $(basename "$2"): $3)"; fi; }
assert_file_lacks() { if grep -q -- "$3" "$2"; then ng "$1 (present in $(basename "$2"): $3)"; else ok "$1"; fi; }

# ---------- build ----------
# file_mtime <path> : 更新時刻を epoch 秒で返す (GNU/BSD stat の両対応)。
# GNU を先に試す。逆順にすると、GNU stat では -f がファイルシステム情報の指定になり
# %m を解釈できず "?" を exit 0 で返すため、フォールバックに到達しない。
# BSD stat は -c を知らずに exit != 0 で失敗するので、この順序なら両方で正しく動く。
file_mtime() {
  stat -c %Y "$1" 2>/dev/null || stat -f %m "$1" 2>/dev/null
}

# build_binary <dest> <srcdir> : ビルドし、出力が本当に更新されたかを確認する。
# 差し戻しやキャッシュで古いバイナリが残っていると、テストが嘘の結果を出すため。
build_binary() {
  local dest="$1" src="$2" before
  before="$(date +%s)"
  (cd "$src" && go build -o "$dest" .) || return 1
  [ -f "$dest" ] || { c '31' "     build produced no file at $dest"; return 1; }
  local mtime
  mtime="$(file_mtime "$dest")"
  case "$mtime" in
    ''|*[!0-9]*)
      # 検査できないまま素通りさせない (それでは保険にならない)。
      c '31' "     cannot read the mtime of $dest; refusing to trust the build"
      return 1
      ;;
  esac
  if [ "$mtime" -lt "$before" ]; then
    c '31' "     $dest was not rewritten by the build (stale binary?)"
    return 1
  fi
}

# ---------- Cloud Run helpers ----------
# ready_condition は Ready 条件を "<status>\t<reason>\t<message>" で返す。
ready_condition() {
  gcloud run services describe "$SERVICE" --project "$PROJECT" --region "$REGION" \
    --format=json 2>/dev/null | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    print("\t\t"); raise SystemExit
for c in (d.get("status") or {}).get("conditions") or []:
    if c.get("type") == "Ready":
        print("\t".join([c.get("status", ""), c.get("reason", ""), c.get("message", "")]))
        raise SystemExit
print("\t\t")
'
}

wait_ready() {
  local i line status reason message
  info "waiting for the revision to become ready (up to 180s)..."
  for i in $(seq 1 60); do
    line="$(ready_condition)"
    status="$(printf '%s' "$line" | cut -f1)"
    reason="$(printf '%s' "$line" | cut -f2)"
    message="$(printf '%s' "$line" | cut -f3)"
    case "$status" in
      True)  info "ready after ${i} checks"; return 0 ;;
      False) c '31' "     Ready=False reason=$reason"; c '31' "     $message"; return 1 ;;
    esac
    sleep 3
  done
  c '31' "     timed out (last status='$status')"
  return 1
}

current_revision() {
  gcloud run services describe "$SERVICE" --project "$PROJECT" --region "$REGION" \
    --format='value(status.latestReadyRevisionName)' 2>/dev/null
}

# write_manifest <path> [env-value]
write_manifest() {
  local path="$1" extra="${2:-}"
  {
    echo "apiVersion: serving.knative.dev/v1"
    echo "kind: Service"
    echo "metadata:"
    echo "  name: $SERVICE"
    echo "spec:"
    echo "  template:"
    echo "    spec:"
    echo "      containers:"
    echo "      - image: $IMAGE"
    if [ -n "$extra" ]; then
      echo "        env:"
      echo "        - name: CLRND_E2E"
      echo "          value: \"$extra\""
    fi
  } > "$path"
}

# set_env_value <manifest> <value> : CLRND_E2E を必ず <value> にする (無ければ追加)。
set_env_value() {
  python3 - "$1" "$2" <<'PY'
import re, sys
path, value = sys.argv[1], sys.argv[2]
s = open(path).read()
if "CLRND_E2E" in s:
    # 置換文字列に値を埋め込むと \1 などが展開されてしまうので、
    # 関数形式にして value を literal として扱う。
    s = re.sub(r'(name: CLRND_E2E\n\s+value: )\S+', lambda m: m.group(1) + value, s)
else:
    s = s.replace("      containers:\n      - image:",
                  "      containers:\n      - env:\n        - name: CLRND_E2E\n          value: %s\n        image:" % value)
open(path, "w").write(s)
PY
}

# pin_live_revision <suffix> : live サービス側にリビジョン名を固定させる。
# gcloud run deploy --revision-suffix は spec.template.metadata.name を設定する。
# Terraform の template.metadata.name も同じ状態を作る。リビジョン名を指定せずに
# 作ったサービスでは Cloud Run はこのフィールドを返さないので、init が名前を
# 引き継ぐ経路を試すにはこの前提を明示的に作る必要がある。
pin_live_revision() {
  gcloud run deploy "$SERVICE" --image "$IMAGE" --revision-suffix="$1" \
    --project "$PROJECT" --region "$REGION" --no-allow-unauthenticated --quiet >/dev/null 2>&1
}

# live_revision_name は live サービスが固定しているリビジョン名を返す (無ければ空)。
live_revision_name() {
  gcloud run services describe "$SERVICE" --project "$PROJECT" --region "$REGION" \
    --format='value(spec.template.metadata.name)' 2>/dev/null
}

# pin_revision <manifest> <revision-name> : spec.template.metadata.name を設定する。
# metadata ブロックが既に在る場合 (gcloud 由来のアノテーションが残っているときなど) は
# その中に name を足す。無ければ metadata ごと作る。
pin_revision() {
  python3 - "$1" "$2" <<'PY'
import sys
path, rev = sys.argv[1], sys.argv[2]
lines = open(path).read().split("\n")
out, i, done = [], 0, False
while i < len(lines):
    out.append(lines[i])
    if not done and lines[i] == "  template:":
        if i + 1 < len(lines) and lines[i + 1] == "    metadata:":
            out.append(lines[i + 1])            # 既存の metadata: を維持し
            out.append("      name: %s" % rev)  # その直下に name を挿す
            i += 1
        else:
            out.append("    metadata:")
            out.append("      name: %s" % rev)
        done = True
    i += 1
if not done:
    raise SystemExit("pin_revision: no 'spec.template:' block found in %s" % path)
open(path, "w").write("\n".join(out))
PY
}

# =====================================================================
resolve_project

if [ "${1:-}" = "--cleanup-orphans" ]; then
  trap - EXIT
  cleanup_orphans
  exit 0
fi

trap cleanup EXIT

step "Setup"
command -v gcloud >/dev/null || die "gcloud is not on PATH"
command -v go >/dev/null || die "go is not on PATH"
# 自分が作る専用ディレクトリ以外は絶対に消さない。
case "$WORK" in
  */"$WORK_DIR_NAME") ;;
  *) die "refusing to remove $WORK: not a $WORK_DIR_NAME directory" ;;
esac
rm -rf "$WORK"
mkdir -p "$BIN"
info "region  = $REGION"
info "repo    = $REPO"
info "service = ${SERVICE_PREFIX}<timestamp>"

info "work    = $WORK"
info "building the current binary ($(git -C "$REPO" rev-parse --abbrev-ref HEAD))..."
build_binary "$BIN/clrnd" "$REPO" || die "build failed"
CLRND="$BIN/clrnd"

if [ -n "$OLD_REF" ]; then
  info "building the comparison binary from $OLD_REF..."
  mkdir -p "$WORK/old-src"
  git -C "$REPO" archive "$OLD_REF" | tar -x -C "$WORK/old-src" || die "git archive $OLD_REF failed"
  build_binary "$BIN/clrnd-old" "$WORK/old-src" || die "build of $OLD_REF failed"
  OLD="$BIN/clrnd-old"
fi

export CLOUDSDK_CORE_PROJECT="$PROJECT"
export CLOUDSDK_RUN_REGION="$REGION"

# =====================================================================
if [ "${ONLY:-}" != "old" ]; then
step "Phase 1: current working tree"

D1="$WORK/current"; mkdir -p "$D1"; cd "$D1"

info "--- 1-1. create a new service ---"
write_manifest "$D1/manifest.yaml"
run_cmd "$CLRND" deploy "$SERVICE" "$D1/manifest.yaml" --auto-approve
assert_rc_zero "deploy creates a new service"
wait_ready || ng "the first deploy never became ready"

info "--- 1-1b. status ---"
run_cmd "$CLRND" status "$SERVICE"
assert_rc_zero "status succeeds"
assert_contains "status reports Ready=True" "Ready:           True"
assert_contains "status reports the service URL" "URL:             https://"
assert_contains "status reports the traffic split" "100%"
run_cmd "$CLRND" status "$SERVICE" --format json
assert_rc_zero "status --format json succeeds"
if printf '%s' "$OUT" | python3 -c 'import json,sys; d=json.load(sys.stdin); sys.exit(0 if d.get("service") and d.get("conditions") else 1)'; then
  ok "status --format json is valid JSON with service and conditions"
else
  ng "status --format json did not produce the expected JSON"
fi

info "--- 1-1c. wait ---"
run_cmd "$CLRND" wait "$SERVICE" --timeout 120s
assert_rc_zero "wait returns once the service is ready"
assert_contains "wait reports progress on stderr" "Ready=True"

info "--- 1-1d. revisions ---"
run_cmd "$CLRND" revisions "$SERVICE"
assert_rc_zero "revisions succeeds"
assert_contains "revisions prints a header" "REVISION"
assert_contains "revisions reports the traffic share" "100%"
assert_contains "revisions reports the image" "$IMAGE"
run_cmd "$CLRND" revisions "$SERVICE" --format json
assert_rc_zero "revisions --format json succeeds"
if printf '%s' "$OUT" | python3 -c 'import json,sys; d=json.load(sys.stdin); sys.exit(0 if isinstance(d, list) and d and d[0].get("name") else 1)'; then
  ok "revisions --format json is a non-empty array of revisions"
else
  ng "revisions --format json did not produce the expected JSON"
fi

info "--- 1-2. diff against a hand-written minimal manifest ---"
info "Cloud Run fills in defaults on create, so this diff is expected to be non-empty."
run_cmd "$CLRND" diff "$SERVICE" "$D1/manifest.yaml"
assert_rc_zero "diff succeeds"
assert_contains "server defaults show up in the diff (containerConcurrency)" "containerConcurrency"
assert_contains "server defaults show up in the diff (startupProbe)" "startupProbe"
assert_contains "server defaults show up in the diff (traffic)" "latestRevision"

info "--- 1-3. make the live service pin a revision name ---"
info "Cloud Run only reports spec.template.metadata.name when a client set it,"
info "so create that precondition the way --revision-suffix or Terraform would."
pin_live_revision "pinned" || ng "failed to pin a revision name on the live service"
wait_ready || ng "the pinned deploy never became ready"
PINNED_REV="$(live_revision_name)"
if [ -n "$PINNED_REV" ]; then
  ok "the live service now pins a revision name"
else
  ng "the live service does not pin a revision name; the rest of phase 1 proves nothing"
fi

info "--- 1-4. the manifest that init scaffolds ---"
D2="$WORK/current-init"; mkdir -p "$D2"; cd "$D2"
run_cmd "$CLRND" init "$SERVICE"
assert_rc_zero "init succeeds"
assert_file_lacks "init drops the revision name the live service pins" "$D2/manifest.yaml" "$PINNED_REV"
assert_file_lacks "init leaves no empty template metadata" "$D2/manifest.yaml" "metadata: {}"

info "--- 1-5. diff immediately after init ---"
run_cmd "$CLRND" diff
assert_rc_zero "diff succeeds"
assert_empty "diff right after init is empty"

info "--- 1-5b. change the template and deploy again ---"
info "This is the regression: with the revision name carried over, Cloud Run"
info "rejects the new revision with a 409."
set_env_value "$D2/manifest.yaml" "second"
run_cmd "$CLRND" deploy --auto-approve
assert_rc_zero "a second deploy that changes the template succeeds"
wait_ready || ng "the second deploy never became ready"

info "--- 1-6. verify ---"
run_cmd "$CLRND" verify
assert_rc_zero "verify succeeds"
assert_missing "no warning when the revision name is not pinned" "warning:"

info "--- 1-7. verify warns about a pinned revision name ---"
cp "$D2/manifest.yaml" "$D2/pinned.yaml"
REV="$(current_revision)"
[ -n "$REV" ] || ng "could not read the current revision name"
pin_revision "$D2/pinned.yaml" "$REV" || ng "pin_revision failed"
assert_file_has "the test manifest now pins the revision name" "$D2/pinned.yaml" "$REV"
run_cmd "$CLRND" verify "$SERVICE" "$D2/pinned.yaml" --local-only
assert_rc_zero "verify still succeeds with a pinned revision name"
assert_contains "verify warns about the pinned revision name" "warning:"

info "--- 1-8. deploying a pinned revision name is rejected ---"
set_env_value "$D2/pinned.yaml" "third"
run_cmd "$CLRND" deploy "$SERVICE" "$D2/pinned.yaml" --auto-approve --timeout 120s
assert_contains "deploy also warns about the pinned revision name" "warning:"
# Cloud Run はこの要求を同期的に 409 で拒否することも、受理してロールアウトだけ
# 失敗させることもある。deploy が待つようになったので、どちらの経路でも
# 非ゼロで終わらなければならない。以前は後者で exit 0 になっていた。
if [ "$RC" -ne 0 ]; then
  ok "deploy fails when a revision name cannot be reused (exit=$RC)"
  if printf '%s' "$OUT" | grep -q "alreadyExists"; then
    info "rejected synchronously by the API (409)"
  else
    info "accepted by the API, then caught by the rollout wait"
  fi
else
  ng "deploy exited 0 for a rollout that cannot succeed"
fi
fi

# =====================================================================
if [ -n "$OLD_REF" ] && [ "${ONLY:-}" != "current" ]; then
step "Phase 2: comparison against $OLD_REF"

D3="$WORK/old-init"; mkdir -p "$D3"; cd "$D3"

info "--- 2-0. make the live service pin a revision name again ---"
info "Phase 1 left the service without a pinned name, so recreate the precondition."
pin_live_revision "phase2" || ng "failed to pin a revision name on the live service"
wait_ready || info "the pinned deploy is not ready; continuing anyway"

info "--- 2-1. the manifest that the old init scaffolds ---"
run_cmd "$OLD" init "$SERVICE"
assert_rc_zero "init succeeds on $OLD_REF"
if grep -q "$SERVICE-0000" "$D3/manifest.yaml"; then
  info "$OLD_REF pins the revision name in the scaffolded manifest"
else
  info "$OLD_REF does not pin the revision name"
fi

info "--- 2-2. deploy a template change with the old binary ---"
set_env_value "$D3/manifest.yaml" "phase2"
run_cmd "$OLD" deploy --auto-approve
if [ "$RC" -ne 0 ]; then
  ok "$OLD_REF fails to deploy a template change (exit=$RC)"
else
  info "the API call succeeded; checking how the rollout ended..."
  sleep 5
  COND="$(ready_condition)"
  info "Ready condition: $(printf '%s' "$COND" | tr '\t' '/')"
  if [ "$(printf '%s' "$COND" | cut -f1)" = "False" ]; then
    ok "$OLD_REF fails asynchronously"
  else
    ok "$OLD_REF deploys the change successfully"
  fi
fi
elif [ -n "${ONLY:-}" ] && [ "${ONLY:-}" = "old" ]; then
die "ONLY=old requires OLD_REF"
fi

# =====================================================================
step "Result"
c '32' "  PASS: $PASS"
if [ "$FAIL" -gt 0 ]; then c '31' "  FAIL: $FAIL"; else echo "  FAIL: 0"; fi
[ "$FAIL" -eq 0 ]

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

# Carrying on after a failed resolution leaves HERE empty, and REPO then resolves to / instead of
# the repository, so the build and git would run against the wrong directory. Always stop here.
HERE="$(cd "$(dirname "$0")" && pwd)" || { echo "error: cannot resolve the script directory" >&2; exit 1; }
REPO="${REPO:-$(cd "$HERE/../.." && pwd)}"
[ -n "$REPO" ] || { echo "error: cannot resolve the repository root" >&2; exit 1; }
# Keep build output outside the repository. When the repository lives under a cloud-synced
# folder (Dropbox, iCloud, OneDrive ...), the sync client can revert a binary being built to an
# older version or create a "conflicted copy", and the test then silently runs an old binary.
#
# WORK_ROOT is "the parent directory of the work area". What is actually used is always a
# dedicated directory under it, and only that directory is removed (if WORK itself could be
# overridden, the rm -rf further down would wipe out whatever directory the user specified).
WORK_DIR_NAME="clrnd-e2e-work"
WORK_ROOT="${WORK_ROOT:-${TMPDIR:-/tmp}}"
WORK="${WORK_ROOT%/}/$WORK_DIR_NAME"
BIN="$WORK/bin"

# Common prefix of the throwaway services. It is also what --cleanup-orphans targets.
SERVICE_PREFIX="clrnd-e2e-"
SERVICE="${SERVICE_PREFIX}$(date +%Y%m%d%H%M%S)"

REGION="${REGION:-asia-northeast1}"
IMAGE="${IMAGE:-us-docker.pkg.dev/cloudrun/container/hello}"
# Comparing against an older ref is optional. Without one, phase 2 is skipped entirely.
OLD_REF="${OLD_REF:-}"

PASS=0
FAIL=0

# ---------- redaction ----------
# Make sure pasting the run log as-is exposes no Google Cloud identifiers. Pasting the log into
# an issue or PR is the natural next step after a failure, so redaction is on by default.
# $OUT, which the assertions check, is left untouched (only the display is redacted), so
# assertions can be written against the real names. For local investigation, RAW=1 restores
# the raw output.
#
# The order matters: mask URLs and service names first, then the remaining long numbers. In the
# reverse order the number replacement breaks the shape of URLs and service names, and the
# later patterns no longer match.
redact() {
  if [ "${RAW:-}" = "1" ]; then cat; return; fi
  sed \
    -e 's#https://[A-Za-z0-9._-]*\.run\.app#<service-url>#g' \
    -e "s/${SERVICE:-__no_service__}/<service>/g" \
    -e 's/clrnd-e2e-[0-9]\{8,\}/<service>/g' \
    -e "s/${PROJECT:-__no_project__}/<project>/g" \
    -e 's/[0-9]\{9,\}-compute@developer\.gserviceaccount\.com/<project-number>-compute@developer.gserviceaccount.com/g' \
    -e 's/[0-9]\{9,\}/<number>/g'
}

# ---------- output helpers ----------
# Output that can carry an identifier goes through c or info, and run_cmd redacts the command
# output it prints, so redacting in these helpers covers it. Only fixed text is printed directly.
c() { printf '\033[%sm%s\033[0m\n' "$1" "$2" | redact; }
step() { echo; c '1;36' "==== $* ===="; }
info() { echo "     $*" | redact; }
ok()   { PASS=$((PASS + 1)); c '32' "  PASS  $*"; }
ng()   { FAIL=$((FAIL + 1)); c '31' "  FAIL  $*"; }
die()  { c '31' "error: $*"; exit 1; }

# ---------- project resolution ----------
# Keep the project ID out of both the script body and the run log (to avoid leaking it).
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
    if delete_service "$name"; then
      info "  deleted"
    else
      c '31' "  failed to delete $name"
    fi
  done <<< "$names"
  [ "$found" -eq 1 ] || info "nothing to delete"
}

# Delete the service this run created on a normal exit, a failure or Ctrl-C alike.
# The trap does not run on kill -9; use --cleanup-orphans in that case.
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
    if delete_service "$SERVICE"; then
      info "deleted"
    else
      c '31' "     failed to delete; run: $0 --cleanup-orphans"
    fi
  else
    info "nothing to delete"
  fi
  return $rc
}

# ---------- assertions ----------
OUT=""
RC=0

# run_cmd runs a command and stores its output in OUT and its exit code in RC (a failure does not
# stop the script).
run_cmd() {
  info "\$ $(basename "$1") ${*:2}"
  OUT="$("$@" 2>&1)"
  RC=$?
  [ -z "$OUT" ] || printf '%s\n' "$OUT" | redact | sed 's/^/       | /'
  return 0
}

assert_rc_zero()    { if [ "$RC" -eq 0 ]; then ok "$1"; else ng "$1 (exit=$RC)"; fi; }
assert_empty()      { if [ -z "$OUT" ]; then ok "$1"; else ng "$1 (unexpected output)"; fi; }
assert_contains()   { if printf '%s' "$OUT" | grep -q -- "$2"; then ok "$1"; else ng "$1 (missing: $2)"; fi; }
assert_missing()    { if printf '%s' "$OUT" | grep -q -- "$2"; then ng "$1 (unexpected: $2)"; else ok "$1"; fi; }
assert_file_has()   { if grep -q -- "$3" "$2"; then ok "$1"; else ng "$1 (missing in $(basename "$2"): $3)"; fi; }
assert_file_lacks() { if grep -q -- "$3" "$2"; then ng "$1 (present in $(basename "$2"): $3)"; else ok "$1"; fi; }

# ---------- build ----------
# file_mtime <path> : print the modification time in epoch seconds (works with GNU and BSD stat).
# Try GNU first. In the reverse order, -f on GNU stat selects file system information, which
# cannot interpret %m and prints "?" with exit 0, so the fallback is never reached.
# BSD stat does not know -c and fails with exit != 0, so in this order it works correctly on both.
file_mtime() {
  stat -c %Y "$1" 2>/dev/null || stat -f %m "$1" 2>/dev/null
}

# build_binary <dest> <srcdir> : build, then confirm the output really was rewritten.
# If a revert or a cache leaves an old binary in place, the test reports false results.
build_binary() {
  local dest="$1" src="$2" before
  before="$(date +%s)"
  (cd "$src" && go build -o "$dest" .) || return 1
  [ -f "$dest" ] || { c '31' "     build produced no file at $dest"; return 1; }
  local mtime
  mtime="$(file_mtime "$dest")"
  case "$mtime" in
    ''|*[!0-9]*)
      # Do not let it pass unchecked (that would not be a safeguard at all).
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
# ready_condition prints the Ready condition as "<status>\t<reason>\t<message>".
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

# serving_revision prints the name of the revision receiving the most traffic.
# After a rollback this differs from latestReadyRevisionName, so check with this one.
serving_revision() {
  gcloud run services describe "$SERVICE" --project "$PROJECT" --region "$REGION" \
    --format=json 2>/dev/null | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    print(""); raise SystemExit
targets = (d.get("status") or {}).get("traffic") or []
best = max(targets, key=lambda t: t.get("percent", 0), default=None)
print(best.get("revisionName", "") if best else "")
'
}

# revision_percent <revision> : check through gcloud the share that revision receives
# (looking at the API's state, not clrnd's output). The same revision can appear in more than
# one entry (one for the percentage, one for a tag), so the shares are summed.
# Prints nothing when it cannot be read. Printing 0 or -1 would turn an auth error or an API
# failure into a *pass* as "0% share" or "0 revisions". Callers treat empty as a failure.
revision_percent() {
  local raw
  raw="$(gcloud run services describe "$SERVICE" --project "$PROJECT" --region "$REGION" \
    --format=json 2>/dev/null)" || return 0
  [ -n "$raw" ] || return 0
  printf '%s' "$raw" | python3 -c '
import json, sys
name = sys.argv[1]
try:
    d = json.load(sys.stdin)
except Exception:
    raise SystemExit
total = sum(t.get("percent", 0) or 0
            for t in ((d.get("status") or {}).get("traffic") or [])
            if t.get("revisionName") == name)
print(total)
' "$1"
}

# revision_count : the number of revisions belonging to the service. Prints nothing when it cannot
# be read.
revision_count() {
  local raw
  raw="$(gcloud run revisions list --service "$SERVICE" --project "$PROJECT" --region "$REGION" \
    --format='value(metadata.name)' 2>/dev/null)" || return 0
  printf '%s' "$raw" | grep -c . || true
}

# assert_percent <label> <revision> <expected percent>
assert_percent() {
  local got
  got="$(revision_percent "$2")"
  if [ -z "$got" ]; then
    ng "$1 (could not read the traffic split)"
  elif [ "$got" = "$3" ]; then
    ok "$1"
  else
    ng "$1 (got ${got}%, want $3%)"
  fi
}

# wait_serving <revision> : wait until that revision is serving (up to 60s).
# latestRevision resolves to "the newest *ready* revision", so right after a change it can
# still point at the previous one.
wait_serving() {
  local i
  for i in $(seq 1 20); do
    [ "$(serving_revision)" = "$1" ] && return 0
    sleep 3
  done
  return 1
}

# wait_revision_gone <revision> : wait until that revision is gone (up to 60s).
# Cloud Run deletes asynchronously; right after the call returns it can still be read.
wait_revision_gone() {
  local i
  for i in $(seq 1 20); do
    revision_exists "$1" || return 0
    sleep 3
  done
  return 1
}

# revision_exists <revision> : whether that revision still exists.
revision_exists() {
  gcloud run revisions describe "$1" --project "$PROJECT" --region "$REGION" \
    --format='value(metadata.name)' >/dev/null 2>&1
}

current_revision() {
  gcloud run services describe "$SERVICE" --project "$PROJECT" --region "$REGION" \
    --format='value(status.latestReadyRevisionName)' 2>/dev/null
}

# created_revision prints the most recently *created* revision. A revision that receives no
# traffic (deploy --no-traffic) does not show up in latestReadyRevisionName until it is ready,
# so use this one to look at "the revision the deploy just created".
created_revision() {
  gcloud run services describe "$SERVICE" --project "$PROJECT" --region "$REGION" \
    --format='value(status.latestCreatedRevisionName)' 2>/dev/null
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

# set_env_value <manifest> <value> : make CLRND_E2E equal to <value> (adding it if absent).
set_env_value() {
  python3 - "$1" "$2" <<'PY'
import re, sys
path, value = sys.argv[1], sys.argv[2]
s = open(path).read()
if "CLRND_E2E" in s:
    # Embedding the value in the replacement string would expand things like \1,
    # so use a function to treat value as a literal.
    s = re.sub(r'(name: CLRND_E2E\n\s+value: )\S+', lambda m: m.group(1) + value, s)
else:
    s = s.replace("      containers:\n      - image:",
                  "      containers:\n      - env:\n        - name: CLRND_E2E\n          value: %s\n        image:" % value)
open(path, "w").write(s)
PY
}

# pin_live_revision <suffix> : make the live service pin a revision name.
# gcloud run deploy --revision-suffix sets spec.template.metadata.name, and Terraform's
# template.metadata.name produces the same state. For a service created without a revision
# name Cloud Run does not return this field, so exercising the path where init carries the
# name over requires creating this precondition explicitly.
pin_live_revision() {
  gcloud run deploy "$SERVICE" --image "$IMAGE" --revision-suffix="$1" \
    --project "$PROJECT" --region "$REGION" --no-allow-unauthenticated --quiet >/dev/null 2>&1
}

# live_revision_name prints the revision name the live service pins (empty if none).
live_revision_name() {
  gcloud run services describe "$SERVICE" --project "$PROJECT" --region "$REGION" \
    --format='value(spec.template.metadata.name)' 2>/dev/null
}

# pin_revision <manifest> <revision-name> : set spec.template.metadata.name.
# When a metadata block already exists (for example when annotations from gcloud remain), add
# name inside it. Otherwise create metadata along with it.
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
            out.append(lines[i + 1])            # keep the existing metadata:
            out.append("      name: %s" % rev)  # and insert name right under it
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
# Never remove anything other than the dedicated directory this script creates.
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

D1="$WORK/current"; mkdir -p "$D1"; cd "$D1" || die "cannot enter $D1"

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

info "--- 1-2. diff against a hand-written minimal manifest, as written ---"
info "Cloud Run fills in defaults on create, so comparing the manifest as written is non-empty."
run_cmd "$CLRND" diff "$SERVICE" "$D1/manifest.yaml" --no-server-defaults
assert_rc_zero "diff --no-server-defaults succeeds"
assert_contains "server defaults show up in the diff (containerConcurrency)" "containerConcurrency"
assert_contains "server defaults show up in the diff (startupProbe)" "startupProbe"
assert_contains "server defaults show up in the diff (traffic)" "latestRevision"
# Metadata the server adds on its own must be gone without relying on default resolution
# (issue #25). This is the only place that runs that path against a real service.
assert_missing "the location label is not part of the diff" "cloud.googleapis.com/location"
assert_missing "server-set metadata is not part of the diff" "serving.knative.dev/creator"

info "--- 1-2b. diff (default: server defaults resolved) ---"
info "By default the server resolves the defaults, so the same minimal manifest shows no diff."
run_cmd "$CLRND" diff "$SERVICE" "$D1/manifest.yaml"
assert_rc_zero "diff succeeds"
assert_empty "diff converges on a minimal manifest by default"

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
D2="$WORK/current-init"; mkdir -p "$D2"; cd "$D2" || die "cannot enter $D2"
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

info "--- 1-5b2. refresh ---"
BEFORE_REFRESH="$(current_revision)"
run_cmd "$CLRND" refresh "$SERVICE" --auto-approve --timeout 120s
assert_rc_zero "refresh succeeds"
wait_ready || ng "the service did not become ready after the refresh"
AFTER_REFRESH="$(current_revision)"
if [ -n "$AFTER_REFRESH" ] && [ "$AFTER_REFRESH" != "$BEFORE_REFRESH" ]; then
  ok "refresh created a new revision ($AFTER_REFRESH)"
else
  ng "refresh did not create a new revision (still $AFTER_REFRESH)"
fi
# refresh names the revision explicitly, but the local manifest carries no name, so it does not
# show up in the diff (alignRevisionName). If this breaks, diff never converges.
run_cmd "$CLRND" diff
assert_rc_zero "diff succeeds after a refresh"
assert_empty "diff stays empty after a refresh"

info "--- 1-5c. rollback ---"
BEFORE_REV="$(serving_revision)"
EXPECTED_REV="$("$CLRND" revisions "$SERVICE" --format json 2>/dev/null | python3 -c '
import json, sys
revisions = json.load(sys.stdin)
serving = max(range(len(revisions)), key=lambda i: revisions[i]["percent"])
for r in revisions[serving + 1:]:
    if r.get("ready") == "True":
        print(r["name"])
        break
')"
info "serving now: $BEFORE_REV / expected rollback target: $EXPECTED_REV"
if [ -n "$EXPECTED_REV" ] && [ "$EXPECTED_REV" != "$BEFORE_REV" ]; then
  ok "there is an older ready revision to roll back to"
else
  ng "could not determine a rollback target (serving=$BEFORE_REV target=$EXPECTED_REV)"
fi

run_cmd "$CLRND" rollback "$SERVICE" --auto-approve --timeout 120s
assert_rc_zero "rollback succeeds"
wait_ready || ng "the service did not become ready after the rollback"

AFTER_REV="$(serving_revision)"
if [ "$AFTER_REV" = "$EXPECTED_REV" ]; then
  ok "traffic moved to the previous revision"
else
  ng "serving revision is $AFTER_REV, want $EXPECTED_REV"
fi

info "--- 1-5c2. refresh refuses when it cannot do its job ---"
# (a) Specifying the same revision name as the current one. With the same name no new revision
#     is created, so this is the path where the diff is empty, "No changes." is printed, and the
#     command succeeds having done nothing.
CURRENT_TEMPLATE_REV="$(live_revision_name)"
if [ -n "$CURRENT_TEMPLATE_REV" ]; then
  SAME_SUFFIX="${CURRENT_TEMPLATE_REV#"$SERVICE"-}"
  run_cmd "$CLRND" refresh "$SERVICE" --revision-suffix "$SAME_SUFFIX" --auto-approve
  if [ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q "already the current template revision"; then
    ok "refresh refuses a suffix that would not create a revision"
  else
    ng "refresh accepted a suffix that creates no revision (exit=$RC)"
  fi
else
  info "skipping: the live service pins no template revision name"
fi

# (b) Right after a rollback, traffic is pinned to a specific revision. A refresh in this state
#     would create a new revision that only ever gets 0%.
run_cmd "$CLRND" refresh "$SERVICE" --auto-approve
if [ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q "receives no traffic"; then
  ok "refresh refuses while traffic is pinned to specific revisions"
else
  ng "refresh created a revision that would serve nothing (exit=$RC)"
fi

info "--- 1-5d. deploy again after the rollback ---"
# rollback pins spec.traffic, so confirm that a later deploy can move it back to a new revision
# (that the service does not get stuck pinned).
set_env_value "$D2/manifest.yaml" "after-rollback"
run_cmd "$CLRND" deploy --auto-approve --timeout 120s
assert_rc_zero "deploy still works after a rollback"

info "--- 1-5e. traffic: split, then follow the latest again ---"
# rollback only moves all 100% back; partial shares and the "follow the latest again" path
# exist only in traffic. What is checked here, against the real API: (a) the split matches what
# was requested, (b) no revision is created, and (c) the pin can be removed so traffic follows
# the latest revision via latestRevision again.
LATEST_REV="$(current_revision)"
PREV_REV="$("$CLRND" revisions "$SERVICE" --format json 2>/dev/null | python3 -c '
import json, sys
latest = sys.argv[1]
for r in json.load(sys.stdin):
    if r["name"] != latest and r.get("ready") == "True":
        print(r["name"]); break
' "$LATEST_REV")"
if [ -z "$PREV_REV" ]; then
  ng "no older ready revision to split traffic with"
else
  BEFORE_COUNT="$(revision_count)"
  run_cmd "$CLRND" traffic "$SERVICE" --to "$PREV_REV" --percent 20 --auto-approve --timeout 120s
  assert_rc_zero "traffic splits the assignment"
  wait_ready || ng "the service did not settle after the traffic split"

  assert_percent "the target revision receives the requested share" "$PREV_REV" 20
  assert_percent "the rest stays on the revision that was serving" "$LATEST_REV" 80
  AFTER_COUNT="$(revision_count)"
  if [ -z "$BEFORE_COUNT" ] || [ -z "$AFTER_COUNT" ]; then
    ng "could not read the revision count"
  elif [ "$AFTER_COUNT" = "$BEFORE_COUNT" ]; then
    ok "traffic creates no revision"
  else
    ng "the revision count changed from $BEFORE_COUNT to $AFTER_COUNT"
  fi

  run_cmd "$CLRND" traffic "$SERVICE" --to-latest --auto-approve --timeout 120s
  assert_rc_zero "traffic --to-latest succeeds"
  wait_ready || ng "the service did not settle after --to-latest"
  wait_serving "$LATEST_REV" || true
  assert_percent "traffic follows the latest revision again" "$LATEST_REV" 100
  # Also check that the pin was removed (that the spec side is back to latestRevision). If it is
  # still a name, traffic will not move to the revision the next deploy creates.
  if gcloud run services describe "$SERVICE" --project "$PROJECT" --region "$REGION" \
      --format=json 2>/dev/null |
      python3 -c 'import json,sys; print(any(t.get("latestRevision") for t in ((json.load(sys.stdin).get("spec") or {}).get("traffic") or [])))' |
      grep -q True; then
    ok "--to-latest leaves the split unpinned"
  else
    ng "--to-latest pinned the split to a revision name"
  fi
fi

info "--- 1-5f. deploy --no-traffic, then move traffic over ---"
# The "deploy first, decide on serving later" path. It cannot be expressed in a manifest, so
# confirm against the real API that the new revision is created at 0%.
SERVING_BEFORE="$(serving_revision)"
set_env_value "$D2/manifest.yaml" "no-traffic"
run_cmd "$CLRND" deploy --no-traffic --auto-approve --timeout 120s
assert_rc_zero "deploy --no-traffic succeeds"
# A revision that receives no traffic is not listed in latestReadyRevisionName until it counts
# as ready. What matters here is "was it created", so use latestCreatedRevisionName.
NEW_REV="$(created_revision)"
if [ -n "$NEW_REV" ] && [ "$NEW_REV" != "$SERVING_BEFORE" ]; then
  ok "deploy --no-traffic creates a new revision"
else
  ng "no new revision was created (latest=$NEW_REV serving-before=$SERVING_BEFORE)"
fi
assert_percent "the new revision receives no traffic" "$NEW_REV" 0
if [ "$(serving_revision)" = "$SERVING_BEFORE" ]; then
  ok "the previous revision keeps serving"
else
  ng "traffic moved to $(serving_revision), want it to stay on $SERVING_BEFORE"
fi

run_cmd "$CLRND" traffic "$SERVICE" --to-latest --auto-approve --timeout 120s
assert_rc_zero "traffic moves to the revision deployed with --no-traffic"
wait_ready || ng "the service did not settle after moving traffic"
# latestRevision resolves to "the newest *ready* revision". A revision created with --no-traffic
# is not ready until its instances are up, so wait for the switch.
if wait_serving "$NEW_REV"; then
  ok "the canary sequence ends on the new revision"
else
  ng "serving revision is $(serving_revision), want $NEW_REV"
fi

info "--- 1-6. verify ---"
run_cmd "$CLRND" verify
assert_rc_zero "verify succeeds"
assert_missing "no warning when the revision name is not pinned" "warning:"

info "--- 1-6b. verify --format json ---"
run_cmd "$CLRND" verify --format json
assert_rc_zero "verify --format json succeeds"
if printf '%s' "$OUT" | python3 -c '
import json, sys
d = json.load(sys.stdin)
raise SystemExit(0 if d.get("ok") is True and d.get("service") and not d.get("missing") else 1)
' 2>/dev/null; then
  ok "verify --format json reports ok with no missing resources"
else
  ng "verify --format json did not produce the expected object"
fi

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
# Cloud Run may reject this request synchronously with a 409, or accept it and fail only the
# rollout. Now that deploy waits, it must exit non-zero on either path. Previously the latter
# exited 0.
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

info "--- 1-8b. re-deploying the same manifest does not report success ---"
# 1-8 left the service broken. With the same manifest the diff is empty, and previously this
# exited 0 with just "No changes.".
run_cmd "$CLRND" deploy "$SERVICE" "$D2/pinned.yaml" --auto-approve --timeout 60s
if [ "$RC" -ne 0 ]; then
  ok "a retry with no changes still fails while the service is unhealthy (exit=$RC)"
  if printf '%s' "$OUT" | grep -q "No changes."; then
    info "confirmed via the no-changes path (nothing was applied, health was still checked)"
  fi
else
  ng "a retry with no changes reported success while the service is unhealthy"
fi

info "--- 1-8b2. verify checks the container image ---"
# The image existence check really queries Artifact Registry. $IMAGE is a public image, so
# ordinary ADC can read it (this step also verifies that assumption itself).
D5="$WORK/current-image"; mkdir -p "$D5"
write_manifest "$D5/manifest.yaml"
run_cmd "$CLRND" verify "$SERVICE" "$D5/manifest.yaml"
assert_rc_zero "verify accepts a real Artifact Registry image"
assert_missing "no warning for an image it could check" "warning:"

# A tag that does not exist. It returns 404, so verify must fail.
sed "s#image: .*#image: ${IMAGE}:clrnd-e2e-no-such-tag#" "$D5/manifest.yaml" > "$D5/bad-tag.yaml"
run_cmd "$CLRND" verify "$SERVICE" "$D5/bad-tag.yaml"
if [ "$RC" -ne 0 ]; then ok "verify rejects an image tag that does not exist"; else ng "verify accepted a nonexistent image tag"; fi
assert_contains "verify names the missing image" "does not exist"

# A registry that cannot be checked is skipped silently (no warning on every run).
sed "s#image: .*#image: gcr.io/clrnd-e2e-no-such-project/no-such-image:v1#" "$D5/manifest.yaml" > "$D5/gcr.yaml"
run_cmd "$CLRND" verify "$SERVICE" "$D5/gcr.yaml"
assert_rc_zero "verify passes a gcr.io image it cannot check"
assert_missing "verify says nothing about a registry it cannot check" "warning:"

info "--- 1-8d. --image overrides the manifest ---"
# Replace the nonexistent tag in the manifest with a real image via --image.
# If the override did not take effect, verify and deploy would fail, so success is the evidence.
run_cmd "$CLRND" verify "$SERVICE" "$D5/bad-tag.yaml" --image "$IMAGE"
assert_rc_zero "verify checks the overridden image, not the one in the manifest"

run_cmd "$CLRND" deploy "$SERVICE" "$D5/bad-tag.yaml" --image "$IMAGE" --auto-approve --timeout 120s
assert_rc_zero "deploy applies the overridden image"
LIVE_IMAGE="$(gcloud run services describe "$SERVICE" --project "$PROJECT" --region "$REGION" \
  --format='value(spec.template.spec.containers[0].image)' 2>/dev/null)"
if [ "$LIVE_IMAGE" = "$IMAGE" ]; then
  ok "the live service runs the overridden image"
else
  ng "the live image is $LIVE_IMAGE, want $IMAGE"
fi

# With a single container the name can be omitted, but a name that does not exist is rejected.
run_cmd "$CLRND" deploy "$SERVICE" "$D5/bad-tag.yaml" --image "sidecar=$IMAGE" --auto-approve
if [ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q "does not define"; then
  ok "--image rejects a container the manifest does not define"
else
  ng "--image accepted an unknown container name (exit=$RC)"
fi

info "--- 1-8c. render ---"
# render does not touch the API, but this is the only place that runs template expansion
# (tfstate / env / must_env) through the real binary. The unit tests call render.Render
# directly and do not cover the path from flag parsing through to writing the file.
D4="$WORK/current-render"; mkdir -p "$D4"
cat > "$D4/e2e.tfstate" <<JSON
{
  "version": 4,
  "terraform_version": "1.9.0",
  "serial": 1,
  "lineage": "clrnd-e2e",
  "outputs": {},
  "resources": [
    {
      "mode": "managed",
      "type": "null_resource",
      "name": "image",
      "provider": "provider[\"registry.terraform.io/hashicorp/null\"]",
      "instances": [
        { "schema_version": 0, "attributes": { "id": "$IMAGE" } }
      ]
    }
  ]
}
JSON
cat > "$D4/template.yaml" <<'YAML'
apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: {{ must_env "CLRND_E2E_SERVICE" }}
spec:
  template:
    spec:
      containers:
      - image: {{ tfstate "null_resource.image.id" }}
        env:
        - name: FROM_ENV
          value: "{{ env "CLRND_E2E_UNSET" "fallback" }}"
YAML

export CLRND_E2E_SERVICE="$SERVICE"
run_cmd "$CLRND" render "$D4/template.yaml" --tfstate "$D4/e2e.tfstate"
assert_rc_zero "render succeeds"
assert_contains "render resolves {{ tfstate }} from the state file" "image: $IMAGE"
assert_contains "render resolves {{ must_env }}" "name: $SERVICE"
assert_contains "render falls back to the {{ env }} default" "fallback"
assert_missing "render leaves no unexpanded template" "{{"

run_cmd "$CLRND" render "$D4/template.yaml" --tfstate "$D4/e2e.tfstate" -o "$D4/rendered.yaml"
assert_rc_zero "render -o succeeds"
assert_empty "render -o prints nothing on stdout"
assert_file_has "render -o writes the expanded manifest" "$D4/rendered.yaml" "image: $IMAGE"

run_cmd "$CLRND" render "$D4/template.yaml" --tfstate "$D4/e2e.tfstate" -o "$D4/template.yaml"
if [ "$RC" -ne 0 ]; then ok "render refuses to write over its own input"; else ng "render overwrote its own input"; fi
assert_file_has "the template source is untouched" "$D4/template.yaml" "must_env"

# json_escape is a template function, but whether the result is still valid YAML/JSON when a
# troublesome value goes through it can only be known by actually expanding it.
# Check the form README recommends (a >- block scalar) with a value containing an apostrophe.
# json_escape escapes for JSON and leaves ' alone, so embedding it in '...' can break the YAML
# depending on the value. Test the real-world shape: JSON inside an annotation.
export CLRND_E2E_RAW="it's \"quoted\" & fine"
cat > "$D4/escape.yaml" <<YAML
apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: $SERVICE
spec:
  template:
    metadata:
      annotations:
        clrnd-e2e.example.com/config: >-
          {"text": "{{ must_env "CLRND_E2E_RAW" | json_escape }}"}
    spec:
      containers:
      - image: $IMAGE
YAML
run_cmd "$CLRND" render "$D4/escape.yaml" -o "$D4/escaped.yaml"
assert_rc_zero "render applies json_escape"
assert_file_has "json_escape escapes the quotes" "$D4/escaped.yaml" '\\"quoted\\"'

# YAML layer: the strict parser (verify) can read it. Embedded in '...', this would fail here.
run_cmd "$CLRND" verify "$SERVICE" "$D4/escaped.yaml" --local-only
assert_rc_zero "the manifest with an escaped JSON annotation still parses"

# JSON layer: extract the annotation value, and check it parses as JSON and yields the original.
# The expected value is passed via argv (so shell and Python quoting are not layered twice).
cat > "$D4/check-escape.py" <<'PYCHECK'
import json, sys

lines = open(sys.argv[1]).read().split("\n")
payload = ""
for i, line in enumerate(lines):
    if line.rstrip().endswith(">-"):
        payload = lines[i + 1].strip()
        break
raise SystemExit(0 if payload and json.loads(payload)["text"] == sys.argv[2] else 1)
PYCHECK
if python3 "$D4/check-escape.py" "$D4/escaped.yaml" "$CLRND_E2E_RAW"; then
  ok "the escaped value parses as JSON and round-trips"
else
  ng "json_escape produced something that does not round-trip through YAML and JSON"
fi
unset CLRND_E2E_RAW

# Whether the expanded result is really deployable is shown by running verify on the same template.
run_cmd "$CLRND" verify "$SERVICE" "$D4/template.yaml" --tfstate "$D4/e2e.tfstate" --local-only
assert_rc_zero "verify accepts the rendered template"
unset CLRND_E2E_SERVICE

info "--- 1-8e. revisions --prune ---"
# Cloud Run does not delete old revisions automatically. Several have piled up by now, so
# confirm that only old ones are deleted while the serving one is kept.
PRUNE_BEFORE="$(revision_count)"
PRUNE_SERVING="$(serving_revision)"
run_cmd "$CLRND" revisions "$SERVICE" --prune --keep 1 --dry-run
assert_rc_zero "revisions --prune --dry-run succeeds"
if [ "$(revision_count)" = "$PRUNE_BEFORE" ]; then
  ok "--dry-run deletes nothing"
else
  ng "--dry-run changed the revision count ($PRUNE_BEFORE -> $(revision_count))"
fi

# Note down one revision that should be deleted. Deletion is asynchronous, so judge by this one
# revision disappearing rather than by the count (right after the call returns, the count may
# not have gone down yet).
PRUNE_TARGET="$("$CLRND" revisions "$SERVICE" --format json 2>/dev/null | python3 -c '
import json, sys
revisions = json.load(sys.stdin)
for r in revisions[1:]:
    if r.get("percent", 0) == 0 and not r.get("tags"):
        print(r["name"]); break
')"
run_cmd "$CLRND" revisions "$SERVICE" --prune --keep 1 --auto-approve
assert_rc_zero "revisions --prune succeeds"
if [ -z "$PRUNE_TARGET" ]; then
  ng "could not determine a revision that should have been pruned"
elif wait_revision_gone "$PRUNE_TARGET"; then
  ok "an old revision is gone"
else
  ng "$PRUNE_TARGET is still there after pruning"
fi

# With --keep 1 the serving revision is also "the newest one", so the protection rule itself
# is not exercised. Go down to --keep 0 and confirm the revision receiving traffic is kept.
run_cmd "$CLRND" revisions "$SERVICE" --prune --keep 0 --auto-approve
assert_rc_zero "revisions --prune --keep 0 succeeds"
if revision_exists "$PRUNE_SERVING"; then
  ok "the revision serving traffic is kept even with --keep 0"
else
  ng "the revision serving traffic was deleted"
fi
if [ "$(serving_revision)" = "$PRUNE_SERVING" ]; then
  ok "the traffic split is unchanged after pruning"
else
  ng "serving revision is $(serving_revision), want $PRUNE_SERVING"
fi
PRUNE_AFTER="$(revision_count)"
if [ -z "$PRUNE_BEFORE" ] || [ -z "$PRUNE_AFTER" ]; then
  ng "could not read the revision count"
elif [ "$PRUNE_AFTER" -lt "$PRUNE_BEFORE" ]; then
  ok "the revision count went down ($PRUNE_BEFORE -> $PRUNE_AFTER)"
else
  ng "no revision was deleted (still $PRUNE_AFTER)"
fi
run_cmd "$CLRND" status
assert_rc_zero "the service still works after pruning"

info "--- 1-9. delete ---"
if [ -n "$OLD_REF" ]; then
  info "skipping: phase 2 still needs the service"
else
  run_cmd "$CLRND" delete "$SERVICE" --dry-run
  assert_rc_zero "delete --dry-run succeeds"
  assert_contains "delete shows what it is about to remove" "About to delete:"
  if gcloud run services describe "$SERVICE" --project "$PROJECT" --region "$REGION" >/dev/null 2>&1; then
    ok "delete --dry-run left the service alone"
  else
    ng "delete --dry-run removed the service"
  fi

  # Deletion is asynchronous, so clrnd delete waits until the service is gone. It can be
  # checked right after it returns.
  run_cmd "$CLRND" delete "$SERVICE" --auto-approve --timeout 120s
  assert_rc_zero "delete succeeds"
  if gcloud run services describe "$SERVICE" --project "$PROJECT" --region "$REGION" >/dev/null 2>&1; then
    ng "the service still exists right after delete returned"
  else
    ok "the service is gone by the time delete returns"
  fi
fi
fi

# =====================================================================
if [ -n "$OLD_REF" ] && [ "${ONLY:-}" != "current" ]; then
step "Phase 2: comparison against $OLD_REF"

D3="$WORK/old-init"; mkdir -p "$D3"; cd "$D3" || die "cannot enter $D3"

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

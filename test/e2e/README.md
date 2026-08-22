# End-to-end test

Runs the `clrnd` binary against a real Cloud Run project: it creates a service,
deploys to it, inspects the results, and deletes it again.

This is **not** part of `go test ./...`. The directory holds no Go files, so
`go build ./...`, `go vet ./...`, and `go test ./...` ignore it entirely. It
cannot run in CI either, because CI has no Google Cloud credentials. Run it by
hand when you change anything that talks to the Cloud Run API.

> **It creates real, billable resources.** The services are tiny and are deleted
> on exit, but they are real. Only point this at a project you are happy to
> create and destroy Cloud Run services in.

## Setup

- `gcloud` on `PATH`, authenticated with Application Default Credentials
  (`gcloud auth application-default login`)
- The Cloud Run API enabled on the target project
- A project to run against, provided either way:
  - `PROJECT=<project-id> ./run.sh`, or
  - write the project ID into `project.env` next to this README (one line).
    That file is git-ignored — **do not commit a project ID.**

Without one of those the script refuses to run, which also means a fresh clone
cannot create anything by accident.

## Usage

```sh
./run.sh                        # run against the current working tree
./run.sh --cleanup-orphans      # delete leftover clrnd-e2e-* services and exit
KEEP=1 ./run.sh                 # keep the service for inspection
ONLY=current ./run.sh           # only the current-binary phase
OLD_REF=<git-ref> ./run.sh      # also build that ref and compare its behaviour
REGION=<region> ./run.sh        # default: asia-northeast1
WORK_ROOT=<dir> ./run.sh        # parent of the build/scratch dir (default: $TMPDIR)
```

Each run uses a unique service named `clrnd-e2e-<timestamp>` and deletes it on
exit, including on failure and Ctrl-C. `kill -9` skips that cleanup; use
`--cleanup-orphans` to remove anything left behind.

Build output and scratch directories go to `$TMPDIR/clrnd-e2e-work`, **not** into
the repository. That is deliberate: when the checkout lives in a cloud-synced
folder (Dropbox, iCloud, OneDrive), the sync client can restore an older copy of
a binary while it is being rebuilt, or move the new one aside as a "conflicted
copy" — and the run then silently tests a stale binary. `build_binary` also fails
the run if the output was not actually rewritten.

`WORK_ROOT=<dir>` moves that directory elsewhere. It names the **parent**: the run
always works inside `<dir>/clrnd-e2e-work` and only ever deletes that
subdirectory, so pointing `WORK_ROOT` at a directory of your own does not wipe it.

That directory **contains the real service name, service account, and URL** — do
not paste it into an issue or a pull request.

## What phase 1 checks

| # | Check |
| --- | --- |
| 1-1 | `deploy` creates a new service and it becomes ready |
| 1-1b | `status` reports Ready, the URL and the traffic split, in text and JSON |
| 1-1c | `wait` returns once the service is ready and reports progress on stderr |
| 1-2 | a hand-written minimal manifest keeps showing server defaults in `diff` (see issue #11) |
| 1-3 | the live service is made to pin a revision name, the way `--revision-suffix` does |
| 1-4 | `init` drops that revision name and leaves no empty template metadata |
| 1-5 | `diff` right after `init` is empty, and a following template change deploys cleanly |
| 1-6 | `verify` succeeds and prints no warning |
| 1-7 | `verify` warns about a pinned revision name but still succeeds |
| 1-8 | `deploy` warns about a pinned revision name and **exits non-zero** — whether Cloud Run rejects it synchronously (409) or the rollout fails afterwards |

Step 1-3 matters: **Cloud Run does not report a revision name it generated itself.**
A service deployed without one comes back with no `spec.template.metadata.name`
at all, so `init` has nothing to carry over and the rest of phase 1 would pass
for the wrong reason. The precondition has to be created deliberately.

## Phase 2: comparing against another revision

Set `OLD_REF` to any git ref to build that revision as well and run the same
service through it. Phase 2 is skipped when `OLD_REF` is unset.

This is a bisect-style tool, not a fixed expectation: it reports what the older
binary does rather than asserting a particular outcome. It was originally used
to confirm that, before revision names were removed from what `init` writes,
`init` → edit → `deploy` failed with:

```
googleapi: Error 409: Revision named '<revision>' with different configuration
already exists., alreadyExists
```

## Behaviour worth knowing about

Things this test pinned down that are not obvious from the code:

- **A revision name only comes back if a client set it.** Deploy without
  `spec.template.metadata.name` and the field is absent from every later read —
  `spec.template.metadata` holds just the server-managed labels. Deploy with
  `gcloud run deploy --revision-suffix=<s>`, a Terraform `template.metadata.name`,
  or a manifest that pins one, and the field is set and echoed back from then on.
  That is the precondition under which carrying the name into a scaffolded
  manifest breaks the next deploy.

- **Reusing a revision name can fail asynchronously.** The `ReplaceService` call
  returns 200 and `clrnd deploy` exits 0, while the rollout fails with
  `Ready=False / ConflictingRevisionName`. A CI job would treat that as success.
  This is the concrete argument for the `wait` subcommand (issue #8).
- **Cloud Run fills in a lot of defaults on create** — `containerConcurrency`,
  container `ports`, `resources.limits`, `startupProbe`, `serviceAccountName`,
  `timeoutSeconds`, `spec.traffic`, and several annotations and labels. A
  hand-written minimal manifest therefore never diffs clean (issue #11); a
  manifest produced by `init` does.

# CLAUDE.md

This file provides shared guidance to Claude Code and OpenAI Codex when working with code in this
repository. It holds what you need before touching the code and cannot get from the file you are
editing: the rules that cut across files, where things live, and how to extend clrnd. The reasons
behind a single function are in that function's comment; keep them there, not here, so the two
copies cannot drift apart.

## Overview

`clrnd` is a Go CLI that deploys Cloud Run services, jobs and worker pools from Knative-style YAML
manifests.
For services it offers the subcommand set ecspresso has where Cloud Run's model allows it (ECS-only
commands such as `register`/`exec`/`scale` are deliberately absent), plus `traffic`, which splits
traffic between revisions — Cloud Run's own model. Jobs live under `clrnd jobs` and worker pools
under `clrnd worker-pools`, spelled the way gcloud spells them (`gcloud run jobs`,
`gcloud run worker-pools`); the top-level commands stay the services'. See
[Adding a resource kind](#adding-a-resource-kind) for how a kind is put on the shared path.

## Commands

```sh
go build ./...          # build all packages
go run . init <svc> --project <P> --region <R>   # run without installing
go install .            # install the clrnd binary to $GOPATH/bin
go test ./...           # run tests
go test -run TestName ./internal/cloudrun   # run a single test
go vet ./...            # static checks
gofmt -w .              # format
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.1 run ./...   # lint (CI pins the same version in verify.yml; keep both in sync)

# End-to-end test against a real Cloud Run project. NOT part of `go test ./...`
# and it cannot run in CI (no credentials). See test/e2e/README.md.
PROJECT=<project-id> ./test/e2e/run.sh
./test/e2e/run.sh --cleanup-orphans   # remove what a killed run left behind
```

[test/e2e](test/e2e/) uses Go with the `e2e` build tag to create and delete a real resource of each
kind.
Ordinary `go test ./...` excludes it, and without `CLRND_E2E_CLOUD=1` (which `run.sh` sets) it
skips; opt in through `run.sh` with an explicitly configured project.
It is the only check of server-side behaviour (defaulting, revision-name conflicts, asynchronous
rollout failures, what the API echoes back), so run it when you change anything that talks to the
Cloud Run API, and extend it when you add a command that does.

## Where things live

- [main.go](main.go) → `cmd.Execute`. [cmd/](cmd/) parses flags and does I/O only;
  [internal/cloudrun](internal/cloudrun/) holds every API call and all manifest handling.
- Commands: one file per command in [cmd/](cmd/), registered on `rootCmd` ([cmd/root.go](cmd/root.go)),
  `jobsCmd` ([cmd/jobs.go](cmd/jobs.go)), `jobsExecutionsCmd` or `workerPoolsCmd`
  ([cmd/worker_pools.go](cmd/worker_pools.go)). The commands every kind has —
  `verify`, `diff`, `deploy`, `delete`, `init`, `wait`, plus `revisions`, `rollback` and the
  command that changes the split (`traffic`, `worker-pools instance-split`) for the kinds that have
  revisions — are built once per `target`
  ([cmd/target.go](cmd/target.go)) by `newXCmd(t)` factories.
- Shared cmd pieces: the apply flow and confirmation ([cmd/apply.go](cmd/apply.go)), target and
  format flags and resolution ([cmd/flags.go](cmd/flags.go)), file writing
  ([cmd/write.go](cmd/write.go)), exit codes ([cmd/exit.go](cmd/exit.go)), error printing
  (`reportError` in [cmd/root.go](cmd/root.go)).
- [internal/cloudrun](internal/cloudrun/), by concern:
  - kinds and the plan/diff/apply flow: [kind.go](internal/cloudrun/kind.go); the service kind and
    manifest normalization: [cloudrun.go](internal/cloudrun/cloudrun.go); the job kind:
    [job.go](internal/cloudrun/job.go); the worker pool kind and its instance-count rules:
    [workerpool.go](internal/cloudrun/workerpool.go); listing revisions, rolling back and changing
    the split for any kind that has them: [revisionkind.go](internal/cloudrun/revisionkind.go);
    revision-template helpers:
    [template.go](internal/cloudrun/template.go); what a manifest runs:
    [workload.go](internal/cloudrun/workload.go)
  - the client and target validation: [client.go](internal/cloudrun/client.go),
    [target.go](internal/cloudrun/target.go)
  - API existence checks: [verify.go](internal/cloudrun/verify.go), [image.go](internal/cloudrun/image.go)
  - waiting: [wait.go](internal/cloudrun/wait.go); listing: [paging.go](internal/cloudrun/paging.go)
  - per-command logic: `status.go`, `revisions.go`, `rollback.go`, `traffic.go`,
    `instancesplit.go`, `refresh.go`, `jobstatus.go`, `workerpoolstatus.go`, `execution.go`, `executions.go`, `imageoverride.go`
- Manifest templating (ecspresso-compatible `tfstate`, `env`, `must_env`, `json_escape`):
  [internal/render](internal/render/render.go). The config file: [internal/config](internal/config/config.go).

## Invariants

Break one of these and something that looks fine fails for a user. Each is explained where it is
implemented.

- **Interruptible.** Every command uses `cmd.Context()`, never `context.Background()`; anything that
  blocks (a prompt, a poll) selects on `ctx.Done()`.
- **Local checks first.** Read, render (`renderManifest`), apply `--image`, validate
  (`cloudrun.Validate` / `CheckSyntax`) and check flags *before* `newCloudRunClient`, so a local
  mistake is never hidden behind "project is required" or a credentials error.
- **The target is named on purpose.** Project and region come from the flag, then the config file —
  never from the environment (gcloud's `CLOUDSDK_*`/`GOOGLE_CLOUD_*` are set by CI auth actions and
  Cloud Shell; see `resolveProject`). Do not add them back, even as a lowest fallback. Both are
  validated where requests are built (`NewClient`, `VerifyRemote`): the region becomes the API's
  host name.
- **Every write is a compare-and-swap** on the `resourceVersion` of the read the diff was computed
  from (`setResourceVersion`). Never invent one, and never send one a manifest carried (`parse`
  drops it).
- **diff is deploy without the apply.** Both go through `planObject`/`planAgainst`
  (`TestDiffAndPlanAgree`). Commands that edit the live definition (rollback, traffic,
  instance-split, refresh) read it once and plan against that read (`PlanRollback`, `PlanSplit`,
  `PlanEdit`).
- **Server defaults are for the diff only.** The dry-run copy is compared, never written back.
- **clrnd does not manage revision names**: `init` drops them and `diff` ignores a live one the
  manifest does not pin. `refresh` is the one exception.
- **One polling loop.** Every wait goes through `poll` with a `poller`; do not write another loop.
  `WaitOptions.Timeout` 0 means no limit everywhere (the CLI's 10-minute default is the flag's), and
  with no generation given a wait pins the one its first read reports. Retries follow `isRetryable`.
- **Lists are complete or an error.** Paging goes through `listAll`, which errors rather than return
  a partial list.
- **Files clrnd writes go through [cmd/write.go](cmd/write.go)**, never `os.WriteFile`.
- **`--image` is the only field a flag may override**, and only on `verify`/`diff`/`deploy`.

## Adding a resource kind

Jobs and worker pools were added this way; follow the same shape for the next one:

1. **Probe the real API first**, as jobs and worker pools were: compare-and-swap on `resourceVersion` (409/400), a
   dry run that returns the defaults, `observedGeneration`/`Ready` settling, async deletion, the
   annotations and labels the server manages (they go into the `serverManaged*` lists in
   cloudrun.go), and every integer field where 0 might differ from leaving it out — the generated
   types drop a 0 unless the field is in `ForceSendFields` (see `maxRetries` in job.go).
2. **internal/cloudrun:** a `kindOf[T]` with the API calls, `apiVersion`, `meta`, a `workload`
   extractor and `status`, plus the hooks it needs. A kind holding a `RevisionTemplate` supplies
   `template` and reuses [template.go](internal/cloudrun/template.go) for `align`/`scaffold`; rules
   only this kind has go in `check` (a worker pool's instance count). Add the type to `resource`
   and the kind to `kindHints`.
3. **cmd:** a `target` in [cmd/target.go](cmd/target.go), added to `targets`
   (`TestTargetsAreComplete` fails on a missing field), a command group with `Args: cobra.NoArgs`
   and a `RunE` that prints the help (otherwise a mistyped subcommand exits 0), and a config key in
   [internal/config](internal/config/config.go).
4. **Docs and E2E:** README (commands, permissions table), a phase in test/e2e.

## CLI conventions

- **stdout is data only.** Diffs, manifests, JSON and execution names go to stdout; status lines,
  warnings, prompts and errors go to stderr. Commands that confirm print their target
  (`printTarget`) and prompt on stderr.
- **Mutations share one flow.** Commands that apply a plan use `addApplyFlags` and `applyPlan`
  (diff → confirm → apply → wait; an empty diff still waits). Commands with no plan to show
  (`delete`, `revisions --prune`, `jobs executions cancel`) use `confirmAction`: no TTY and no
  `--auto-approve` means refuse. `jobs run` has no prompt.
- **Exit codes** are in [cmd/exit.go](cmd/exit.go): 0 success, 1 error, 2 for `diff --exit-code`
  differences (printed without an `Error:` line).
- **Flags.** `-o`/`--output` is a file to write; `--format text|json` is an output format. Validate
  values before building the client. `--timeout`/`--interval` go through `validateWaitFlags`.
- **`--format json` is a published interface.** A renamed key keeps its old name alongside, as
  `revisions` keeps `image` and `jobs status` keeps `completed`.
- **Positional arguments** are `[name] [manifest]`; each falls back to the config file. A name that
  is not given and a config that names another kind is an error (`target.resolveName`).
- **Adding a subcommand:** a `newXCmd(...)` factory with its flags in an options struct, registered
  on its parent; build it per target when another kind may need it.

## Testing

- API calls are tested against a fake API: `NewClient` appends its `opts` after the default
  endpoint, so tests inject `option.WithEndpoint` + `option.WithHTTPClient` (`newTestClient` in
  internal/cloudrun, `startFakeAPI` + `clientOptions` in cmd). Add a test for every new API call.
- `executeRoot` runs the root command with stdin pinned to an empty reader and the target
  environment cleared, so results do not depend on the terminal or the developer's environment.
- A test meant to fail inside `RunE` must get past `loadConfig` first; assert on the error text so
  it cannot pass by failing earlier.
- Behaviour-preserving changes: check that a new test fails without the change, or say why it
  passes on both sides.

## CI and release

- Checks live in [.github/workflows/verify.yml](.github/workflows/verify.yml), called by both
  `ci.yml` and `release.yml`; add new checks there.
- Every checkout sets `persist-credentials: false`.
- Pinned tool versions are bumped by hand in every place they are written;
  [.github/scripts/check-tool-pins.sh](.github/scripts/check-tool-pins.sh) fails when the copies
  disagree (it reads the golangci-lint line in this file too). That includes the runner image:
  every job names the same Ubuntu release, never `ubuntu-latest`.
- Releases are cut from `main` only: `v*` tags, a guard that the commit is on `main`, the same
  checks as CI, then GoReleaser, signing, provenance and packslip. The workflow comments explain
  each choice.

## Writing rules

- Everything written in the repository is in **English**: user-facing strings, code comments (Go,
  shell, YAML), test names and messages, and documentation. Translate any Japanese comment left from
  before this rule rather than editing it in Japanese.
- Commit messages, issues, PR titles and descriptions, and review replies are in **English**. Older
  history is in Japanese; leave it. The PR title becomes the squash commit's subject on `main`.
- **Do not write a count unless the number itself matters.** "Twelve subcommands", "three things to
  know", "CI's third job" summarize a list and go stale silently (README said `eleven` long after
  `traffic` made it twelve, #114). Name the items or say "the following". "Both" over a list that
  can grow has the same problem — say "each" or "all of them". Keep a number when it *is* the fact:
  a timeout, a page limit, a two-way split, "exactly one container".

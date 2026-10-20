# End-to-end test

The Go tests build and execute the clrnd binary against a real Cloud Run project. They create a
service, a job and a worker pool, deploy and run them, inspect their state independently, and
remove them. These are external CLI tests: they do not call clrnd's internal implementation. The
live state is read straight from the Cloud Run API with Google's generated Go client, never through
clrnd's code; gcloud pins a revision as a writer other than clrnd and deletes orphaned services and
jobs.

**This creates real, billable resources.** Use a project intended for disposable test resources.
The `e2e` build tag excludes the suite from ordinary `go test ./...`. The cloud test also skips
unless `CLRND_E2E_CLOUD=1` is set, which `run.sh` does, so `go test -tags=e2e ./...` or an IDE's
test runner cannot create resources by accident. Running it also requires an explicit project, from
`PROJECT` or the git-ignored `project.env` next to this file. CI runs only
the offline harness tests, not the cloud scenarios.

## Setup

- Go (the version in the repository's go.mod) and git on PATH
- gcloud on PATH, authenticated both for its CLI and for Application Default Credentials
- The Cloud Run API enabled and permission to deploy, run and delete resources in the target project
- `PROJECT=<project-id>`, or a project ID on the first line of `test/e2e/project.env`

No Python is needed.

## Credentials

By default, clrnd and the test's own API reads use ADC, and gcloud uses its own active login. Each
identity is checked before creating resources. A dedicated config directory prevents another terminal's gcloud login or
configuration changes from changing the test's identity. A service account also bounds its roles.

Create an impersonated account once (replace the placeholders):

```sh
gcloud iam service-accounts create clrnd-e2e --project <project>
SA=clrnd-e2e@<project>.iam.gserviceaccount.com
gcloud projects add-iam-policy-binding <project> --member "serviceAccount:$SA" --role roles/run.developer
# Let the tester deploy as the runtime account (normally the default compute account).
gcloud iam service-accounts add-iam-policy-binding <runtime-service-account> \
  --member "serviceAccount:$SA" --role roles/iam.serviceAccountUser
gcloud iam service-accounts add-iam-policy-binding "$SA" \
  --member "user:<you>" --role roles/iam.serviceAccountTokenCreator

export CLOUDSDK_CONFIG=~/.config/gcloud-clrnd-e2e
gcloud auth login
gcloud config set auth/impersonate_service_account "$SA"
gcloud auth application-default login --impersonate-service-account "$SA"
```

Set `CLRND_E2E_GCLOUD_CONFIG` to that directory, or write the path into the git-ignored
`test/e2e/gcloud-config.env`. A relative environment path is relative to the caller's directory;
a relative path in the file is relative to that file. The harness passes absolute
`CLOUDSDK_CONFIG` and `GOOGLE_APPLICATION_CREDENTIALS` values to each child process. Dedicated ADC
must be a service account, including an impersonated one. A mismatch between the gcloud and ADC
accounts is reported. No service account key file is needed. Reauthenticate in the same directory
when the source login expires.

The gcloud preflight checks that it can list services and jobs; clrnd must receive an API 404 for
a deliberately absent service. Listing failures are errors, including during orphan cleanup. API
reads retry a rate limit or a server error, and treat only a 404 as absent.

## Usage

The existing entry point remains a small launcher; all test logic is Go:

```sh
PROJECT=<project-id> ./test/e2e/run.sh
ONLY=current ./test/e2e/run.sh         # service scenario only
ONLY=jobs ./test/e2e/run.sh            # job scenario only
ONLY=worker-pools ./test/e2e/run.sh    # worker pool scenario only
OLD_REF=<git-ref> ./test/e2e/run.sh     # also observe the old binary's revision-name behavior
ONLY=old OLD_REF=<git-ref> ./test/e2e/run.sh
KEEP=1 ./test/e2e/run.sh               # retain resources and scratch files
RAW=1 ./test/e2e/run.sh                # show unredacted identifiers
REGION=<region> ./test/e2e/run.sh      # default: asia-northeast1
WORK_ROOT=<directory> ./test/e2e/run.sh
CLRND_E2E_GCLOUD_CONFIG=<directory> ./test/e2e/run.sh
./test/e2e/run.sh --cleanup-orphans
```

The equivalent direct command, also usable without bash, is:

```sh
CLRND_E2E_CLOUD=1 PROJECT=<project-id> go test -tags=e2e -count=1 -timeout=0 -v ./test/e2e -run '^TestE2E$'
```

Without `CLRND_E2E_CLOUD=1` the test reports `SKIP` and touches nothing.

Use `ONLY` to select cloud scenarios. Go's `-run` filter can also select a subtest, but setup still
builds and checks credentials. The default Go test timeout is disabled by the launcher: individual
CLI calls are bounded, including explicit timeouts on deliberately failing rollouts. Ctrl-C cancels
test commands and attempts cleanup with a separate bounded context. A second interrupt restores
the default kill. Forced termination, including `kill -9`, may leave resources; use orphan cleanup.

Resources are named `clrnd-e2e-<timestamp>-<random-id>`, `<service-name>-job` and
`<service-name>-wp` (a worker pool cannot share a service's name). Ordinary cleanup only deletes
this run's names. `--cleanup-orphans` deliberately deletes every service, job and worker pool with
the `clrnd-e2e-` prefix in the configured project and region; do not run it while another test is
using that project. `KEEP=1` does not affect this explicit orphan-cleanup command. Services and
jobs are removed with gcloud alone; worker pools go through the API (gcloud releases before its
worker-pools commands are still common), so without a usable ADC they are reported as not checked.

Each run uses a private `clrnd-e2e-work.*` directory under `WORK_ROOT` (default: the OS temporary
directory). It is removed on success and retained on failure or with `KEEP=1`; the log prints its
location. It includes manifests and an unredacted `commands.log` with stdout, stderr, exit codes and durations of every command and API read.
**Do not paste these files into issues: they contain real identifiers.** Console output masks
project IDs, generated resource names, account addresses, service URLs and long numbers unless
`RAW=1` is set. Build output remains outside the checkout to avoid cloud-sync clients reverting
binaries during a build.

`IMAGE` and `JOB_IMAGE` can override the sample images. Service image checks expect an Artifact
Registry reference without an explicit tag or digest, so a nonexistent tag can be appended. The
job image must support Google's sample job behavior: `SLEEP_MS` delays completion and `FAIL_RATE=1`
forces failure.

## Coverage and structure

- `services_test.go`: explicit target resolution; deploy, status, wait and revisions; server-default
  diffs; a gcloud-pinned revision followed by init and a template change; refresh and its refusal
  cases; rollback; traffic splitting without creating a revision; deploy with no traffic followed
  by promotion; verify text/JSON and pinned-name warnings; rejection of a conflicting revision and
  its retry; Artifact Registry checks and image overrides; prune protection; dry-run and real delete.
- `render_test.go`: Terraform state lookup, env/must_env, render output files, input-overwrite
  refusal, and json_escape through YAML and embedded JSON.
- `jobs_test.go`: verify/deploy/status/wait; explicit zero retries; diff with and without defaults;
  cross-kind refusals; init and template changes; successful and failing executions; deferred waits,
  timeout and no-wait behavior; execution lists in text/JSON; cancel refusals and completion; deletion
  while an execution is still running.
- `workerpools_test.go`: the refused `scalingMode: manual` without a count; verify, deploy, status
  text/JSON, wait and a clean diff; init with a clean diff with and without defaults; a count-only
  change that creates no revision; a count set the way an autoscaler sets it, kept by a deploy of a
  manifest that leaves the count out; revisions and a rollback that moves the instances without
  creating a revision, then a prune that spares both the revision running and the latest created;
  deploy `--no-promote` creating a revision that runs nothing, `instance-split --to-latest
  --percent 50` splitting the instances with the revision running them, and `--to-latest` moving
  them all; cross-kind refusals; dry-run and real delete.
- `services_test.go`'s comparison scenario: builds `OLD_REF` from a git archive, pins a live revision,
  scaffolds with the historical binary, then records its behavior after changing the template. Old
  versions may fail synchronously or asynchronously, or succeed; this phase intentionally observes
  rather than requires a historical bug.
- `harness_test.go`: build, preflight, scenario selection and cleanup; `settings_test.go`: target
  and credential setup; `command_test.go`: process execution, assertions and redaction;
  `api_test.go`: the Cloud Run API reads and cleanup deletes; `cloud_helpers_test.go`: live-state
  checks built on them; `manifest_test.go`: YAML editing and archive extraction.
  Child processes receive explicit `Dir`/`Env` and empty stdin; output streams are checked
  separately. A failed exit code or live-state check stops its scenario, while the other scenarios
  still run; a wording check on output is recorded and the scenario continues, so one run reports
  every message that changed.
- The service scenario (`TestE2E/service/current`, then `TestE2E/service/comparison`), the job
  scenario (`TestE2E/jobs`) and the worker pool scenario (`TestE2E/worker-pools`) run in parallel:
  they own separate resources and work directories.
  Console lines carry the subtest name, and every `commands.log` entry starts with it. Cleanup runs
  after every scenario has finished.
- `helpers_test.go`: offline regression checks for the harness. Run them without any credentials:

```sh
go test -tags=e2e ./test/e2e -run '^TestHarness' -count=1
```

When adding a scenario, preserve the external CLI boundary and check live state through the API
reads in `api_test.go` where output alone cannot prove an effect; each gcloud call costs more than a
second of start-up. Add it to the coverage list above. A scenario that
shares a resource with another must run inside that scenario's subtest, as the comparison does;
give it its own resource and directory to run it in parallel.

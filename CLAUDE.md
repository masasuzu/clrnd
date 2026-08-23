# CLAUDE.md

This file provides shared guidance to Claude Code and OpenAI Codex when working with code in this repository.

## Overview

`clrnd` is a Go CLI for deploying services to Google Cloud Run. It takes a service name and a
manifest file (Knative-style Service YAML) and exposes `verify`, `render`, `diff`, `deploy`,
`init`, `status`, `wait`, `revisions`, `rollback`, `delete`, and `refresh` subcommands. All eleven
are implemented, which is the whole ecspresso-shaped set Cloud Run's model allows. (`init` was formerly `load`; `load`
remains a cobra alias for `init`.) The subcommand set deliberately tracks ecspresso where Cloud Run's model allows
(ECS-only commands like `register`/`exec`/`scale` have no Cloud Run analog and are not added).

## Commands

```sh
go build ./...          # build all packages
go run . init <svc> --project <P> --region <R>   # run without installing
go install .            # install the clrnd binary to $GOPATH/bin
go test ./...           # run tests
go test -run TestName ./internal/cloudrun   # run a single test
go vet ./...            # static checks
gofmt -w .              # format
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2 run ./...   # lint (CI runs this)

# End-to-end test against a real Cloud Run project. NOT part of `go test ./...`
# and it cannot run in CI (no credentials). See test/e2e/README.md.
PROJECT=<project-id> ./test/e2e/run.sh
./test/e2e/run.sh --cleanup-orphans   # remove services a killed run left behind
```

[test/e2e](test/e2e/) holds a shell-driven end-to-end test that creates and deletes a real Cloud
Run service. The directory has no Go files, so the whole Go toolchain ignores it; it is opt-in and
refuses to run without an explicitly configured project. Run it by hand when changing anything that
talks to the Cloud Run API — it is the only check that covers server-side behaviour (defaulting,
revision-name conflicts, asynchronous rollout failures).

## Architecture

- Entry point [main.go](main.go) calls `cmd.Execute()` and exits non-zero on error.
  `Execute` builds a `signal.NotifyContext` (SIGINT/SIGTERM) and calls `ExecuteContext`, so every
  subcommand must use `cmd.Context()` (never `context.Background()`) to stay interruptible. It also
  releases the handler once the context is cancelled (`go func() { <-ctx.Done(); stop() }()`) so a
  second Ctrl-C restores the default kill — without that, `NotifyContext` swallows every later
  signal and code that ignores the context becomes unkillable. Anything that blocks (a prompt, a
  poll) must select on `ctx.Done()`; `confirm` in [cmd/flags.go](cmd/flags.go) does this by reading
  stdin in a goroutine.
- [cmd/root.go](cmd/root.go) defines the cobra root command and registers every subcommand in
  its `init()`. Each subcommand lives in its own file (`cmd/<name>.go`) as a package-level
  `*cobra.Command` var, following the standard cobra layout.
- Invocation form is `clrnd <subcommand> [service] [manifest]`. Positional args are optional and
  `resolveService`/`resolveManifest` fill them from the config file when absent (positional →
  config); args fill service first, then manifest. Only the three commands that take **both** a
  service and a manifest use `cobra.MaximumNArgs(2)`: `verify`, `diff`, `deploy`.
  Every other command uses `MaximumNArgs(1)`: `init`, `status`,
  `revisions`, `rollback`, `delete`, `refresh`, `wait` (service only) and `render` (**manifest
  only** — `Use: "render [manifest]"`, so `clrnd render my-svc manifest.yaml` is an error).
  `render` does not match the service name, so it calls `resolveManifestAt(args, 0)` rather than
  `resolveManifest`.
- All Cloud Run access and manifest handling lives in [internal/cloudrun](internal/cloudrun/cloudrun.go).
  Subcommands in `cmd/` only parse flags and do I/O, then call into this package.
- `clrnd --version` is served by [cmd/version.go](cmd/version.go): the `version` var is filled by
  goreleaser's ldflags (`-X github.com/masasuzu/clrnd/cmd.version=...`), falling back to
  `debug.ReadBuildInfo` (for `go install module@version`) and then `(devel)`.
- Manifests are rendered as Go `text/template` by [internal/render](internal/render/render.go)
  BEFORE parsing/validation. `verify`/`render`/`diff`/`deploy` call `renderManifest` (in
  [cmd/flags.go](cmd/flags.go)) right after `os.ReadFile`. The `render` subcommand prints this
  rendered output as-is (no parse/normalize), for debugging template output. Template funcs (ecspresso-compatible):
  `{{ tfstate "addr" }}`, `{{ tfstatef "fmt" args }}`, `{{ env "VAR" ["default"] }}`,
  `{{ must_env "VAR" }}`. The `tfstate`/`tfstatef` funcs resolve Terraform state via
  `fujiwara/tfstate-lookup`; states are declared with the repeatable
  `--tfstate <location>|<name>=<location>` flag and lazy-loaded (a state is only read when a
  placeholder references it, so manifests without placeholders need no `--tfstate`). A *named* state
  follows ecspresso's `func_prefix` model: the `<name>` is used verbatim as the function-name prefix,
  so `--tfstate net_=<loc>` registers `{{ net_tfstate "addr" }}` / `{{ net_tfstatef "fmt" args }}`
  (NOT a 2-arg `{{ tfstate "name" "addr" }}` form, which does not exist). `<name>` must be a valid Go
  identifier prefix (`^[A-Za-z_][A-Za-z0-9_]*$`); this is validated in `render.Render` via
  `render.IsValidName` (a clean error, not a `template.Funcs` panic) so it covers BOTH the flag and
  config paths. The flag parser (`parseTfstateSources`) also uses `render.IsValidName` to decide
  whether `name=` is a name or part of the location.
  Per-state registration in `render.Render` means referencing an unconfigured prefix is a
  `text/template` parse error ("function ... not defined"), matching ecspresso. `'` in an address is
  rewritten to `"` for convenience. `init` takes no manifest, so it is not rendered.

### internal/cloudrun (the core logic)

- Cloud Run API calls go through `Client` ([internal/cloudrun/client.go](internal/cloudrun/client.go)),
  which holds the `*run.APIService` plus the target project/region so callers do not pass them
  around. `NewClient(ctx, project, region, opts ...option.ClientOption)` appends `opts` **after**
  the default endpoint option, which is the injection point tests use
  (`option.WithEndpoint(httptestURL)` + `option.WithHTTPClient`) — see `newTestClient` in
  [internal/cloudrun/client_test.go](internal/cloudrun/client_test.go) and `startFakeAPI` in
  [cmd/integration_test.go](cmd/integration_test.go). Add new API calls as `Client` methods, and
  cover them against the fake API rather than leaving them untested.
  `VerifyRemote` ([internal/cloudrun/verify.go](internal/cloudrun/verify.go)) builds its own IAM /
  Secret Manager / Artifact Registry clients but takes the same variadic `option.ClientOption`, so
  `cmd` passes `clientOptions` through and one fake server can answer all three APIs (they are
  routed by path). It takes **no region**: the Artifact Registry location is part of the image
  reference, and neither IAM nor Secret Manager is regional.
  It looks the service account up as `projects/-/serviceAccounts/<email>`: Cloud Run allows a
  service account from **another** project, and pinning the project would turn a valid setup into a
  404, which `RemoteCheck.Missing` reports as a hard failure.
  It also checks `containers[].image` against Artifact Registry
  ([internal/cloudrun/image.go](internal/cloudrun/image.go) parses the reference; `checkImages` in
  `verify.go` makes the call). Only `<location>-docker.pkg.dev` is checkable, and the location and
  project come from the *image*, not from the target — a digest goes to `dockerImages.get` and a tag
  to `packages/<path>/tags/<tag>`, with the image path's `/` written as `%2F` **exactly once** (the
  API returns names in that form and rejects a double-escaped one with a 404). Images on other
  registries are **skipped silently, not reported as `Unchecked`**: warning about every Docker Hub
  image would put a `warning:` line on every run and train people to ignore them. Confirmed against
  the real API: a missing repository/package/tag/digest is a `404`, a project that does not exist or
  cannot be read is a `403` (so a typo in the project never becomes a false `Missing`), and a public
  image such as `us-docker.pkg.dev/cloudrun/container/hello` is readable with ordinary ADC.
  Auth is **Application Default Credentials**, picked up automatically by `run.NewService`
  (`google.golang.org/api/run/v1`). The user runs `gcloud auth application-default login` once;
  no credentials are passed explicitly.
- `cmd/` builds the client via `newCloudRunClient` in [cmd/flags.go](cmd/flags.go), which resolves
  project/region and passes the package var `clientOptions` (empty in production, set by tests).
  Create the client **after** all the local work — reading/rendering the manifest, checking for
  existing files, **and validating** — so local errors surface before target resolution and
  credential discovery. `deploy` therefore calls `cloudrun.Validate` itself before building the
  client, even though `Plan` validates again (the check is pure and cheap); otherwise a manifest
  problem hides behind "project is required" or "could not find default credentials".
- Deploy is split into `Client.Plan` (parse + validate the manifest, then delegate) and
  `Client.PlanService` (`Get` the live service, compute the `Diff` of live vs desired; `Create` when
  404 via `isNotFound`/`googleapi.Error`). Commands that edit the live definition rather than a
  manifest — `rollback`, and `refresh` — go straight to `PlanService`. `PlanService` also copies the
  `resourceVersion` of the service it just fetched onto `desired` (`setResourceVersion`), so **every
  write is a compare-and-swap against the exact state the diff was computed from**. Verified against
  the real API: a write with no `resourceVersion` is accepted as an unconditional overwrite (so two
  concurrent deploys silently lose one of the changes), a stale-but-well-formed one gets a `409`,
  and a malformed one gets a `400` — which is why nothing may ever be invented for the field.
  `Apply` translates that 409 (`isConflict`) into "changed after the diff was computed; re-run",
  keeping the API message. `DeployPlan.Apply`
  (the actual `Create`/`ReplaceService`). The print → confirm → apply → wait sequence itself lives
  in `applyPlan` ([cmd/apply.go](cmd/apply.go)), shared by every mutating command; `cmd/deploy.go`
  only builds the plan and hands it over. `applyPlan` prints `plan.Diff` (stdout), then `confirm`s
  on stderr unless `--auto-approve` or `--dry-run`; a non-interactive stdin
  (`isInteractive` via `os.ModeCharDevice`) without `--auto-approve` refuses to apply. An empty diff
  skips the apply but still waits (see the `cmd/apply.go` entry under Conventions).
  `--dry-run` passes `dryRun=all` for server-side validation with no mutation; when it
  is off the `DryRun` setter is **not** called at all (passing `""` would send an empty `dryRun=`
  query parameter).
- The v1 namespaces API requires a **regional endpoint** (`https://<region>-run.googleapis.com`
  via `option.WithEndpoint`), so a region is mandatory.
- `--project`/`--region` are registered via `addTargetFlags` in [cmd/flags.go](cmd/flags.go) and
  resolved by `resolveProject`/`resolveRegion` with precedence **flag → env → config file**
  (matching gcloud): env vars are `CLOUDSDK_CORE_PROJECT`→`GOOGLE_CLOUD_PROJECT` and
  `CLOUDSDK_RUN_REGION`→`GOOGLE_CLOUD_REGION`; the config file (see below) is the lowest fallback.
  Error if none is set. NOT `MarkFlagRequired` (that would reject the env/config-only case).
  `render` needs neither; `verify` accepts them optionally — `resolveTargetOptional` (in
  [cmd/flags.go](cmd/flags.go)) resolves with the same precedence but returns `ok=false` instead of
  erroring, so the API existence check runs only when a target is available.
- The `-c`/`--config` persistent flag loads a YAML config via [internal/config](internal/config/config.go)
  in the root's `PersistentPreRunE` (`loadConfig`), into the package var `cfg`. When `--config` is
  omitted it auto-detects `clrnd.yml`/`clrnd.yaml` in the cwd (absent → empty config, not an error;
  an explicit missing `--config` IS an error — except for `init`, which *writes* the config: it
  carries `Annotations[annotationConfigOptional]`, so `loadConfig` reads an explicit `--config`
  only when it already exists). Config holds `project`, `region`, `service`,
  `manifest`, and `tfstate` (list of `{name, location}`). For `--tfstate`, a CLI flag (if any)
  replaces the config list, otherwise the config list is used. Relative paths from the config
  (`manifest`, local `tfstate` locations) are resolved against the config file's directory via
  `resolveConfigPath` (`configDir` is set in `loadConfig`); CLI-arg paths stay cwd-relative.
- `sanitizeMap` strips server-managed read-only fields (`status`, the `serverManagedMetaFields` of
  `metadata` — `uid`, `resourceVersion`, `ownerReferences`, … — and the `serverManagedAnnotations` /
  `serverManagedLabels` of **both** `metadata` and `spec.template.metadata`), and drops
  `spec.template.metadata` entirely when nothing is left in it (local manifests normally have no
  template metadata, so an empty `metadata: {}` would be a diff line that never goes away).
  Anything missing from those slices becomes a diff that never goes away under
  `--no-server-defaults` (with server defaults on, the dry-run response fills it back in on both
  sides and hides the omission — which is why the `--no-server-defaults` path is the one the E2E
  asserts against). `run.googleapis.com/client-name` / `client-version` are treated as
  server-managed on purpose: they record which tool last wrote, not configuration, so keeping them
  would bake "gcloud" into a manifest `init` produced.
  `ToManifest` applies it to a fetched service. YAML is produced with `sigs.k8s.io/yaml`
  (JSON tags → YAML), which sorts keys alphabetically.
- `Diff` returns a unified diff (via `go-difflib`) of two manifests, empty when identical.
  **`compareServices` is the single comparison path**: it aligns the desired definition with the
  live service, normalizes both through `ToManifest`, and diffs them (a nil `current` means the
  service does not exist yet, so everything is an addition). `Client.CompareManifest` (used by
  `diff`) and `Client.PlanService` (used by `deploy`, `rollback`, `refresh`) both go through it, so
  the commands can never drift apart. Both also run the same pre-processing before anything is sent
  — `setNamespace` puts the target project on the body — because while server defaults are being
  resolved (the default; `--no-server-defaults` turns it off) the diff path performs a real
  (dry-run) write and gets the same validation as `deploy`.
  `CheckSyntax` is the strict-parse-only check `cmd/diff.go` runs *before* building the client, so
  a manifest problem is not hidden behind a credentials error.
- **Server defaults** (`--no-server-defaults`, `PlanOptions.ResolveDefaults`): Cloud Run fills in a lot
  of fields on create (`containerConcurrency`, container `ports`, `resources.limits`, `startupProbe`,
  `serviceAccountName`, `timeoutSeconds`, `spec.traffic`, several annotations and labels), so a
  hand-written minimal manifest never diffs clean (issue #11). A `dryRun=all` write returns the
  service *with those defaults applied* — verified against the real API — so `resolveDefaults` sends
  the desired definition through one and compares that instead. Two rules matter: it is **on by
  default** (like `kubectl diff`), which means `diff` now needs permission to update the service —
  `--no-server-defaults` is the way out for read-only credentials, and the flag is negative to match
  `--no-wait`; and the resolved copy is used **only for the diff** — `plan.desired` stays the original, so
  clrnd never writes back values the server computed (which would pin today's defaults forever).
  `CompareManifest` runs the same `validate` as `deploy` **regardless of the option**, so the two
  commands agree on what a valid input is; scoping it to the dry-run path let `diff` render a
  "you can rename a service" diff that `deploy` would refuse. It also mirrors `PlanService`'s 404
  handling (`current = nil`, everything is an addition), so `diff` works on a service that does not
  exist yet — and passes `create` to `resolveDefaults`, since a *replace* dry run would 404 too. `resolveDefaults` wraps failures without claiming a cause — permission is the likely
  one, but a rejected manifest or a service deleted mid-run look the same from here.
- **Revision names**: `spec.template.metadata.name` is optional on write (Cloud Run generates one),
  and a name Cloud Run generated is **never echoed back** — the field is only present on read when a
  client set it (`gcloud run deploy --revision-suffix`, a Terraform `template.metadata.name`, or a
  manifest that pins one). An existing revision name cannot be reused with a different
  configuration: doing so fails with a 409, sometimes only when the rollout runs. clrnd therefore
  does not manage revision names: `WithoutRevisionName` (pure — it shallow-copies the
  Spec/Template/Metadata chain rather than mutating its argument) removes the field from what
  `init` scaffolds, and `alignRevisionName` (called from `compareServices`) drops the live value
  from the comparison
  **when the local manifest does not pin one** — without that, every manifest without a revision
  name would show a permanent diff. When the manifest *does* pin a name it is kept on both sides
  and shows up as a normal diff, and **both `verify` and `deploy`** warn (`warnPinnedRevision` in
  [cmd/flags.go](cmd/flags.go), via `RevisionName`) that the next template-changing deploy will
  fail with a 409. `deploy` warns too because a CI job that only runs `deploy` would otherwise see
  nothing but the opaque API error. The warning is not a failure: pinning is legitimate for a
  one-shot deploy.
- `Validate` checks a local manifest with no API access: strict YAML unmarshal into `run.Service`
  (catches unknown/misspelled fields), required-field checks, and that `metadata.name` matches the
  service argument. Returns `errors.Join` of all problems so the user sees them at once. The local
  `Validate` needs no `--project`/`--region` and no credentials.
- `VerifyRemote` (in [internal/cloudrun/verify.go](internal/cloudrun/verify.go)) complements
  `Validate` with API existence checks, aligning `verify`'s semantics with ecspresso. It confirms
  the manifest's service account (IAM `projects.serviceAccounts.get`) and the Secret Manager secrets
  used by `secretKeyRef`/secret volumes (`projects.secrets.get`) exist. It returns a `RemoteCheck`
  that separates `Missing` (404 — resource confirmed absent, fails verify) from `Unchecked`
  (client-init failure, permission denied, API disabled — could not decide, surfaced by
  `cmd/verify.go` as a `warning:` on stderr and NOT a failure). This split keeps an ambient
  project/region in CI from turning a passing offline lint red. Auth is the same ADC; the IAM/Secret
  Manager clients are subpackages of `google.golang.org/api` (no new module). `cmd/verify.go` runs it
  only when a target resolves and `--local-only` is off. When a target does not resolve it warns
  **only if `--project` or `--region` was passed as a flag** (`cmd.Flags().Changed`); a half-set
  environment is skipped silently, so an ambient `CLOUDSDK_*` in CI does not produce noise on every
  run. `cmd/verify.go` still requires *both* project and region to resolve before running the
  remote checks, even though `VerifyRemote` no longer takes the region: a half-configured target is
  more likely a mistake than an intent, and quietly reaching for whatever project is ambient is the
  worse failure.
- `refresh` (in [internal/cloudrun/refresh.go](internal/cloudrun/refresh.go)) re-applies the **live**
  definition unchanged so a new revision is created — it never reads a local manifest.
  This is the **one deliberate exception** to "clrnd does not manage revision names": Cloud Run only
  creates a revision when `spec.template` changes, so forcing a rollout requires naming the revision
  explicitly (`<service>-r<UTC timestamp>`, or `--revision-suffix`). The name is dropped again by the
  next `deploy` from a manifest, and `alignRevisionName` keeps `diff` clean in the meantime because
  the local manifest pins nothing. `validateRevisionName` rejects names the API would reject, using
  the constraints confirmed against the real API: only lowercase letters, digits and hyphens,
  starting with a letter, not ending with a hyphen, and shorter than 64 characters. It does **not**
  check the `<service>-` prefix Cloud Run also requires — that one is guaranteed by construction in
  `RefreshTarget`, which builds the name as `<service>-<suffix>`.
  `RefreshTarget` also refuses two situations where the command would succeed without doing its job:
  a name equal to the one already on `spec.template` (no new revision is created, so the diff is
  empty and `applyPlan` reports "No changes."), and a service whose `spec.traffic` pins every target
  to a specific revision (`servesLatestRevision`) — the state `rollback` leaves behind, where a new
  revision would be created but serve nothing.
- `delete` (in [cmd/delete.go](cmd/delete.go)) is the one command that destroys something, so it
  fetches the service first: a missing service fails before any prompt, and what is about to go is
  printed to stderr **with the project and region** — the realistic accident is deleting the right
  service name in the wrong project. It does not use `applyPlan` (there is no diff to show) but
  shares `confirmAction`, so the "no TTY and no `--auto-approve` → refuse" rule is identical.
  `--dry-run` skips the prompt because nothing is destroyed, matching `deploy`.
  **Deletion is asynchronous too**: the DELETE returns while the service is still readable for a
  while, so `delete` polls with `Client.WaitDeleted` until a 404 comes back (`--no-wait` opts out).
  Without it, "delete then recreate" races. `WaitDeleted` shares `waitDefaults`/`nextWaitInterval`
  and the same transient-error tolerance as `Wait`.
- `rollback` (in [internal/cloudrun/rollback.go](internal/cloudrun/rollback.go)) repoints
  `spec.traffic` at an earlier revision. It touches **only** the traffic split — `spec.template` is
  left alone, so no new revision is created and the revision-name conflict cannot apply. Traffic
  tags are kept (pinned at 0%) so a rollback does not silently remove tag URLs; untagged entries
  (including `latestRevision`) are dropped because the target now takes all of it.
  `RollbackTarget` shallow-copies the Spec rather than mutating the live service.
  `SelectRollbackRevision` picks the first ready revision *older* than the **newest revision that is
  serving any traffic** — not the one with the largest share. Mid-canary (new 10% / stable 90%) the
  largest share is the *stable* revision, so choosing by share rolls straight past the known-good
  version. This relies on a Cloud Run behaviour worth knowing: a revision that has lost all
  its traffic stays `Ready=True` (with `Reason: Retired`) and only its `Active` condition flips to
  `False`, so "the previous working version" is still findable.
- `revisions` (in [internal/cloudrun/revisions.go](internal/cloudrun/revisions.go)) is read-only.
  Traffic shares live on the **Service** (`status.traffic`) while the revisions themselves come from
  `Namespaces.Revisions.List`, so `ListRevisions` fetches both and joins them; a revision can appear
  in `status.traffic` more than once (a percentage entry plus a tag entry), so the shares are summed
  and the tags collected. The list is paged through with the `Continue` token, with two
  guards: it stops when the same token comes back (the next page would repeat the last one) and
  after `listRevisionsMaxPages` pages. Without them a server that keeps returning the same token
  grows `items` without bound, and a `ctx` with no deadline has no way to stop it. Both guards
  **return an error rather than the partial list**: only an empty `Continue` ends the paging
  normally. A truncated list is not just short — the repeated-token case has read the same page
  twice, so it can hold duplicates as well as gaps, and `rollback` picks both the current revision
  and the one to return to out of it, so treating it as complete can roll back to the wrong
  version (or report that there is nothing to roll back to). `newRevisions` is the
  pure conversion and `Revisions.Text()` the pure `text/tabwriter` formatting, both testable without
  the API. Sorting is newest-first by `creationTimestamp`, falling back to the revision name
  (Cloud Run numbers them sequentially) when the timestamp will not parse.
- `wait` (in [internal/cloudrun/wait.go](internal/cloudrun/wait.go)) polls `Client.Status` until
  the rollout settles. `waitDone` is the pure decision function: it refuses to judge until
  `status.observedGeneration` reaches the generation being waited for (otherwise the *previous*
  generation's `Ready=True` reads as success), then succeeds on `Ready=True` and **fails
  immediately** on `Ready=False` rather than burning the timeout. The interval backs off from 2s to
  15s, and the whole loop honours `cmd.Context()` so Ctrl-C stops it.
  A failed *poll* is not a failed rollout: the generated API client does not retry, so a single
  transient 503 would otherwise make an already-applied `deploy` exit non-zero — the very CI
  false-signal this feature exists to remove, inverted. The loop therefore keeps polling through
  errors until the timeout (reporting them via `OnRetry`) and surfaces the last one if it does time
  out. Which errors those are is `isRetryable` (in
  [internal/cloudrun/cloudrun.go](internal/cloudrun/cloudrun.go)), shared by `Wait` and
  `WaitDeleted` so the two cannot drift: 408/429/5xx and any error with no HTTP status (a dropped
  connection, DNS, EOF) are retried, every other 4xx returns at once. Waiting out a 400/401/403
  ends in the same failure ten minutes later, having held a CI job the whole time; 404 falls out of
  the same rule (a service that does not exist will not appear). The one 403 that *is* retried is a
  rate limit — Google's APIs report quota exhaustion as 403 with a `rateLimitExceeded`-style
  `reason`, and that does clear on its own, so `retryableForbiddenReasons` separates it from a
  permission problem.
  `waitProgress` deliberately withholds the `Ready` value until `observedGeneration` reaches the
  awaited generation — the condition visible before then belongs to the *previous* generation and
  reads as "already finished".
  `DeployPlan.Apply` returns the applied `*run.Service` so `deploy` can wait for exactly the
  generation it just applied (`AppliedGeneration`). **`deploy` waits by default** and fails when the
  rollout fails — without that, a broken revision still exited 0 and CI treated it as success.
  `--no-wait` restores the old behaviour. `waitForRollout` in [cmd/wait.go](cmd/wait.go) is the
  shared entry point that prints progress to stderr.
- `status` (in [internal/cloudrun/status.go](internal/cloudrun/status.go)) is read-only. `newStatus`
  is a pure `*run.Service` → `Status` conversion and `Status.Text()` is pure formatting, so the whole
  presentation is testable without the API; `Client.Status` is the thin API wrapper. `Status.Ready()`
  finds the `Ready` condition and is meant to be reused by `wait`. The JSON form is the `Status`
  struct itself (`--format json`), so the two outputs cannot drift apart.
- `init` (in [cmd/init.go](cmd/init.go), formerly `load`) fetches a service via `GetService`/
  `ToManifest` and scaffolds `manifest.yaml` (the `--output` file) plus the config file, refusing to
  overwrite existing files without `--force`. The config goes to `--config` when given, otherwise
  `clrnd.yml` (`initConfigFile`); the recorded `manifest:` is made relative to the config's directory
  by `manifestPathFor`, because `resolveConfigPath` resolves it from there. Both files are written
  `0600` (they can carry plaintext `env[].value`). If the config write fails after the manifest was
  overwritten, `restoreManifest` puts the manifest back (best-effort) so `--force` cannot leave the
  old manifest destroyed with no config to show for it.
- `render -o` also writes `0600`, and refuses (`sameFile`, via `os.SameFile`) to write over the very
  manifest it is rendering — otherwise the template source is replaced by its own output.

## Conventions

- All user-facing strings (cobra `Short`/`Long`, flag usage, error messages) are in **English**.
  Code comments are in Japanese — keep that split.
- `rootCmd.SilenceUsage` is on, so a runtime error prints only the error. Cobra applies that to
  flag and argument errors too, so `SetFlagErrorFunc` adds a one-line `Run '<cmd> --help' for usage`
  instead of the whole block. Note that cobra prints usage with `Println`, i.e. to `OutOrStderr()` —
  in tests that redirect `SetOut` it lands on **stdout**, so assertions about it must look at both
  streams.
- Exit codes live in [cmd/exit.go](cmd/exit.go): `ExitCode` maps an error to a code and `main.go`
  applies it. `diff --exit-code` returns the `errDiffFound` sentinel, which becomes **2**. The split
  (0 clean / 1 error / 2 differences) is `terraform plan -detailed-exitcode`'s, chosen so that
  errors keep 1 and existing scripts are unaffected.
- Subcommands succeed **silently on stdout**: on success they emit only data (e.g. the manifest)
  there, never a confirmation message. Errors are returned from `RunE` so cobra prints them to
  stderr and sets a non-zero exit code. Advisory `warning:` lines (a pinned revision name in
  `verify`/`deploy`, a `VerifyRemote` check that could not be completed) go to **stderr** and do
  not fail the command — stdout stays data-only, which is what the rule protects.
  Exception: the mutating commands are interactive — `deploy`, `rollback`, `refresh` and `delete`
  print the diff (or, for `delete`, what is about to go) to stdout as data and their status/prompt
  lines (`No changes.`, the `[y/N]` prompt, `Aborted.`) to **stderr**; stdout stays data-only. This
  is intentional, not a violation.
- When adding a subcommand: create `cmd/<name>.go` with a `*cobra.Command` var, set `RunE`, and
  register it with `rootCmd.AddCommand` in [cmd/root.go](cmd/root.go).
- Anything that mutates a service shares [cmd/apply.go](cmd/apply.go): `addApplyFlags` registers
  `--dry-run` / `--auto-approve` / `--no-wait` / `--timeout`, and `applyPlan` runs the one flow
  (print the diff to stdout → confirm on stderr → apply → wait for the rollout). Only the prompt
  text differs per command. Do not re-implement that sequence.
  An **empty diff still waits** (for readiness only, with no generation to reach): a deploy that
  failed its rollout leaves live == desired, so a retry with the same manifest produces no diff, and
  returning early there would hand CI a green run over a broken service — the exact failure the wait
  exists to catch. `confirmAction` in the same file is
  the confirmation rule on its own, for destructive commands that have no plan to show (`delete`).
- `executeRoot` in [cmd/integration_test.go](cmd/integration_test.go) pins stdin to an empty
  `strings.Reader`. Without it `cmd.InOrStdin()` falls back to `os.Stdin`, and the confirmation
  tests pass or fail depending on whether `go test` was started from a terminal.
- CI checks live in [.github/workflows/verify.yml](.github/workflows/verify.yml), which is
  `workflow_call`-only: [ci.yml](.github/workflows/ci.yml) calls it on `pull_request`/`push` to
  main, and [release.yml](.github/workflows/release.yml) calls it as `needs:` of the GoReleaser job.
  **Add new checks there, not in `ci.yml`.** Tag pushes do not match `ci.yml`'s branch filter, so
  before this split a tag on an untested (or not-even-on-main) commit produced a fully signed
  release; signing and provenance attest *which commit was built*, never that it passed anything.
  The called workflow checks out the same ref as its caller, so the verified SHA is the SHA
  GoReleaser builds. Every checkout in the repository passes `persist-credentials: false`: these
  jobs compile and run the code from a pull request, and there is no reason for that code to be
  able to read the token `actions/checkout` would otherwise leave in `.git/config` (nothing here
  does an authenticated git operation — GoReleaser authenticates through `GITHUB_TOKEN` in the
  environment). Note the consequence of gating on `govulncheck`: an advisory published after a
  tag blocks re-running that release until the dependency (or the Go toolchain in `go.mod`) is
  bumped.
- Flag naming: `-o`/`--output` means **a file to write to** (`render`, `init`). A machine-readable
  output *format* is `--format text|json` with no shorthand (gcloud's spelling), so the same flag
  name never means two different things. `addFormatFlag`, `validateFormat`, and `writeFormatted` in
  [cmd/flags.go](cmd/flags.go) are the shared pieces — validate the format value **before** building
  the client, like every other local check.

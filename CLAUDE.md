# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

`clrnd` is a Go CLI for deploying services to Google Cloud Run. It takes a service name and a
manifest file (Knative-style Service YAML) and exposes `verify`, `render`, `diff`, `deploy`, and
`init` subcommands. All five are implemented. (`init` was formerly `load`; `load` remains a cobra
alias for `init`.) The subcommand set deliberately tracks ecspresso where Cloud Run's model allows
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
```

## Architecture

- Entry point [main.go](main.go) calls `cmd.Execute()` and exits non-zero on error.
- [cmd/root.go](cmd/root.go) defines the cobra root command and registers every subcommand in
  its `init()`. Each subcommand lives in its own file (`cmd/<name>.go`) as a package-level
  `*cobra.Command` var, following the standard cobra layout.
- Invocation form is `clrnd <subcommand> [service] [manifest]`. Positional args are optional
  (`cobra.MaximumNArgs(2)` for verify/render/diff/deploy, `MaximumNArgs(1)` for init); `resolveService`/
  `resolveManifest` fill them from the config file when absent (positional → config). args fill
  service first, then manifest. `render` does not match the service name, so it calls only
  `resolveManifest` (the `[service]` slot is still accepted for positional consistency).
- All Cloud Run access and manifest handling lives in [internal/cloudrun](internal/cloudrun/cloudrun.go).
  Subcommands in `cmd/` only parse flags and do I/O, then call into this package.
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

- `newClient` builds the API client; `GetService`/`Deploy` share it. Auth is **Application
  Default Credentials**, picked up automatically by `run.NewService`
  (`google.golang.org/api/run/v1`). The user runs `gcloud auth application-default login` once;
  no credentials are passed explicitly.
- Deploy is split into `Plan` (validate locally, `Get` the live service, compute the `Diff` of
  live vs desired; `Create` when 404 via `isNotFound`/`googleapi.Error`) and `DeployPlan.Apply`
  (the actual `Create`/`ReplaceService`). `cmd/deploy.go` prints `plan.Diff` (stdout), then
  `confirm`s on stderr unless `--auto-approve` or `--dry-run`; a non-interactive stdin
  (`isInteractive` via `os.ModeCharDevice`) without `--auto-approve` refuses to apply. Empty diff →
  skip apply. `--dry-run` passes `dryRun=all` for server-side validation with no mutation.
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
  an explicit missing `--config` IS an error). Config holds `project`, `region`, `service`,
  `manifest`, and `tfstate` (list of `{name, location}`). For `--tfstate`, a CLI flag (if any)
  replaces the config list, otherwise the config list is used. Relative paths from the config
  (`manifest`, local `tfstate` locations) are resolved against the config file's directory via
  `resolveConfigPath` (`configDir` is set in `loadConfig`); CLI-arg paths stay cwd-relative.
- `sanitizeMap` strips server-managed read-only fields (`status`, `metadata.uid`,
  `resourceVersion`, server-set annotations/labels — see the `serverManaged*` slices). `ToManifest`
  applies it to a fetched service; `Normalize` applies the same to a local manifest file so the two
  sides are comparable. YAML is produced with `sigs.k8s.io/yaml` (JSON tags → YAML), which sorts
  keys alphabetically.
- `Diff` returns a unified diff (via `go-difflib`) of two manifests, empty when identical. `diff`
  normalizes both the live service and the local manifest before comparing.
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
  only when a target resolves and `--local-only` is off, and warns when only one of project/region is
  set. Image (Artifact Registry) checks are a deliberate future second stage (`region` is already
  plumbed through for them); see the TODO in `verify.go`.
- `init` (in [cmd/init.go](cmd/init.go), formerly `load`) fetches a service via `GetService`/
  `ToManifest` and scaffolds `manifest.yaml` (the `--output` file) plus `clrnd.yml` (project/region/
  service/manifest), refusing to overwrite existing files without `--force`.

## Conventions

- All user-facing strings (cobra `Short`/`Long`, flag usage, error messages) are in **English**.
  Code comments are in Japanese — keep that split.
- Subcommands succeed **silently**: on success they emit only data (e.g. the manifest) to stdout,
  never a confirmation message. Errors are returned from `RunE` so cobra prints them to stderr and
  sets a non-zero exit code. Exception: `deploy` is interactive — it prints the diff to stdout (data)
  and status/prompt lines (`No changes.`, the `[y/N]` prompt, `Aborted.`) to **stderr**; stdout
  stays data-only. This is intentional, not a violation.
- When adding a subcommand: create `cmd/<name>.go` with a `*cobra.Command` var, set `RunE`, and
  register it with `rootCmd.AddCommand` in [cmd/root.go](cmd/root.go).

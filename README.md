# clrnd

`clrnd` is a command-line tool for deploying services to [Google Cloud Run](https://cloud.google.com/run).
It takes a service name and a manifest file as arguments and provides subcommands to verify, render,
diff, deploy, and initialize Cloud Run services.

## Installation

```sh
go install github.com/masasuzu/clrnd@latest
```

Or build from source:

```sh
git clone https://github.com/masasuzu/clrnd.git
cd clrnd
go build -o clrnd .
```

## Authentication

`clrnd` uses [Application Default Credentials (ADC)](https://cloud.google.com/docs/authentication/application-default-credentials)
to access the Cloud Run Admin API. Authenticate once with:

```sh
gcloud auth application-default login
```

## Configuration file

To avoid repeating arguments and flags, put them in a config file and pass it with `-c` /
`--config`. If `--config` is omitted, `clrnd` looks for `clrnd.yml` then `clrnd.yaml` in the current
directory.

```yaml
# clrnd.yml
project: my-project
region: asia-northeast1
service: my-svc            # optional; overridable by the positional argument
manifest: manifest.yaml    # optional; overridable by the positional argument
tfstate:
  - location: gs://my-tf-state/app/default.tfstate        # default state (name omitted): {{ tfstate "..." }}
  - name: network_                                         # prefixed state: {{ network_tfstate "..." }}
    location: gs://my-tf-state/network/default.tfstate
```

Relative paths in the config (`manifest`, and local `tfstate` locations) are resolved relative to
the config file's directory, so the config works from any working directory. Paths passed as CLI
arguments stay relative to the current directory.

With the service and manifest in the config, commands need no positional arguments:

```sh
clrnd deploy -c clrnd.yml              # uses service + manifest from the config
clrnd deploy other-svc -c clrnd.yml    # override just the service (positional args fill service, then manifest)
```

Resolution order (highest first), matching gcloud:

| Setting  | Order |
| -------- | ----- |
| project  | `--project` → `$CLOUDSDK_CORE_PROJECT` / `$GOOGLE_CLOUD_PROJECT` → config `project` |
| region   | `--region` → `$CLOUDSDK_RUN_REGION` / `$GOOGLE_CLOUD_REGION` → config `region` |
| service  | positional `[service]` → config `service` |
| manifest | positional `[manifest]` → config `manifest` |
| tfstate  | `--tfstate` (if any given, replaces config) → config `tfstate` |

## Templating with Terraform state

Manifests are rendered as [Go templates](https://pkg.go.dev/text/template) before they are parsed,
so you can fill placeholders from Terraform state outputs (or any resource attribute) and from
environment variables, using the same notation as [ecspresso](https://github.com/kayac/ecspresso).
This applies to `verify`, `render`, `diff`, and `deploy`.

```yaml
spec:
  template:
    spec:
      serviceAccountName: '{{ tfstate "output.run_service_account" }}'
      containers:
      - image: '{{ must_env "IMAGE" }}'
        env:
        - name: DB_HOST
          value: '{{ tfstate "google_sql_database_instance.main.private_ip_address" }}'
        - name: LOG_LEVEL
          value: '{{ env "LOG_LEVEL" "info" }}'
```

Provide the state location with `--tfstate` (repeatable). A state can be a local path or a remote
URL (`gs://`, `s3://`, …); it is only read when a placeholder actually references it.

```sh
# Single (default) state
clrnd deploy my-svc manifest.yaml --project p --region r \
  --tfstate gs://my-bucket/prod/terraform.tfstate

# Multiple states: the <prefix> in --tfstate <prefix>=<location> becomes the
# {{ <prefix>tfstate "<addr>" }} function name (ecspresso's func_prefix).
clrnd deploy my-svc manifest.yaml --project p --region r \
  --tfstate gs://my-bucket/app/terraform.tfstate \
  --tfstate network_=gs://my-bucket/network/terraform.tfstate
# -> {{ tfstate "..." }} for the app state, {{ network_tfstate "..." }} for the network state
```

Template functions:

| Function | Description |
| -------- | ----------- |
| `{{ tfstate "<addr>" }}` | Look up `<addr>` in the default state (the `--tfstate` given without a name). |
| `{{ tfstatef "<format>" args... }}` | `printf`-format the address first, then look it up (e.g. `{{ tfstatef "aws_subnet.%s.id" .az }}`). |
| `{{ <prefix>tfstate "<addr>" }}` | Look up `<addr>` in the state `--tfstate <prefix>=<location>` (the prefix becomes the function name). |
| `{{ <prefix>tfstatef "<format>" args... }}` | `printf` variant for a prefixed state. |
| `{{ env "<VAR>" "<default>" }}` | Value of environment variable `<VAR>`, or `<default>` if it is unset or empty (the default is optional). |
| `{{ must_env "<VAR>" }}` | Value of environment variable `<VAR>`; errors if it is not defined. |

The address may be quoted with `"..."` or backticks `` `...` `` — both are Go template string
literals. Use backticks (or `'`, which is rewritten to `"`) when the address itself contains double
quotes, e.g. `` {{ tfstate `aws_s3_bucket.main["id"]` }} ``. A `<prefix>` must be a valid Go
identifier (letters, digits, `_`; not starting with a digit).

### Example: remote state on GCS

For a Terraform GCS backend, the state object lives at `gs://<bucket>/<prefix>/<workspace>.tfstate`:

```hcl
terraform {
  backend "gcs" {
    bucket = "my-tf-state"
    prefix = "cloudrun/prod"
  }
}
```

The default workspace stores it at `gs://my-tf-state/cloudrun/prod/default.tfstate` — that path is
the `--tfstate` URL:

```sh
gcloud auth application-default login   # GCS is read via ADC, same as the API access

clrnd deploy my-svc manifest.yaml \
  --project my-project --region asia-northeast1 \
  --tfstate gs://my-tf-state/cloudrun/prod/default.tfstate
```

Reading the state needs `storage.objects.get` (e.g. `roles/storage.objectViewer`) on the bucket.
If you use a non-default workspace, the object is `<prefix>/<workspace>.tfstate`; confirm the exact
path with `gcloud storage ls gs://my-tf-state/cloudrun/prod/`.

## Usage

```
clrnd [command]
```

### Commands

| Command  | Description                                               |
| -------- | --------------------------------------------------------- |
| `verify` | Verify a manifest.                                        |
| `render` | Render a manifest with templates expanded.                |
| `diff`   | Show the diff between an existing service and a manifest. |
| `deploy` | Deploy a manifest to Cloud Run.                           |
| `init`   | Initialize a project from an existing service.            |
| `status` | Show the current status of a service.                     |
| `wait`   | Wait until a service is ready.                            |
| `revisions` | List the revisions of a service.                       |
| `rollback` | Send traffic back to an earlier revision.                |
| `delete` | Delete a service.                                         |
| `refresh` | Roll out a new revision without changing the definition. |

Run `clrnd [command] --help` for details on a specific command, and `clrnd --version` for the
installed version.

All commands that take a `<service>` and `<manifest>` expect the service name to match the
manifest's `metadata.name`. A typical workflow is `init` → edit → `render` → `verify` → `diff` →
`deploy`.

### Revision names

clrnd does not manage revision names. `init` omits `spec.template.metadata.name` from the manifest
it writes, and `diff`/`deploy` ignore the name Cloud Run reports for the live revision, so Cloud Run
generates a fresh revision name on every deploy.

`refresh` is the one exception: it sets a name deliberately, because that is the only way to force
a new revision. See [refresh](#refresh).

You may still pin a name yourself. If you do, it shows up in `diff` like any other field, and
`verify` warns you: Cloud Run cannot recreate an existing revision, so the next deploy that changes
the template will be rejected. Pinning is only safe for a one-shot deploy.

`--project` and `--region` may be omitted when the corresponding environment variable is set
(gcloud-compatible): project falls back to `$CLOUDSDK_CORE_PROJECT` then `$GOOGLE_CLOUD_PROJECT`,
region to `$CLOUDSDK_RUN_REGION` then `$GOOGLE_CLOUD_REGION`. An explicit flag always wins.

### verify

Validate that a manifest is a well-formed Cloud Run service definition and contains the fields
required to deploy. The schema check is local: it does not access the API and needs no
credentials, so it is safe to run in CI. A valid manifest produces no output on stdout; problems
are reported to stderr with a non-zero exit code. Advisory `warning:` lines (see
[Revision names](#revision-names)) also go to stderr and do not fail the command.

When `--project` / `--region` are resolvable (flag, env, or config) and `--local-only` is not set,
`verify` additionally checks via the API that the resources the manifest references actually exist:
the service account (`spec.template.spec.serviceAccountName`) and the Secret Manager secrets used by
`secretKeyRef` and secret volumes. Unlike ecspresso's `verify` this remote check is opt-in by
availability — when no project/region is set it is skipped and `verify` stays a fully offline lint.

The remote check only **fails** verify when a resource is confirmed missing (the API returns
not-found). When it cannot reach the API to decide — no credentials, the API is disabled, or the
caller lacks read permission — it prints a `warning:` to stderr and does **not** fail, so an
ambient project/region in CI never turns a passing offline lint red.

```sh
clrnd verify <service> <manifest> [--project <PROJECT>] [--region <REGION>] [--local-only] [--tfstate <location>]
```

| Flag           | Description                                                              |
| -------------- | ----------------------------------------------------------------------- |
| `--project`    | GCP project ID. Enables the API existence checks (env/config fallback). |
| `--region`     | Cloud Run region. Enables the API existence checks (env/config fallback). |
| `--local-only` | Skip the API existence checks; validate the manifest locally only.      |
| `--tfstate`    | Terraform state for `{{ tfstate }}` placeholders (see [Templating](#templating-with-terraform-state)). |

```sh
# Local schema check only (no credentials needed)
clrnd verify my-service service.yaml --local-only

# Also confirm the referenced service account and secrets exist
clrnd verify my-service service.yaml --project my-project --region asia-northeast1
```

### render

Render the manifest as a Go template (`{{ tfstate }}`, `{{ env }}`, …) and print the expanded
result without parsing or validating it. This is handy for debugging template output. It does not
access the Cloud Run API and needs no `--project` / `--region`. It checks no service name, so it
takes only the manifest.

```sh
clrnd render <manifest> [--tfstate <location>] [--output <FILE>]
```

| Flag             | Description                                          |
| ---------------- | ---------------------------------------------------- |
| `--tfstate`      | Terraform state for `{{ tfstate }}` placeholders (see [Templating](#templating-with-terraform-state)). |
| `-o`, `--output` | Output file. Writes to stdout if not set.            |

```sh
clrnd render service.yaml --tfstate gs://my-tf-state/prod/default.tfstate
```

### diff

Fetch the live definition of the service from Cloud Run and show a unified diff against the given
manifest file. Both sides are normalized (read-only fields removed) before comparison, so a
manifest produced by `init` compares cleanly. Nothing is printed when there is no difference.

`diff` also works before the service exists: everything shows up as an addition, the same way
`deploy` would create it.

A hand-written minimal manifest would otherwise be a different story: Cloud Run defaults a lot of
fields, and those would show up as a difference forever. `diff` therefore asks Cloud Run to resolve
them first (via a dry run) so the comparison converges.

> **This means `diff` needs permission to update the service**, because a dry run is a write-shaped
> call. If you run `diff` with read-only credentials, pass `--no-server-defaults` to compare against
> the manifest as written.

```sh
clrnd diff <service> <manifest> --project <PROJECT> --region <REGION>
```

| Flag        | Description                                          |
| ----------- | ---------------------------------------------------- |
| `--project` | GCP project ID. Required unless `$CLOUDSDK_CORE_PROJECT` / `$GOOGLE_CLOUD_PROJECT` is set. |
| `--region`  | Cloud Run region, e.g. `asia-northeast1`. Required unless `$CLOUDSDK_RUN_REGION` / `$GOOGLE_CLOUD_REGION` is set. |
| `--tfstate` | Terraform state for `{{ tfstate }}` placeholders: `<location>` or `<name>=<location>` (repeatable). See [Templating](#templating-with-terraform-state). |
| `--no-server-defaults` | Compare against the manifest as written, without resolving Cloud Run's defaults (read-only credentials are enough). |

```sh
clrnd diff my-service service.yaml --project my-project --region asia-northeast1
```

### deploy

Show the diff against the live service, ask for confirmation, then apply the manifest to Cloud Run
— creating the service if it does not exist or replacing it otherwise. The manifest is validated
locally before the request is sent. When there is no difference, nothing is applied.

**After applying, `deploy` waits until the new revision is serving** and exits non-zero if the
rollout fails. When there is nothing to apply it still checks that the service is currently healthy,
so re-running after a failed rollout does not report success. Cloud Run accepts the request before the revision starts, so without this a broken
revision would still exit 0 and CI would treat the deploy as successful. Pass `--no-wait` to return
as soon as the request is accepted.

```sh
clrnd deploy <service> <manifest> --project <PROJECT> --region <REGION> [--auto-approve] [--dry-run]
```

| Flag             | Description                                                    |
| ---------------- | ------------------------------------------------------------- |
| `--project`      | GCP project ID. Required unless `$CLOUDSDK_CORE_PROJECT` / `$GOOGLE_CLOUD_PROJECT` is set. |
| `--region`       | Cloud Run region, e.g. `asia-northeast1`. Required unless `$CLOUDSDK_RUN_REGION` / `$GOOGLE_CLOUD_REGION` is set. |
| `--tfstate`      | Terraform state for `{{ tfstate }}` placeholders: `<location>` or `<name>=<location>` (repeatable). See [Templating](#templating-with-terraform-state). |
| `--auto-approve` | Apply without the interactive confirmation prompt. Use this in CI/CD. |
| `--dry-run`      | Validate the request server-side without applying any changes (no prompt). |
| `--no-server-defaults` | Show the diff against the manifest as written, without resolving Cloud Run's defaults. |
| `--exit-code` | Exit with 2 when there is a difference. Use this for drift checks in CI. |
| `--no-wait`      | Return as soon as the request is accepted, without waiting for the rollout. |
| `--timeout`      | How long to wait for the rollout to finish (default `10m`).    |

Cloud Run fills in many fields on its own, so a hand-written minimal manifest would otherwise keep
showing them as a difference. The diff therefore asks Cloud Run to resolve those first (via a dry
run) and compares against the result. What gets applied is always your manifest — the resolved
values are used only for the comparison.

That dry run is a write-shaped call, so it needs permission to update the service. Pass
`--no-server-defaults` to compare against the manifest as written instead.

The diff is printed to stdout; the confirmation prompt is on stderr. Without `--auto-approve`, a
non-interactive run (no TTY, e.g. a pipeline) refuses to apply and exits with an error — pass
`--auto-approve` there.

```sh
# Interactive: shows the diff, asks "Apply these changes? [y/N]"
clrnd deploy my-service service.yaml --project my-project --region asia-northeast1

# CI/CD: skip the prompt
clrnd deploy my-service service.yaml --project my-project --region asia-northeast1 --auto-approve

# Validate against the server without changing anything
clrnd deploy my-service service.yaml --project my-project --region asia-northeast1 --dry-run
```

### delete

Delete a Cloud Run service. **This cannot be undone**: the service, all of its revisions, and its
URL go away.

What is about to be deleted is printed to stderr and confirmed first. The project and region are
always shown, because the realistic accident is deleting the right service name in the wrong
project.

```
About to delete:
  service: my-service
  project: my-project
  region:  asia-northeast1
  url:     https://my-service-xxxx.a.run.app
Delete service "my-service"? This cannot be undone. [y/N]:
```

```sh
clrnd delete <service> --project <PROJECT> --region <REGION>
clrnd delete my-service --auto-approve   # CI/CD: skip the prompt
clrnd delete my-service --dry-run        # validate the request, delete nothing
```

Without `--auto-approve`, a non-interactive run (no TTY, e.g. a pipeline) refuses to delete and
exits with an error. A service that does not exist fails before any prompt.

Cloud Run deletes asynchronously — the request is accepted while the service is still readable for
a little longer — so `delete` waits until it is actually gone. Pass `--no-wait` to return as soon
as the request is accepted, or `--timeout` to change how long it waits (default `10m`).

### init

Initialize a project from an existing Cloud Run service. `init` fetches the service and scaffolds
two files: the manifest (Knative-style YAML, with server-managed read-only fields such as `status`,
`metadata.uid`, and `resourceVersion` stripped so it is deployable) and a `clrnd.yml` holding the
`project`, `region`, `service`, and `manifest` path. After `init` the other commands run with no
positional arguments. Existing files are not overwritten unless `--force` is given.

For backward compatibility `load` is kept as an alias for `init` (it now scaffolds files rather than
printing to stdout).

```sh
clrnd init <service> --project <PROJECT> --region <REGION> [--output <FILE>] [--force]
```

Flags:

| Flag             | Description                                          |
| ---------------- | ---------------------------------------------------- |
| `--project`      | GCP project ID. Required unless `$CLOUDSDK_CORE_PROJECT` / `$GOOGLE_CLOUD_PROJECT` is set. |
| `--region`       | Cloud Run region, e.g. `asia-northeast1`. Required unless `$CLOUDSDK_RUN_REGION` / `$GOOGLE_CLOUD_REGION` is set. |
| `-o`, `--output` | Manifest file to write (default `manifest.yaml`).    |
| `--force`        | Overwrite existing files.                            |

Examples:

```sh
# Scaffold clrnd.yml + manifest.yaml from a live service
clrnd init my-service --project my-project --region asia-northeast1

# Then everything runs from the config alone
clrnd diff
clrnd deploy
```

### status

Fetch a service from Cloud Run and print its current state: the `Ready` condition, the latest ready
and created revisions, the observed generation, the traffic split, the URL, and every status
condition. Read-only — nothing is modified.

```sh
clrnd status <service> --project <PROJECT> --region <REGION>
```

```
Service:         my-svc
URL:             https://my-svc-xxxx.a.run.app
Ready:           True
Latest ready:    my-svc-00007-abc
Latest created:  my-svc-00007-abc
Generation:      7 (observed 7)
Traffic:
  100%  my-svc-00007-abc
Conditions:
  Ready                True
  ConfigurationsReady  True
  RoutesReady          True
```

When the service is not ready, the reason is shown next to `Ready` and the message on its own line:

```
Ready:           False (RevisionFailed)
Message:         Revision my-svc-00008-def is not ready and cannot serve traffic.
```

`--format json` prints the same information as JSON, for scripting:

```sh
clrnd status --format json | jq -r '.conditions[] | select(.type == "Ready") | .status'
```

`service` may be omitted when set in the config file.

### revisions

List the revisions of a service, newest first, with the share of traffic each one currently
receives. Read-only.

```sh
clrnd revisions <service> --project <PROJECT> --region <REGION>
```

```
REVISION          READY                   TRAFFIC  TAGS    CREATED               IMAGE
my-svc-00008-ghi  Unknown (Deploying)     10%      canary  2026-08-23T11:00:00Z  gcr.io/p/i:v3
my-svc-00007-abc  True                    90%      -       2026-08-22T10:00:00Z  gcr.io/p/i:v2
my-svc-00006-def  False (RevisionFailed)  0%       -       2026-08-21T09:00:00Z  gcr.io/p/i:v1
```

`--format json` prints the same list as JSON:

```sh
clrnd revisions --format json | jq -r '.[] | select(.percent > 0) | .name'
```

### refresh

Re-apply the live definition of a service so that a new revision is created, without changing
anything about it. Useful when the image tag still points somewhere new, or to restart the
containers.

`refresh` never reads a local manifest — it redeploys what is currently running.

```sh
clrnd refresh <service> --project <PROJECT> --region <REGION>
clrnd refresh --revision-suffix rebuild-42 --auto-approve
```

Cloud Run only creates a revision when `spec.template` changes, so `refresh` gives the new revision
an explicit name: `<service>-r<UTC timestamp>`, or `<service>-<--revision-suffix>`. This is the one
place clrnd sets a revision name (see [Revision names](#revision-names)); the next `deploy` from a
manifest drops it again, and `diff` ignores it in the meantime.

The diff is shown and confirmed the same way `deploy` does, and the rollout is waited for unless
`--no-wait` is given.

`refresh` refuses two cases rather than succeeding without effect: when the generated name matches
the revision the service already points at (run it again a second later, or pass a different
`--revision-suffix`), and when traffic is pinned to specific revisions — the state a `rollback`
leaves behind, where a new revision would be created but would serve nothing.

### rollback

Send all traffic back to an earlier revision. Without `--revision`, the revision just before the
newest one currently serving traffic is chosen — so during a canary (new revision at 10%, stable at
90%) a rollback lands on the stable revision, not two generations back.

Only the traffic split changes: `spec.template` is untouched, so **no new revision is created**.
Traffic tags are kept (pinned at 0%) so a rollback does not remove tag URLs.

```sh
clrnd rollback <service> --project <PROJECT> --region <REGION>
clrnd rollback --revision my-service-00006-def --auto-approve
```

The diff is shown and confirmed the same way `deploy` does, and the rollout is waited for unless
`--no-wait` is given. `--dry-run`, `--auto-approve`, and `--timeout` behave as they do for `deploy`.

### wait

Poll a service until its `Ready` condition becomes `True`. If it becomes `False`, `wait` fails
immediately instead of burning the timeout. Progress goes to stderr; nothing is written to stdout.
Ctrl-C stops the wait.

```sh
clrnd wait <service> --project <PROJECT> --region <REGION>
clrnd wait --timeout 5m --interval 5s
```

## Exit codes

| Code | Meaning |
| ---- | ------- |
| `0`  | Success. Also what `diff` returns when it found differences, unless `--exit-code` is given, and what any command returns when you answer `n` at a confirmation prompt. |
| `1`  | Something went wrong. The message is on stderr. |
| `2`  | `diff --exit-code` only: the command succeeded and there **is** a difference. |

The split follows `terraform plan -detailed-exitcode`, so a drift check reads naturally:

```sh
clrnd diff --exit-code
case $? in
  0) echo "in sync" ;;
  2) echo "drift detected"; exit 1 ;;
  *) echo "diff failed"; exit 1 ;;
esac
```

Without `--exit-code`, `diff` exits 0 whether or not it printed anything — so a CI step that
just runs `clrnd diff` will always pass.

## License

Released under the [MIT License](LICENSE).

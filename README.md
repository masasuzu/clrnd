# clrnd

`clrnd` is a command-line tool for deploying services to [Google Cloud Run](https://cloud.google.com/run).
It takes a service name and a manifest file as arguments and provides eleven subcommands:
`verify`, `render`, `diff`, `deploy`, `init`, `status`, `wait`, `revisions`, `rollback`, `delete`,
and `refresh`.

## Installation

Download a binary for your platform from the
[releases page](https://github.com/masasuzu/clrnd/releases) — linux, macOS and Windows, on amd64
and arm64. Each release ships a `checksums.txt`, a keyless cosign signature over it
(`checksums.txt.bundle`), an SBOM per archive, and a build provenance attestation.

```sh
# Verify the checksums were signed by this repository's release workflow
cosign verify-blob checksums.txt \
  --bundle checksums.txt.bundle \
  --certificate-identity-regexp '^https://github.com/masasuzu/clrnd/\.github/workflows/release\.yml@refs/tags/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# Then check the archive against them
shasum -a 256 -c checksums.txt --ignore-missing
```

```sh
# example: macOS arm64
tar xzf clrnd_<version>_darwin_arm64.tar.gz
install clrnd /usr/local/bin/
clrnd --version
```

With a Go toolchain (**Go 1.26.7 or newer**, matching `go.mod`):

```sh
go install github.com/masasuzu/clrnd@latest
```

Or build from source:

```sh
git clone https://github.com/masasuzu/clrnd.git
cd clrnd
go build -o clrnd .
```

`clrnd completion bash|zsh|fish|powershell` prints a shell completion script (cobra's built-in);
`clrnd completion <shell> --help` explains where your shell wants it installed.

## Authentication

`clrnd` uses [Application Default Credentials (ADC)](https://cloud.google.com/docs/authentication/application-default-credentials)
to access the Cloud Run Admin API. Authenticate once with:

```sh
gcloud auth application-default login
```

### Required permissions

| Command | Permissions |
| ------- | ----------- |
| `render` | none — it never contacts the API |
| `verify` | none for the local checks. The API existence checks additionally use `iam.serviceAccounts.get`, `secretmanager.secrets.get`, and `artifactregistry.tags.get` / `artifactregistry.dockerimages.get` |
| `status`, `wait`, `init` | `run.services.get` |
| `revisions` | `run.services.get`, `run.revisions.list`, plus `run.revisions.delete` for `--prune` |
| `diff` | `run.services.get` **and `run.services.update`** — plus `run.services.create` for a service that does not exist yet. See below |
| `deploy` | `run.services.get`, `run.services.update`, and `run.services.create` for a service that does not exist yet |
| `rollback` | `run.services.get`, `run.revisions.list`, `run.services.update` |
| `refresh` | `run.services.get`, `run.services.update` |
| `delete` | `run.services.get`, `run.services.delete` |

`roles/run.viewer` covers the read-only commands and `roles/run.developer` the rest — including
`revisions --prune`, which is the one part of `revisions` that writes. Deploying a
service that runs as a service account also needs `iam.serviceAccounts.actAs` on that service
account (`roles/iam.serviceAccountUser`) — a Cloud Run requirement, not a clrnd one.

**`diff` needs write permission by default.** It asks Cloud Run to fill in the fields it defaults,
and the only way to get those is a `dryRun=all` write, which is checked against
`run.services.update` even though it changes nothing. For a service that does not exist yet the
dry run has to be a *create* (a replace would 404), so that case needs `run.services.create`
instead — the same permission `deploy` would need to create it for real. Pass
`--no-server-defaults` to make no write-shaped call at all: the comparison then stays within
`roles/run.viewer`, at the cost of a diff that shows Cloud Run's defaults as differences on a
hand-written manifest.

`--format json` prints one object instead of the human-readable stream, so a CI job can decide for
itself what to treat as fatal:

```sh
clrnd verify --format json | jq -e '.unchecked | length == 0'
```

```json
{
  "service": "my-service",
  "manifest": "service.yaml",
  "ok": true,
  "unchecked": ["secret \"api-token\": googleapi: Error 403: permission denied"]
}
```

`ok` is `false` only for things clrnd is sure about — a failed local check (`errors`) or a
confirmed 404 (`missing`) — and the command's exit code matches it. Checks that could not be
decided land in `unchecked` and leave `ok` true, which is the same split the text output makes
between a `warning:` and a failure.

None of the permissions `verify` uses for its existence checks are required — a missing one does
**not** fail the command. It
cannot tell "the secret is absent" from "I am not allowed to look", so it prints a `warning:` on
stderr and succeeds. Only a confirmed 404 fails the command.

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

The config is parsed strictly: **an unknown key is an error**, not a warning. A misspelled
`regoin:` fails loudly instead of silently falling back to the environment. A `--config` you pass
explicitly must exist (`init` is the exception — it is the command that creates it); an
auto-detected `clrnd.yml` that is absent is simply an empty config.

## Templating with Terraform state

Manifests are rendered as [Go templates](https://pkg.go.dev/text/template) before they are parsed,
so you can fill placeholders from Terraform state outputs (or any resource attribute) and from
environment variables, using the same notation as [ecspresso](https://github.com/kayac/ecspresso).
This applies to `verify`, `render`, `diff`, and `deploy`.

> **The manifest is always rendered as a template**, even with no `--tfstate` configured. A manifest
> that needs a literal `{{` — a container argument for another templating system, say — will fail to
> parse. Write it as `{{ "{{" }}` to get one through.

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

## Trust boundary

**Treat the manifest, the config file, and the Terraform state you point at as executable input —
the same trust level you give a `Makefile`.** Rendering one is not a read-only operation. Do not
run `clrnd` against a manifest, config, or state that someone outside your trust boundary can
write, and be careful with a CI job that renders a manifest from a fork's pull request.

Three specific things to know:

- **A manifest can read any environment variable.** It is a Go template, so
  `{{ env "GITHUB_TOKEN" }}` works anywhere in the file. In CI that means the job's secrets can end
  up in the rendered output (the job log) or in the deployed container's environment.
- **A Terraform state can redirect where clrnd connects.** `tfstate-lookup`, the library behind
  `{{ tfstate }}`, follows the `backend` recorded **inside the state document**. Whoever can write
  that file can therefore make clrnd send your `$TFE_TOKEN` to a host of their choosing, or make an
  authenticated request to a bucket they control. This is how the library works (ecspresso has the
  same property), not a bug in clrnd — but it means an untrusted state file is an untrusted input.
  Note that the *manifest* cannot choose a state location: only `--tfstate` and the config file
  do that, and a manifest can only reference a prefix that is already registered.
- **Files clrnd writes can hold those same values.** `render -o` and `init` write mode `0600`, and
  replace an existing file through a temporary file in the same directory, so a failed or
  interrupted write leaves the previous content in place instead of a truncated file. Both of those
  are Unix properties: on Windows `os.Rename` is `MoveFileEx`, which the OS does not guarantee to
  be an atomic replacement, and file modes are reduced to the read-only bit, so `0600` does not
  keep the file from other users there. On Windows, put those outputs where the filesystem's own
  permissions protect them.
- **Diffs contain plaintext values.** `sanitizeMap` strips server-managed fields, not secrets, so a
  `env[].value` shows up in `diff` and `deploy` output as written — and `clrnd deploy --auto-approve`
  in CI leaves it in the job log. Reference secrets with `secretKeyRef` or a secret volume rather
  than putting them in `value:`; `verify` understands both and checks that they exist.

## What clrnd does not manage

`clrnd` deploys the Cloud Run **service definition** — what a Knative-style Service YAML can
express. These live next to it and are deliberately out of scope:

- **IAM policy, including public access.** clrnd never calls `GetIamPolicy`/`SetIamPolicy`. A
  service made public with `gcloud run deploy --allow-unauthenticated` carries an `allUsers`
  binding that is **not** part of the manifest, so `diff` will never show it and `init` will not
  capture it. A service `clrnd deploy` creates is private; and `clrnd delete` followed by a redeploy
  silently loses the public setting. Manage access with `gcloud run services add-iam-policy-binding`
  or Terraform.
- **Cloud Run jobs.** Only services (`kind: Service`) are supported. `verify` rejects a job
  manifest — `kind must be "Service", got "Job"` — but it says nothing about jobs being
  unsupported, so this is the place that says it.
- **Domain mappings.** `delete` removes the service without mentioning any mapping pointed at it.
- **Traffic tags as a first-class concept.** Tags in the manifest are applied like any other field,
  and `rollback` / `traffic` preserve existing tags at 0%, but there is no command to add or move
  one. Traffic *percentages* are managed — see [`traffic`](#traffic).

Two smaller edges worth knowing: a mistyped `--region` becomes a DNS failure rather than "unknown
region", because the region goes straight into the API endpoint; and the diff is a plain unified
diff with three lines of context and no pager, so a large service produces a large diff.

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
| `traffic` | Change how traffic is split between revisions.            |
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

| Resource | Checked with | Notes |
| -------- | ------------ | ----- |
| `spec.template.spec.serviceAccountName` | IAM `serviceAccounts.get` | looked up across projects, since Cloud Run allows a service account from another project |
| `secretKeyRef` and secret volumes | Secret Manager `secrets.get` | cross-project aliases in `run.googleapis.com/secrets` are resolved |
| the secret **version** each reference points at | Secret Manager `secrets.versions.get` | the version comes from `key`, or from a `/versions/<v>` suffix on the name, or `latest` as Cloud Run assumes; a `DISABLED` or `DESTROYED` version fails the same way a missing one does, and the lookup is skipped when the secret itself is missing |
| `run.googleapis.com/vpc-access-connector` | Serverless VPC Access `connectors.get` | a bare connector name is resolved against the target project and region; a fully qualified name is used as written |
| `run.googleapis.com/cloudsql-instances` | Cloud SQL Admin `instances.get` | each `<project>:<region>:<instance>`; the project comes from the connection name, so a cross-project (or domain-scoped `example.com:project`) instance works |
| `containers[].image` | Artifact Registry | **only `*-docker.pkg.dev` images**; the location and project come from the image reference, so a cross-project image works |

Both a tag and a digest are handled (`repo/app:v1` and `repo/app@sha256:…`); with neither, `latest`
is assumed, matching what Cloud Run would pull.

**Images on other registries are not checked, and nothing is printed about them.** `gcr.io` has no
equivalent API, and Docker Hub and the rest are out of reach entirely. Warning on every one of them
would mean a `warning:` line on every run for anyone using Docker Hub, which is how warnings stop
being read.

Unlike ecspresso's `verify` this remote check is opt-in by availability — when no project/region is
set it is skipped and `verify` stays a fully offline lint.

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
| `--image`      | Override a container image, the same way `deploy` does (the existence check then looks at the overridden image). |
| `--format`     | Output format: `text` (default) or `json`.                              |
| `--tfstate`    | Terraform state for `{{ tfstate }}` placeholders (see [Templating](#templating-with-terraform-state)). |

```sh
# Local schema check only (no credentials needed)
clrnd verify my-service service.yaml --local-only

# Also confirm the referenced service account, secrets, and image exist
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
| `-o`, `--output` | Output file. Writes to stdout if not set. Written with mode `0600` through a temporary file, so a failed write does not destroy what was there and an existing file's mode is tightened (see [Trust boundary](#trust-boundary) for the Windows caveat). It may not be the manifest being rendered. |

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

> **This means `diff` needs permission to update the service** (or to *create* it, when the service
> does not exist yet), because a dry run is a write-shaped call. If you run `diff` with read-only
> credentials, pass `--no-server-defaults` to compare against the manifest as written — with that
> flag no dry run is performed at all.

```sh
clrnd diff <service> <manifest> --project <PROJECT> --region <REGION>
```

| Flag        | Description                                          |
| ----------- | ---------------------------------------------------- |
| `--project` | GCP project ID. Required unless `$CLOUDSDK_CORE_PROJECT` / `$GOOGLE_CLOUD_PROJECT` or `project:` in the config file is set. |
| `--region`  | Cloud Run region, e.g. `asia-northeast1`. Required unless `$CLOUDSDK_RUN_REGION` / `$GOOGLE_CLOUD_REGION` or `region:` in the config file is set. |
| `--tfstate` | Terraform state for `{{ tfstate }}` placeholders: `<location>` or `<name>=<location>` (repeatable). See [Templating](#templating-with-terraform-state). |
| `--image`   | Override a container image, the same way `deploy` does, so the diff matches what would be applied. |
| `--no-server-defaults` | Compare against the manifest as written, without resolving Cloud Run's defaults (read-only credentials are enough). |
| `--exit-code` | Exit with 2 when there is a difference. Use this for drift checks in CI — see [Exit codes](#exit-codes). |

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

Like `verify`, `deploy` warns on stderr when the manifest pins `spec.template.metadata.name` — the
next deploy that changes the template will be rejected by Cloud Run. `deploy` repeats the warning
because a CI job that only runs `deploy` would otherwise see nothing but the API error when that
happens. It is a warning, not a failure: pinning is legitimate for a one-shot deploy.

**Every change is a compare-and-swap.** `clrnd` sends the `metadata.resourceVersion` it computed the
diff against, so if the service changed in between — a colleague's `gcloud run deploy`, a Terraform
apply, another CI job — the write is rejected instead of silently overwriting them:

```
Error: service "my-service" changed after the diff was computed; re-run to compare against the
current state: googleapi: Error 409: Conflict for resource 'my-service': version '...' was
specified but current version is '...'., aborted
```

Re-run the command: the second attempt diffs against the new state, so you see what the other change
did before deciding to apply on top of it. This applies to `deploy`, `rollback`, and `refresh` alike.

```sh
clrnd deploy <service> <manifest> --project <PROJECT> --region <REGION> [--auto-approve] [--dry-run]
```

| Flag             | Description                                                    |
| ---------------- | ------------------------------------------------------------- |
| `--project`      | GCP project ID. Required unless `$CLOUDSDK_CORE_PROJECT` / `$GOOGLE_CLOUD_PROJECT` or `project:` in the config file is set. |
| `--region`       | Cloud Run region, e.g. `asia-northeast1`. Required unless `$CLOUDSDK_RUN_REGION` / `$GOOGLE_CLOUD_REGION` or `region:` in the config file is set. |
| `--tfstate`      | Terraform state for `{{ tfstate }}` placeholders: `<location>` or `<name>=<location>` (repeatable). See [Templating](#templating-with-terraform-state). |
| `--image`        | Override a container image: `<image>`, or `<container>=<image>` when the manifest has more than one container (repeatable). |
| `--auto-approve` | Apply without the interactive confirmation prompt. Use this in CI/CD. |
| `--dry-run`      | Validate the request server-side without applying any changes (no prompt). |
| `--no-server-defaults` | Show the diff against the manifest as written, without resolving Cloud Run's defaults. |
| `--no-traffic`   | Create the revision without sending traffic to it; the current split is kept. |
| `--no-wait`      | Return as soon as the request is accepted, without waiting for the rollout. |
| `--interval`     | How long to wait between rollout polls (default `2s`; it backs off up to `15s`). |
| `--timeout`      | How long to wait for the rollout to finish (default `10m`).    |

**`--image` covers the usual CI case**: a manifest committed to the repository, deployed with the
tag that was just built.

```sh
clrnd deploy --image "$REGISTRY/app:$GITHUB_SHA"
clrnd deploy --image app=$REGISTRY/app:$SHA --image proxy=$REGISTRY/proxy:$SHA   # with a sidecar
```

The container name may be omitted only when the manifest defines exactly one container; with more,
clrnd refuses rather than guessing which one you meant. `verify` and `diff` take the same flag, so
the whole `verify` → `diff` → `deploy` sequence looks at the same image. The manifest stays the
source of truth for everything else — `--image` exists because the image tag is the one field that
legitimately changes on every deploy. `{{ must_env "IMAGE" }}` in the manifest remains an
alternative, and `render` deliberately has no `--image`: it prints the template expansion as-is,
without parsing it.

**`--no-traffic` is how you deploy before deciding to serve it.** Without it, a service whose
manifest says nothing about `spec.traffic` sends everything to the new revision the moment it is
ready. With it, the split you are serving now is pinned to those revisions by name, so the new
revision starts at 0% and you move traffic over afterwards:

```sh
clrnd deploy --no-traffic
clrnd traffic --to-latest --percent 10    # canary
clrnd traffic --to-latest                 # all of it
```

It needs an existing service (there is nothing to keep traffic on otherwise), and it replaces any
`spec.traffic` written in the manifest — clrnd says so on stderr when the manifest has one.

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

| Flag             | Description                                                    |
| ---------------- | ------------------------------------------------------------- |
| `--project`      | GCP project ID. Required unless `$CLOUDSDK_CORE_PROJECT` / `$GOOGLE_CLOUD_PROJECT` or `project:` in the config file is set. |
| `--region`       | Cloud Run region. Required unless `$CLOUDSDK_RUN_REGION` / `$GOOGLE_CLOUD_REGION` or `region:` in the config file is set. |
| `--auto-approve` | Delete without the interactive confirmation prompt. Use this in CI/CD. |
| `--dry-run`      | Validate the request server-side without deleting anything (no prompt). |
| `--no-wait`      | Return as soon as the request is accepted, without waiting for the service to disappear. |
| `--interval`     | How long to wait between polls while the deletion completes (default `2s`). |
| `--timeout`      | How long to wait for the service to disappear (default `10m`). |

### init

Initialize a project from an existing Cloud Run service. `init` fetches the service and scaffolds
two files: the manifest (Knative-style YAML, with server-managed read-only fields such as `status`,
`metadata.uid`, `resourceVersion`, the `cloud.googleapis.com/location` label, and the
`run.googleapis.com/client-name` / `client-version` annotations stripped so it is deployable) and a `clrnd.yml` holding the
`project`, `region`, `service`, and `manifest` path. After `init` the other commands run with no
positional arguments. Existing files are not overwritten unless `--force` is given.

The config is written to `--config` when you pass it (it does not have to exist yet — `init` is what
creates it), otherwise to `clrnd.yml` in the current directory. The `manifest:` it records is
relative to the config file, so `clrnd init my-service -c infra/clrnd.yml` keeps working from any
directory (the directory has to exist — `clrnd` writes files, it never creates directories). Both files are written with mode `0600`, since a live service definition can contain
plaintext environment variables. `--force` replaces them through a temporary file, so an
interrupted or failing write leaves the previous content in place rather than a truncated file, and
an existing file's mode is tightened to `0600` rather than kept as it was. See
[Trust boundary](#trust-boundary) for what of that holds on Windows.

For backward compatibility `load` is kept as an alias for `init` (it now scaffolds files rather than
printing to stdout).

```sh
clrnd init <service> --project <PROJECT> --region <REGION> [--output <FILE>] [--force] [-c <FILE>]
```

Flags:

| Flag             | Description                                          |
| ---------------- | ---------------------------------------------------- |
| `--project`      | GCP project ID. Required unless `$CLOUDSDK_CORE_PROJECT` / `$GOOGLE_CLOUD_PROJECT` or `project:` in the config file is set. |
| `--region`       | Cloud Run region, e.g. `asia-northeast1`. Required unless `$CLOUDSDK_RUN_REGION` / `$GOOGLE_CLOUD_REGION` or `region:` in the config file is set. |
| `-o`, `--output` | Manifest file to write (default `manifest.yaml`).    |
| `--force`        | Overwrite existing files.                            |
| `-c`, `--config` | Config file to write (default `clrnd.yml`). Need not exist yet. |

Examples:

```sh
# Scaffold clrnd.yml + manifest.yaml from a live service
clrnd init my-service --project my-project --region asia-northeast1

# Then everything runs from the config alone
clrnd diff
clrnd deploy

# Scaffold into a subdirectory; the recorded manifest path stays correct.
# clrnd writes files but never creates directories, so make it first.
mkdir -p infra
clrnd init my-service -c infra/clrnd.yml -o infra/manifest.yaml
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

| Flag        | Description                                                    |
| ----------- | ------------------------------------------------------------- |
| `--project`      | GCP project ID. Required unless `$CLOUDSDK_CORE_PROJECT` / `$GOOGLE_CLOUD_PROJECT` or `project:` in the config file is set. |
| `--region`       | Cloud Run region. Required unless `$CLOUDSDK_RUN_REGION` / `$GOOGLE_CLOUD_REGION` or `region:` in the config file is set. |
| `--format`  | Output format: `text` (default) or `json`.                     |

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

`IMAGE` lists **every** container of the revision, comma-separated, in the order the manifest
declares them — a service with a sidecar shows both images. In JSON the same values are the
`images` array; `image` is still there and holds the first container, so an existing
`jq '.[].image'` keeps working.

`--format json` prints the same list as JSON:

```sh
clrnd revisions --format json | jq -r '.[] | select(.percent > 0) | .name'
```

| Flag        | Description                                                    |
| ----------- | ------------------------------------------------------------- |
| `--project`      | GCP project ID. Required unless `$CLOUDSDK_CORE_PROJECT` / `$GOOGLE_CLOUD_PROJECT` or `project:` in the config file is set. |
| `--region`       | Cloud Run region. Required unless `$CLOUDSDK_RUN_REGION` / `$GOOGLE_CLOUD_REGION` or `region:` in the config file is set. |
| `--format`  | Output format: `text` (default) or `json`.                     |
| `--prune`   | Delete the revisions older than the newest `--keep`.           |
| `--keep`    | How many of the newest revisions to keep when pruning (default `10`). |
| `--auto-approve` | Prune without the interactive confirmation prompt.         |
| `--dry-run` | With `--prune`, only show what would be deleted.                |

Cloud Run never deletes a revision on its own, and a service can only hold so many, so
`--prune` is the way to clear out old ones:

```sh
clrnd revisions --prune --keep 20 --dry-run   # see what would go
clrnd revisions --prune --keep 20
```

**A revision is never deleted while it is serving traffic, named in `spec.traffic`, or carrying a
tag**, however old it is — deleting the first would take the service down, and the last would
remove a tag URL. `spec.traffic` is consulted as well as the live split because the status side
does not show a share until a rollout settles; when it says `latestRevision: true` — which names no
revision at all — the newest created and newest ready revisions are protected too, so a prune that
runs mid-rollout cannot delete the revision that is about to serve.

`--keep` counts from the newest revision down, protected or not, so `--keep 20` means "everything
older than the 20 newest is a candidate". Anything protected in that older range stays, which is
why the number left behind can be larger than `--keep`.

What is about to go is printed first (as data on stdout, so `--format json` works here too — an
empty array when there is nothing to do), and the confirmation follows the same rule as `delete`:
without a terminal, clrnd refuses unless `--auto-approve` is given. `--keep`, `--dry-run` and
`--auto-approve` are rejected without `--prune` rather than quietly ignored.

Revisions are ordered newest-first by `creationTimestamp`, falling back to the revision name when a
timestamp cannot be parsed. That fallback is a plain descending string comparison: Cloud Run's own
numbering (`-00007-abc`) sorts correctly under it, but a hand-chosen `--revision-suffix` may not —
`v2` comes out ahead of `v10`, however much later `v10` was created.

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
place clrnd sets a revision name (see [Revision names](#revision-names)); the next `deploy` that
**actually changes something** drops it again, and `diff` ignores it in the meantime. A `deploy`
with no difference applies nothing, so the name stays until there is a real change to write.

The diff is shown and confirmed the same way `deploy` does, and the rollout is waited for unless
`--no-wait` is given.

`refresh` refuses two cases rather than succeeding without effect: when the generated name matches
the revision the service already points at (run it again a second later, or pass a different
`--revision-suffix`), and when traffic is pinned to specific revisions — the state a `rollback`
leaves behind, where a new revision would be created but would serve nothing.

| Flag                | Description                                                    |
| ------------------- | ------------------------------------------------------------- |
| `--project`      | GCP project ID. Required unless `$CLOUDSDK_CORE_PROJECT` / `$GOOGLE_CLOUD_PROJECT` or `project:` in the config file is set. |
| `--region`       | Cloud Run region. Required unless `$CLOUDSDK_RUN_REGION` / `$GOOGLE_CLOUD_REGION` or `region:` in the config file is set. |
| `--revision-suffix` | Name the new revision `<service>-<suffix>` instead of `<service>-r<UTC timestamp>`. |
| `--auto-approve`    | Apply without the interactive confirmation prompt. Use this in CI/CD. |
| `--dry-run`         | Validate the request server-side without applying any changes (no prompt). |
| `--no-wait`         | Return as soon as the request is accepted, without waiting for the rollout. |
| `--interval`     | How long to wait between rollout polls (default `2s`; it backs off up to `15s`). |
| `--timeout`         | How long to wait for the rollout to finish (default `10m`).    |

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
`--no-wait` is given.

| Flag             | Description                                                    |
| ---------------- | ------------------------------------------------------------- |
| `--project`      | GCP project ID. Required unless `$CLOUDSDK_CORE_PROJECT` / `$GOOGLE_CLOUD_PROJECT` or `project:` in the config file is set. |
| `--region`       | Cloud Run region. Required unless `$CLOUDSDK_RUN_REGION` / `$GOOGLE_CLOUD_REGION` or `region:` in the config file is set. |
| `--revision`     | Revision to send traffic to (default: the one before the revision currently serving). |
| `--auto-approve` | Apply without the interactive confirmation prompt. Use this in CI/CD. |
| `--dry-run`      | Validate the request server-side without applying any changes (no prompt). |
| `--no-wait`      | Return as soon as the request is accepted, without waiting for the rollout. |
| `--interval`     | How long to wait between rollout polls (default `2s`; it backs off up to `15s`). |
| `--timeout`      | How long to wait for the rollout to finish (default `10m`).    |

A `--revision` that is not `Ready` is a warning on stderr, not an error: the rollback goes ahead.
Sending traffic to a revision that failed is sometimes what you want (to reproduce a failure), and
Cloud Run is the authority on whether it can serve.

### traffic

Change how traffic is split between revisions. Only `spec.traffic` changes: no new revision is
created, and traffic tags are kept (pinned at 0%), the same way `rollback` treats them.

```sh
# canary: 10% to a revision, the rest stays where it is
clrnd traffic <service> --to <revision> --percent 10

# all of it, then back to following the newest revision
clrnd traffic <service> --to <revision>
clrnd traffic <service> --to-latest
```

`--percent` below 100 leaves the remainder on the revision **currently serving the most** traffic,
which is the canary shape (stable 90% / new 10%). If that revision *is* the one you are sending
traffic to — it already carries production — there is nothing to split it against, and clrnd says
so rather than promoting the runner-up: doing that mid-canary would drop the stable revision to
your `--percent` and hand the rest to an old one.

`--to-latest` stops pinning the split to a revision name: traffic follows whatever revision is
newest, now and after the next deploy. This is how you undo a `rollback` — while traffic is pinned,
`refresh` refuses to run (a new revision would serve nothing) and only a `deploy` that changes
something would move it.

| Flag             | Description                                          |
| ---------------- | ---------------------------------------------------- |
| `--project`      | GCP project ID. Required unless `$CLOUDSDK_CORE_PROJECT` / `$GOOGLE_CLOUD_PROJECT` or `project:` in the config file is set. |
| `--region`       | Cloud Run region. Required unless `$CLOUDSDK_RUN_REGION` / `$GOOGLE_CLOUD_REGION` or `region:` in the config file is set. |
| `--to`           | Revision to send traffic to.                         |
| `--to-latest`    | Send traffic to the latest revision, and keep following it. |
| `--percent`      | Share of the traffic to send (1-100, default `100`). |
| `--auto-approve` | Apply without the interactive confirmation prompt. Use this in CI/CD. |
| `--dry-run`      | Validate the request server-side without applying any changes (no prompt). |
| `--no-wait`      | Return as soon as the request is accepted, without waiting for the rollout. |
| `--interval`     | How long to wait between rollout polls (default `2s`; it backs off up to `15s`). |
| `--timeout`      | How long to wait for the rollout to finish (default `10m`). |

### wait

Poll a service until its `Ready` condition becomes `True`. If it becomes `False`, `wait` fails
immediately instead of burning the timeout. Progress goes to stderr; nothing is written to stdout.
Ctrl-C stops the wait.

```sh
clrnd wait <service> --project <PROJECT> --region <REGION>
clrnd wait --timeout 5m --interval 5s
```

| Flag         | Description                                                    |
| ------------ | ------------------------------------------------------------- |
| `--project`      | GCP project ID. Required unless `$CLOUDSDK_CORE_PROJECT` / `$GOOGLE_CLOUD_PROJECT` or `project:` in the config file is set. |
| `--region`       | Cloud Run region. Required unless `$CLOUDSDK_RUN_REGION` / `$GOOGLE_CLOUD_REGION` or `region:` in the config file is set. |
| `--timeout`  | How long to wait before giving up (default `10m`).             |
| `--interval` | How long to wait between polls (default `2s`). The interval backs off up to `15s`; a value you set is never shrunk below that cap. |

A failed poll is not a failed rollout: a transient error is reported and retried until the timeout,
because a single 503 would otherwise turn an already-applied deploy into a red CI run. Retried means
`408`, `429`, `5xx`, and errors with no HTTP status (a dropped connection, a DNS failure). Errors
that will not heal are returned at once instead of holding the job for the whole timeout: `400`,
`401`, `403`, and `404` (a service that does not exist will not appear). A `403` that reports a rate
limit is the exception — quota clears on its own, so it is retried.

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

## Contributing

Bug reports and feature requests go to [issues](https://github.com/masasuzu/clrnd/issues); what
changed in each version is on the [releases page](https://github.com/masasuzu/clrnd/releases).

Before opening a pull request:

```sh
go build ./... && go vet ./... && gofmt -l . && go test -race ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.1 run ./...
```

CI pins the same golangci-lint version, so this is the check it runs — not a different one that
happens to be the latest release that day.

CI also checks the things that are not Go code. To reproduce those locally:

```sh
go mod tidy -diff                                                  # go.mod / go.sum are tidy

# the shell scripts (bash -n only checks its first file argument, hence the loop)
for f in test/e2e/run.sh .github/scripts/*.sh; do bash -n "$f"; done
shellcheck test/e2e/run.sh .github/scripts/*.sh
./.github/scripts/check-tool-pins.sh                               # the pins below agree

go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12          # .github/workflows/*.yml
go run github.com/goreleaser/goreleaser/v2@v2.17.1 check           # .goreleaser.yaml
go run github.com/goreleaser/goreleaser/v2@v2.17.1 build --snapshot --clean   # every release target
```

CI uses ShellCheck v0.11.0 (installed from a pinned, checksummed release); any recent version is
close enough locally.

Those tool versions are pinned by hand: Dependabot updates the action SHAs and `go.mod`, but not a
version passed to an action, a `go run tool@version`, or the ShellCheck release CI downloads. When
you bump one, bump it everywhere it is written — `.github/scripts/check-tool-pins.sh` (run in CI)
fails when the copies disagree, which is what kept CI on `latest` while this file said `v2.6.2`.

Releases are cut from `main` only: the release workflow refuses a tag whose commit is not an
ancestor of `main`, and runs the same checks as CI before it builds anything.

Anything that touches the Cloud Run API should also be run through the end-to-end test in
[test/e2e](test/e2e/), which creates and deletes a real service. It is opt-in, cannot run in CI,
and needs a project you are happy to create Cloud Run services in — see
[test/e2e/README.md](test/e2e/README.md).

## License

Released under the [MIT License](LICENSE).

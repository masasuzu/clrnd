# clrnd

`clrnd` is a command-line tool for deploying services to [Google Cloud Run](https://cloud.google.com/run).
It takes a service name and a manifest file as arguments and provides the subcommands
`verify`, `render`, `diff`, `deploy`, `init`, `status`, `wait`, `revisions`, `rollback`, `traffic`,
`delete`, and `refresh`. Cloud Run jobs are deployed the same way, and run, through `clrnd jobs`
(see [jobs](#jobs)).

## Installation

Prebuilt binaries are published for linux, macOS and Windows, on amd64 and arm64. Every release
also ships a `checksums.txt`, a keyless cosign signature over it (`checksums.txt.bundle`), an SBOM
per archive, and a build provenance attestation.

### Version managers

[mise](https://mise.jdx.dev) installs `clrnd` straight from this repository's GitHub releases —
there is no plugin to add and no registry entry to wait for:

```sh
mise use -g github:masasuzu/clrnd
```

mise checks the release's GitHub build provenance attestation when one is available, and this
repository publishes one for every archive, so this path carries the same guarantee as the `cosign`
verification below without running it by hand.

Run it inside a project, without `-g`, to record `clrnd` in that project's `mise.toml` so everyone
working on it installs the same tool. Name a version to pin one — without `@<version>` mise writes
`latest` and re-resolves it on every install:

```sh
mise use github:masasuzu/clrnd@<version>
```

```toml
[tools]
"github:masasuzu/clrnd" = "<version>"
```

With a Go toolchain, `mise use -g go:github.com/masasuzu/clrnd@latest` builds from source instead of
downloading an archive.

### Prebuilt binaries

Download the archive for your platform from the
[releases page](https://github.com/masasuzu/clrnd/releases), then verify it:

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

### With a Go toolchain

Needs the Go version named in [`go.mod`](go.mod) or newer:

```sh
go install github.com/masasuzu/clrnd@latest
```

### From source

```sh
git clone https://github.com/masasuzu/clrnd.git
cd clrnd
go build -o clrnd .
```

### Shell completion

`clrnd completion bash|zsh|fish|powershell` prints a shell completion script (cobra's built-in);
`clrnd completion <shell> --help` explains where your shell wants it installed. `--format`
completes its values (`text`, `json`).

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
| `verify` | none for the local checks. The API existence checks additionally use `iam.serviceAccounts.get`, `secretmanager.secrets.get` and `secretmanager.versions.get`, `artifactregistry.tags.get` / `artifactregistry.dockerimages.get`, `vpcaccess.connectors.get`, and `cloudsql.instances.get` — each only when the manifest references that kind of resource. A check that is denied is a `warning:`, not a failure |
| `status`, `wait`, `init` | `run.services.get` |
| `revisions` | `run.services.get`, `run.revisions.list`, plus `run.revisions.delete` for `--prune` |
| `diff` | `run.services.get` **and `run.services.update`** — plus `run.services.create` for a service that does not exist yet. See below |
| `deploy` | `run.services.get`, `run.services.update`, and `run.services.create` for a service that does not exist yet |
| `rollback` | `run.services.get`, `run.revisions.list`, `run.services.update` |
| `refresh` | `run.services.get`, `run.services.update` |
| `traffic` | `run.services.get`, `run.services.update`, plus `run.revisions.list` for `--to` |
| `delete` | `run.services.get`, `run.services.delete` |
| `jobs verify` | the same as `verify` |
| `jobs diff` | `run.jobs.get` **and `run.jobs.update`** — plus `run.jobs.create` for a job that does not exist yet, for the same reason as `diff` |
| `jobs deploy` | `run.jobs.get`, `run.jobs.update`, and `run.jobs.create` for a job that does not exist yet |
| `jobs run` | `run.jobs.run` — `run.jobs.runWithOverrides` instead when `--args` / `--env` / `--task-count` / `--task-timeout` is given — plus `run.executions.get` for the wait |
| `jobs status`, `jobs wait`, `jobs init` | `run.jobs.get` |
| `jobs delete` | `run.jobs.get`, `run.jobs.delete` |
| `jobs executions list` | `run.jobs.get`, `run.executions.list` |
| `jobs executions wait` | `run.executions.get` |
| `jobs executions cancel` | `run.executions.get`, `run.executions.cancel` |

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
# job: my-job             # instead of service, for the `clrnd jobs` commands
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

Resolution order (highest first):

| Setting  | Order |
| -------- | ----- |
| project  | `--project` → config `project` |
| region   | `--region` → config `region` |
| service  | positional `[service]` → config `service` |
| job      | positional `[job]` → config `job` (the `clrnd jobs` commands) |
| manifest | positional `[manifest]` → config `manifest` |
| tfstate  | `--tfstate` (if any given, replaces config) → config `tfstate` |

**clrnd does not take the project or region from the environment.** gcloud reads
`$CLOUDSDK_CORE_PROJECT` / `$GOOGLE_CLOUD_PROJECT` and `$CLOUDSDK_RUN_REGION` /
`$GOOGLE_CLOUD_REGION`, and clrnd used to as well, ahead of the config file. Those variables are
set by things other than the person deploying — `google-github-actions/auth` exports the
credentials' project, and Cloud Shell sets its own — so they could send a deploy to a project the
repository's `clrnd.yml` did not name. Name the target with `--project` / `--region` or in the
config file. When neither is set and one of those variables is, the error says it was ignored.

The project and region are checked locally before anything is sent: the region must look like a
Cloud Run region (`asia-northeast1`) and the project like a project ID (or number). The region
becomes part of the API's host name, so this also keeps a config file from sending your access
token anywhere else (see [Trust boundary](#trust-boundary)).

Every command that changes something prints its target — the service or job, the project and the
region — on stderr before the diff or the prompt, `--auto-approve` or not, so a CI log records
where each change went.

The config is parsed strictly: **an unknown key is an error**, not a warning. A misspelled
`regoin:` fails loudly instead of silently falling back to the environment. A `--config` you pass
explicitly must exist (`init` is the exception — it is the command that creates it); an
auto-detected `clrnd.yml` that is absent is simply an empty config.

A config file describes one thing to deploy, so it sets **only one** of `service` and `job`. A
command for the other kind does not fall back to that name: `clrnd delete` run next to a job's
config refuses (`the config file names a job`) instead of deleting a service that happens to share
the job's name.

## Templating with Terraform state

Manifests are rendered as [Go templates](https://pkg.go.dev/text/template) before they are parsed,
so you can fill placeholders from Terraform state outputs (or any resource attribute) and from
environment variables, using the same notation as [ecspresso](https://github.com/kayac/ecspresso).
This applies to `verify`, `render`, `diff`, and `deploy`, and to the `clrnd jobs` forms of them.

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
| `{{ <value> \| json_escape }}` | Escape the value for use **inside** a JSON string (quotes, newlines, backslashes); the surrounding `"` are yours to write. |

`json_escape` matters wherever a manifest carries JSON as a string — an annotation, or an
`env[].value` holding a config blob:

```yaml
    metadata:
      annotations:
        example.com/config: >-
          {"token": "{{ must_env "TOKEN" | json_escape }}"}
```

Without it, a value containing `"` or a newline produces broken JSON that Cloud Run accepts as an
opaque string and the application then fails to parse.

The block scalar (`>-`) is deliberate. `json_escape` escapes the value for **JSON**, which is not
the same set of characters YAML needs: inside `'...'` a value containing `'` would still break the
document, and inside `"..."` YAML would consume the very backslashes `json_escape` just added. A
block scalar takes the line as written, so both quoting styles stay out of the way.

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

Specific things to know:

- **A manifest can read any environment variable.** It is a Go template, so
  `{{ env "GITHUB_TOKEN" }}` works anywhere in the file. In CI that means the job's secrets can end
  up in the rendered output (the job log) or in the deployed container's environment.
- **A config file chooses which project and region clrnd acts on.** That is its job, and it is why
  every command that changes something prints the target first. What it cannot do is choose the
  host: the region is checked against the shape of a region before it becomes part of the API's
  host name, so a `region:` written to redirect your access token is refused.
- **A state URL's password is masked in errors.** A location such as
  `https://user:token@host/state` is shown as `https://user:xxxxx@host/state` in every error about
  it, including the ones the tfstate library returns, since errors go to stderr and so to a CI log.
  Anything else in the URL, such as a token in the query string, is shown as written.
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
- **Cloud Run worker pools.** A `kind: WorkerPool` manifest is refused with a message saying so.
- **Domain mappings.** `delete` removes the service without mentioning any mapping pointed at it.
- **Traffic tags as a first-class concept.** Tags in the manifest are applied like any other field,
  and `rollback` / `traffic` preserve existing tags at 0%, but there is no command to add or move
  one. Traffic *percentages* are managed — see [`traffic`](#traffic).

Smaller edges worth knowing: a mistyped `--region` becomes a DNS failure rather than "unknown
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
| `jobs`   | Commands for Cloud Run jobs: `jobs verify`, `jobs diff`, `jobs deploy`, `jobs run`, `jobs status`, `jobs wait`, `jobs delete`, `jobs init`, `jobs executions list`, `jobs executions wait`, `jobs executions cancel`. |

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
caller lacks read permission — it prints a `warning:` to stderr and does **not** fail, so a
project/region in the config file never turns a passing offline lint red in a CI job without
credentials.

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
| `--project` | GCP project ID. Required unless `project:` is set in the config file. |
| `--region`  | Cloud Run region, e.g. `asia-northeast1`. Required unless `region:` is set in the config file. |
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
did before deciding to apply on top of it. This applies to every command that shows a diff before
applying it, for services and jobs alike.

```sh
clrnd deploy <service> <manifest> --project <PROJECT> --region <REGION> [--auto-approve] [--dry-run]
```

| Flag             | Description                                                    |
| ---------------- | ------------------------------------------------------------- |
| `--project`      | GCP project ID. Required unless `project:` is set in the config file. |
| `--region`       | Cloud Run region, e.g. `asia-northeast1`. Required unless `region:` is set in the config file. |
| `--tfstate`      | Terraform state for `{{ tfstate }}` placeholders: `<location>` or `<name>=<location>` (repeatable). See [Templating](#templating-with-terraform-state). |
| `--image`        | Override a container image: `<image>`, or `<container>=<image>` when the manifest has more than one container (repeatable). |
| `--auto-approve` | Apply without the interactive confirmation prompt. Use this in CI/CD. |
| `--dry-run`      | Validate the request server-side without applying any changes (no prompt). |
| `--no-server-defaults` | Show the diff against the manifest as written, without resolving Cloud Run's defaults. |
| `--no-traffic`   | Create the revision without sending traffic to it; the current split is kept. |
| `--no-wait`      | Return as soon as the request is accepted, without waiting for the rollout. |
| `--interval`     | Initial polling interval (default `2s`; it backs off up to `15s`). |
| `--timeout`      | How long to wait for the rollout to finish (default `10m`; `0` waits with no limit). |

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
non-interactive run (stdin is not a terminal: a pipe, a file or `/dev/null`, as in CI) refuses to
apply and exits with an error — pass `--auto-approve` there.

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

Every command that waits treats `--timeout` and `--interval` the same way: a timeout of `0` means no
limit, a negative timeout and an interval that is not positive are refused before anything is sent.

| Flag             | Description                                                    |
| ---------------- | ------------------------------------------------------------- |
| `--project`      | GCP project ID. Required unless `project:` is set in the config file. |
| `--region`       | Cloud Run region. Required unless `region:` is set in the config file. |
| `--auto-approve` | Delete without the interactive confirmation prompt. Use this in CI/CD. |
| `--dry-run`      | Validate the request server-side without deleting anything (no prompt). |
| `--no-wait`      | Return as soon as the request is accepted, without waiting for the service to disappear. |
| `--interval`     | Initial polling interval (default `2s`; it backs off up to `15s`). |
| `--timeout`      | How long to wait for the service to disappear (default `10m`; `0` waits with no limit). |

### init

Initialize a project from an existing Cloud Run service. `init` fetches the service and scaffolds
the manifest (Knative-style YAML, with server-managed read-only fields such as `status`,
`metadata.uid`, `resourceVersion`, the `cloud.googleapis.com/location` label, and the
`run.googleapis.com/client-name` / `client-version` annotations stripped so it is deployable) and a `clrnd.yml` holding the
`project`, `region`, `service`, and `manifest` path. After `init` the other commands run with no
positional arguments. Existing files are not overwritten unless `--force` is given.

The config is written to `--config` when you pass it (it does not have to exist yet — `init` is what
creates it), otherwise to the config file found in the current directory (`clrnd.yml` or
`clrnd.yaml`), or to a new `clrnd.yml` when there is none. `init --force` over an existing config
keeps its `tfstate:` list, which `init` has no way to scaffold. The `manifest:` it records is
relative to the config file, so `clrnd init my-service -c infra/clrnd.yml` keeps working from any
directory (the directory has to exist — `clrnd` writes files, it never creates directories). The files are written with mode `0600`, since a live service definition can contain
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
| `--project`      | GCP project ID. Required unless `project:` is set in the config file. |
| `--region`       | Cloud Run region, e.g. `asia-northeast1`. Required unless `region:` is set in the config file. |
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
| `--project`      | GCP project ID. Required unless `project:` is set in the config file. |
| `--region`       | Cloud Run region. Required unless `region:` is set in the config file. |
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
| `--project`      | GCP project ID. Required unless `project:` is set in the config file. |
| `--region`       | Cloud Run region. Required unless `region:` is set in the config file. |
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

`refresh` refuses to run rather than succeed without effect when the generated name matches
the revision the service already points at (run it again a second later, or pass a different
`--revision-suffix`), and when traffic is pinned to specific revisions — the state a `rollback`
leaves behind, where a new revision would be created but would serve nothing.

| Flag                | Description                                                    |
| ------------------- | ------------------------------------------------------------- |
| `--project`      | GCP project ID. Required unless `project:` is set in the config file. |
| `--region`       | Cloud Run region. Required unless `region:` is set in the config file. |
| `--revision-suffix` | Name the new revision `<service>-<suffix>` instead of `<service>-r<UTC timestamp>`. |
| `--auto-approve`    | Apply without the interactive confirmation prompt. Use this in CI/CD. |
| `--dry-run`         | Validate the request server-side without applying any changes (no prompt). |
| `--no-wait`         | Return as soon as the request is accepted, without waiting for the rollout. |
| `--interval`     | Initial polling interval (default `2s`; it backs off up to `15s`). |
| `--timeout`         | How long to wait for the rollout to finish (default `10m`; `0` waits with no limit). |

### rollback

Send all traffic back to an earlier revision. Without `--revision`, the revision just before the
newest one currently serving traffic is chosen — so during a canary (new revision at 10%, stable at
90%) a rollback lands on the stable revision, not two generations back.

Without `--revision`, it refuses while the newest revision serves nothing and is not Ready — what
a failed deploy leaves behind. Traffic that follows the latest revision then stays on the last one
that became ready, so the revision serving now *is* the good one, and stepping back from it would
take production further back than the deploy that failed. The error names both revisions: fix the
manifest and deploy again, or pass `--revision` to choose one yourself. This is why a CI step like
"roll back when the deploy fails" does not downgrade a service the failed deploy never touched.

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
| `--project`      | GCP project ID. Required unless `project:` is set in the config file. |
| `--region`       | Cloud Run region. Required unless `region:` is set in the config file. |
| `--revision`, `--to` | Revision to send traffic to (default: the one before the revision currently serving). `--to` is the same flag, spelled as in `traffic`. |
| `--auto-approve` | Apply without the interactive confirmation prompt. Use this in CI/CD. |
| `--dry-run`      | Validate the request server-side without applying any changes (no prompt). |
| `--no-wait`      | Return as soon as the request is accepted, without waiting for the rollout. |
| `--interval`     | Initial polling interval (default `2s`; it backs off up to `15s`). |
| `--timeout`      | How long to wait for the rollout to finish (default `10m`; `0` waits with no limit). |

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
| `--project`      | GCP project ID. Required unless `project:` is set in the config file. |
| `--region`       | Cloud Run region. Required unless `region:` is set in the config file. |
| `--to`           | Revision to send traffic to.                         |
| `--to-latest`    | Send traffic to the latest revision, and keep following it. |
| `--percent`      | Share of the traffic to send (1-100, default `100`). |
| `--auto-approve` | Apply without the interactive confirmation prompt. Use this in CI/CD. |
| `--dry-run`      | Validate the request server-side without applying any changes (no prompt). |
| `--no-wait`      | Return as soon as the request is accepted, without waiting for the rollout. |
| `--interval`     | Initial polling interval (default `2s`; it backs off up to `15s`). |
| `--timeout`      | How long to wait for the rollout to finish (default `10m`; `0` waits with no limit). |

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
| `--project`      | GCP project ID. Required unless `project:` is set in the config file. |
| `--region`       | Cloud Run region. Required unless `region:` is set in the config file. |
| `--timeout`  | How long to wait before giving up (default `10m`; `0` waits with no limit). |
| `--interval` | Initial polling interval (default `2s`). It backs off up to `15s`; a value you set above that is kept. |

A failed poll is not a failed rollout: a transient error is reported and retried until the timeout,
because a single 503 would otherwise turn an already-applied deploy into a red CI run. Retried means
`408`, `429`, `5xx`, and errors with no HTTP status (a dropped connection, a DNS failure). Errors
that will not heal are returned at once instead of holding the job for the whole timeout: `400`,
`401`, `403`, and `404` (a service that does not exist will not appear). A `403` that reports a rate
limit is the exception — quota clears on its own, so it is retried.

### jobs

Cloud Run jobs (`kind: Job`) are deployed with the commands under `clrnd jobs`. They are the
service commands of the same name, built for jobs: the same arguments (a job name instead of a
service name), the same flags, the same diff, confirmation and `--dry-run`, and the same
compare-and-swap on `resourceVersion`.

```sh
clrnd jobs verify my-job job.yaml
clrnd jobs diff my-job job.yaml
clrnd jobs deploy my-job job.yaml --auto-approve
```

```yaml
# job.yaml
apiVersion: run.googleapis.com/v1
kind: Job
metadata:
  name: my-job
spec:
  template:
    spec:
      template:
        spec:
          containers:
          - image: us-docker.pkg.dev/my-project/app/migrate:v1
```

What differs from a service:

- The manifest's `apiVersion` is `run.googleapis.com/v1`, and the containers sit one template
  deeper (`spec.template.spec.template.spec`). `verify` checks the same references in them, and
  its `--format json` names the job under `job` instead of `service`.
- `jobs deploy` creates or replaces the job definition and waits until the job is ready. It does
  **not** run the job; there is no revision to roll out and no traffic, so `--no-traffic` does not
  exist here.
- Starting an execution updates the job, so a `jobs deploy` whose diff was computed before someone
  ran the job and applied after it fails with the same "changed after the diff was computed" error
  as a concurrent edit. Re-run it.
- In the config file, name the job with `job:` instead of `service:`.

Each kind's commands refuse the other's manifest before contacting the API and say where it
belongs (`kind is "Job", but this command takes a Service: ... use the 'clrnd jobs' commands`).

#### Status, delete and init

`jobs status`, `jobs delete` and `jobs init` are the service commands of the same name for a job.
`jobs status` shows the Ready condition, the generation, how many executions the job has had and
how the last one went (`--format json` too; the last execution's finish time is `completionTime`,
the key `jobs executions list` uses, and the older `completed` key still carries the same value). `jobs delete` shows the job with its project, region
and execution count, asks before deleting (`--auto-approve`, `--dry-run`), and waits until the job
is gone; its executions are deleted with it. `jobs init` scaffolds `manifest.yaml` and a `clrnd.yml`
that names the job with `job:`, so `clrnd jobs diff` right after it is empty.

#### Running a job

`clrnd jobs run` (or `clrnd jobs execute`, gcloud's name for it) starts an execution and waits until
it finishes. It fails when the execution fails or is cancelled, so a CI step that runs a migration
fails with it. The execution's name goes to stdout as soon as it is created, and progress goes to
stderr:

```sh
clrnd jobs deploy my-job job.yaml --auto-approve
clrnd jobs run my-job                              # waits; exit 1 if the execution fails
EXEC=$(clrnd jobs run my-job --no-wait)            # just start it
clrnd jobs run my-job --args migrate --args --up --env DRY_RUN=1 --task-count 1
```

| Flag | Description |
| ---- | ----------- |
| `--args` | An argument for the container, replacing the job's arguments. Repeatable, one argument each: unlike gcloud's `--args`, a value is not split at commas. |
| `--env` | `NAME=VALUE` set for this execution only. Repeatable. |
| `--container` | The container `--args` / `--env` apply to; needed only when the job has more than one. |
| `--task-count` | Number of tasks for this execution (default: the job's own). |
| `--task-timeout` | How long each task may run, in whole seconds (default: the job's own). |
| `--no-wait` | Return as soon as the execution is created. |
| `--timeout` | Give up waiting after this long. **No limit by default** (`0`). |
| `--interval` | Initial polling interval (default `2s`, backing off up to `15s`). |

The overrides apply to that one execution; the job's definition is unchanged, and running with any
of them needs `run.jobs.runWithOverrides` rather than `run.jobs.run`.

The wait has no time limit by default because the job already bounds itself (its task timeout and
retries), and giving up on the CLI side would not stop anything: it would only turn an execution
that later succeeds into a failed CI step. For the same reason, **stopping the wait — Ctrl-C, or
`--timeout` — never cancels the execution.** The error says it is still running and prints the
`clrnd jobs executions cancel` command that stops it. There is no confirmation prompt, as with
gcloud and ecspresso: running a job changes no definition.

#### Executions

`clrnd jobs executions list [job]` lists a job's executions, newest first, with what each one is
doing or how it ended, its tasks, and when it was created and completed (`--format json` too). A job
that does not exist is an error, not an empty list.

```
EXECUTION     STATUS     TASKS                     CREATED                      COMPLETED
my-job-x7k2p  Running    0/1 succeeded, 1 running  2026-01-02T03:04:05.123456Z  -
my-job-9qd4m  Failed     0/1 succeeded, 1 failed   2026-01-01T03:04:05.123456Z  2026-01-01T03:05:01.654321Z
```

The status is one of `Pending`, `Running`, `Cancelling`, `Succeeded`, `Failed` and `Cancelled`.

`clrnd jobs executions wait <execution>` waits for an execution to finish, as `jobs run` does after
starting one, and fails when it fails or is cancelled — for a CI step that started a job with
`jobs run --no-wait` and does something else first. Like `jobs run`, it has no time limit unless
`--timeout` sets one, and stopping the wait never cancels the execution.

`clrnd jobs wait [job]` is `wait` for a job: it waits until the job is ready after a
`jobs deploy --no-wait`.

`clrnd jobs executions cancel <execution>` stops a running execution; the job itself is unchanged.
It fetches the execution first, so one that does not exist or has already finished fails before
anything is asked. It shows the execution with its job, project, region and status, and asks before
cancelling (`--auto-approve` skips the prompt). Cloud Run has no dry run for a cancel. The cancel
takes effect shortly after it is accepted, so the command waits until the execution has stopped
(`--no-wait`, `--timeout`, `--interval` as for `delete`). It fails if the execution succeeded
before the cancel took effect, since then nothing was cancelled.

## Exit codes

| Code | Meaning |
| ---- | ------- |
| `0`  | Success. Also what `diff` returns when it found differences, unless `--exit-code` is given, and what any command returns when you answer `n` at a confirmation prompt. |
| `1`  | Something went wrong. The message is on stderr. |
| `2`  | `diff --exit-code` only: the command succeeded and there **is** a difference. The diff is on stdout, and nothing is printed to stderr. |

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
[test/e2e](test/e2e/), which creates and deletes a real service and job. The Go suite is gated by
the `e2e` build tag; ordinary `go test ./...` excludes it. The cloud scenarios cannot run in CI
and need a project intended for disposable services and jobs — see
[test/e2e/README.md](test/e2e/README.md).

## License

Released under the [MIT License](LICENSE).

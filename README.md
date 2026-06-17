# clrnd

`clrnd` is a command-line tool for deploying services to [Google Cloud Run](https://cloud.google.com/run).
It takes a service name and a manifest file as arguments and provides subcommands to verify, diff,
deploy, and load Cloud Run services. 

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
  - location: gs://my-tf-state/app/default.tfstate        # default state (name omitted)
  - name: network                                         # named state
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
This applies to `verify`, `diff`, and `deploy`.

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

# Multiple named states: {{ tfstate "<name>" "<addr>" }}
clrnd deploy my-svc manifest.yaml --project p --region r \
  --tfstate gs://my-bucket/app/terraform.tfstate \
  --tfstate network=gs://my-bucket/network/terraform.tfstate
```

Template functions:

| Function | Description |
| -------- | ----------- |
| `{{ tfstate "<addr>" }}` | Look up `<addr>` in the default state (the `--tfstate` given without a name). |
| `{{ tfstate "<name>" "<addr>" }}` | Look up `<addr>` in the named state `--tfstate <name>=<location>`. |
| `{{ env "<VAR>" "<default>" }}` | Value of environment variable `<VAR>`, or `<default>` if it is unset or empty (the default is optional). |
| `{{ must_env "<VAR>" }}` | Value of environment variable `<VAR>`; errors if it is not defined. |

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
| `diff`   | Show the diff between an existing service and a manifest. |
| `deploy` | Deploy a manifest to Cloud Run.                           |
| `load`   | Load the manifest of an existing service.                 |

Run `clrnd [command] --help` for details on a specific command.

All commands that take a `<service>` and `<manifest>` expect the service name to match the
manifest's `metadata.name`. A typical workflow is `load` → edit → `verify` → `diff` → `deploy`.

`--project` and `--region` may be omitted when the corresponding environment variable is set
(gcloud-compatible): project falls back to `$CLOUDSDK_CORE_PROJECT` then `$GOOGLE_CLOUD_PROJECT`,
region to `$CLOUDSDK_RUN_REGION` then `$GOOGLE_CLOUD_REGION`. An explicit flag always wins.

### verify

Validate that a manifest is a well-formed Cloud Run service definition and contains the fields
required to deploy. This is a local check: it does not access the API and needs no credentials,
so it is safe to run in CI. Nothing is printed when the manifest is valid; problems are reported
to stderr with a non-zero exit code.

```sh
clrnd verify <service> <manifest> [--tfstate <location>]
```

`--tfstate` is accepted here too (see [Templating](#templating-with-terraform-state)); resolving a
remote state still requires network access, otherwise `verify` stays fully offline.

```sh
clrnd verify my-service service.yaml
```

### diff

Fetch the live definition of the service from Cloud Run and show a unified diff against the given
manifest file. Both sides are normalized (read-only fields removed) before comparison, so a
manifest produced by `load` compares cleanly. Nothing is printed when there is no difference.

```sh
clrnd diff <service> <manifest> --project <PROJECT> --region <REGION>
```

| Flag        | Description                                          |
| ----------- | ---------------------------------------------------- |
| `--project` | GCP project ID. Required unless `$CLOUDSDK_CORE_PROJECT` / `$GOOGLE_CLOUD_PROJECT` is set. |
| `--region`  | Cloud Run region, e.g. `asia-northeast1`. Required unless `$CLOUDSDK_RUN_REGION` / `$GOOGLE_CLOUD_REGION` is set. |
| `--tfstate` | Terraform state for `{{ tfstate }}` placeholders: `<location>` or `<name>=<location>` (repeatable). See [Templating](#templating-with-terraform-state). |

```sh
clrnd diff my-service service.yaml --project my-project --region asia-northeast1
```

### deploy

Show the diff against the live service, ask for confirmation, then apply the manifest to Cloud Run
— creating the service if it does not exist or replacing it otherwise. The manifest is validated
locally before the request is sent. When there is no difference, nothing is applied.

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

### load

Fetch the manifest (Knative-style YAML) of an existing Cloud Run service. Server-managed,
read-only fields (such as `status`, `metadata.uid`, and `resourceVersion`) are stripped so that
the output can be reused as a deployable manifest.

```sh
clrnd load <service> --project <PROJECT> --region <REGION> [--output <FILE>]
```

Flags:

| Flag             | Description                                          |
| ---------------- | ---------------------------------------------------- |
| `--project`      | GCP project ID. Required unless `$CLOUDSDK_CORE_PROJECT` / `$GOOGLE_CLOUD_PROJECT` is set. |
| `--region`       | Cloud Run region, e.g. `asia-northeast1`. Required unless `$CLOUDSDK_RUN_REGION` / `$GOOGLE_CLOUD_REGION` is set. |
| `-o`, `--output` | Output file. Writes to stdout if not set.            |

Examples:

```sh
# Print the manifest to stdout
clrnd load my-service --project my-project --region asia-northeast1

# Write the manifest to a file
clrnd load my-service --project my-project --region asia-northeast1 --output service.yaml
```

## License

Released under the [MIT License](LICENSE).

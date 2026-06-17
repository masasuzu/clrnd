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
clrnd verify <service> <manifest>
```

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

```sh
clrnd diff my-service service.yaml --project my-project --region asia-northeast1
```

### deploy

Apply the manifest to Cloud Run, creating the service if it does not exist or replacing it
otherwise. The manifest is validated locally before the request is sent.

```sh
clrnd deploy <service> <manifest> --project <PROJECT> --region <REGION> [--dry-run]
```

| Flag        | Description                                                    |
| ----------- | ------------------------------------------------------------- |
| `--project` | GCP project ID. Required unless `$CLOUDSDK_CORE_PROJECT` / `$GOOGLE_CLOUD_PROJECT` is set. |
| `--region`  | Cloud Run region, e.g. `asia-northeast1`. Required unless `$CLOUDSDK_RUN_REGION` / `$GOOGLE_CLOUD_REGION` is set. |
| `--dry-run` | Validate the request server-side without applying any changes. |

```sh
# Validate against the server without changing anything
clrnd deploy my-service service.yaml --project my-project --region asia-northeast1 --dry-run

# Deploy for real
clrnd deploy my-service service.yaml --project my-project --region asia-northeast1
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

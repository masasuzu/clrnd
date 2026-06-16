# crner

`crner` is a command-line tool for deploying services to [Google Cloud Run](https://cloud.google.com/run).
It takes a service name and a manifest file as arguments and provides subcommands to verify, diff,
deploy, and load Cloud Run services.

## Installation

```sh
go install github.com/masasuzu/crner@latest
```

Or build from source:

```sh
git clone https://github.com/masasuzu/crner.git
cd crner
go build -o crner .
```

## Authentication

`crner` uses [Application Default Credentials (ADC)](https://cloud.google.com/docs/authentication/application-default-credentials)
to access the Cloud Run Admin API. Authenticate once with:

```sh
gcloud auth application-default login
```

## Usage

```
crner [command]
```

### Commands

| Command  | Description                                               |
| -------- | --------------------------------------------------------- |
| `verify` | Verify a manifest.                                        |
| `diff`   | Show the diff between an existing service and a manifest. |
| `deploy` | Deploy a manifest to Cloud Run.                           |
| `load`   | Load the manifest of an existing service.                 |

Run `crner [command] --help` for details on a specific command.

### load

Fetch the manifest (Knative-style YAML) of an existing Cloud Run service. Server-managed,
read-only fields (such as `status`, `metadata.uid`, and `resourceVersion`) are stripped so that
the output can be reused as a deployable manifest.

```sh
crner load <service> --project <PROJECT> --region <REGION> [--output <FILE>]
```

Flags:

| Flag             | Description                                          |
| ---------------- | ---------------------------------------------------- |
| `--project`      | GCP project ID. (required)                           |
| `--region`       | Cloud Run region, e.g. `asia-northeast1`. (required) |
| `-o`, `--output` | Output file. Writes to stdout if not set.            |

Examples:

```sh
# Print the manifest to stdout
crner load my-service --project my-project --region asia-northeast1

# Write the manifest to a file
crner load my-service --project my-project --region asia-northeast1 --output service.yaml
```

## License

Released under the [MIT License](LICENSE).

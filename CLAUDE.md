# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

`crner` is a Go CLI for deploying services to Google Cloud Run. It takes a service name and a
manifest file (Knative-style Service YAML) and exposes `verify`, `diff`, `deploy`, and `load`
subcommands. `load` and `diff` are implemented; `verify` and `deploy` are stubs that currently
just print their own name.

## Commands

```sh
go build ./...          # build all packages
go run . load <svc> --project <P> --region <R>   # run without installing
go install .            # install the crner binary to $GOPATH/bin
go test ./...           # run tests (none exist yet)
go test -run TestName ./cmd   # run a single test
go vet ./...            # static checks
gofmt -w .              # format
```

## Architecture

- Entry point [main.go](main.go) calls `cmd.Execute()` and exits non-zero on error.
- [cmd/root.go](cmd/root.go) defines the cobra root command and registers every subcommand in
  its `init()`. Each subcommand lives in its own file (`cmd/<name>.go`) as a package-level
  `*cobra.Command` var, following the standard cobra layout.
- Invocation form is `crner <subcommand> <service> [<manifest>]`. `verify`/`diff`/`deploy` take
  two positional args (`cobra.ExactArgs(2)`); `load` takes one (`cobra.ExactArgs(1)`).
- All Cloud Run access and manifest handling lives in [internal/cloudrun](internal/cloudrun/cloudrun.go).
  Subcommands in `cmd/` only parse flags and do I/O, then call into this package — this is the
  layering to follow for `verify`/`deploy`.

### internal/cloudrun (the core logic)

- `GetService(ctx, project, region, service)` — fetches a `*run.Service` using **Application
  Default Credentials**, picked up automatically by `run.NewService`
  (`google.golang.org/api/run/v1`). The user runs `gcloud auth application-default login` once;
  no credentials are passed explicitly.
- The v1 namespaces API requires a **regional endpoint** (`https://<region>-run.googleapis.com`
  via `option.WithEndpoint`), so `--region` must be a required flag, not optional.
- `sanitizeMap` strips server-managed read-only fields (`status`, `metadata.uid`,
  `resourceVersion`, server-set annotations/labels — see the `serverManaged*` slices). `ToManifest`
  applies it to a fetched service; `Normalize` applies the same to a local manifest file so the two
  sides are comparable. YAML is produced with `sigs.k8s.io/yaml` (JSON tags → YAML), which sorts
  keys alphabetically.
- `Diff` returns a unified diff (via `go-difflib`) of two manifests, empty when identical. `diff`
  normalizes both the live service and the local manifest before comparing.

## Conventions

- All user-facing strings (cobra `Short`/`Long`, flag usage, error messages) are in **English**.
  Code comments are in Japanese — keep that split.
- Subcommands succeed **silently**: on success they emit only data (e.g. the manifest), never a
  confirmation message. Errors are returned from `RunE` so cobra prints them to stderr and sets a
  non-zero exit code.
- When adding a subcommand: create `cmd/<name>.go` with a `*cobra.Command` var, set `RunE`, and
  register it with `rootCmd.AddCommand` in [cmd/root.go](cmd/root.go).

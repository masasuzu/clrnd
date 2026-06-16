# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

`crner` is a Go CLI for deploying services to Google Cloud Run. It takes a service name and a
manifest file (Knative-style Service YAML) and exposes `verify`, `diff`, `deploy`, and `load`
subcommands. Only `load` is fully implemented; `verify`, `diff`, and `deploy` are stubs that
currently just print their own name.

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

### load (the reference implementation)

[cmd/load.go](cmd/load.go) is the model for how real subcommands talk to Cloud Run:

- Auth is **Application Default Credentials**, picked up automatically by
  `run.NewService` (`google.golang.org/api/run/v1`). No credentials are passed explicitly — the
  user runs `gcloud auth application-default login` once.
- The v1 namespaces API requires a **regional endpoint**, constructed as
  `https://<region>-run.googleapis.com` via `option.WithEndpoint`. The region therefore must be a
  required flag, not optional.
- The fetched `*run.Service` is round-tripped through JSON into a `map[string]interface{}`, then
  `sanitizeManifest` strips server-managed read-only fields (`status`, `metadata.uid`,
  `resourceVersion`, server-set annotations/labels — see the `serverManaged*` slices) so the
  output is a re-deployable manifest. Output YAML is produced with `sigs.k8s.io/yaml` (JSON tags →
  YAML), which sorts keys alphabetically.

## Conventions

- All user-facing strings (cobra `Short`/`Long`, flag usage, error messages) are in **English**.
  Code comments are in Japanese — keep that split.
- Subcommands succeed **silently**: on success they emit only data (e.g. the manifest), never a
  confirmation message. Errors are returned from `RunE` so cobra prints them to stderr and sets a
  non-zero exit code.
- When adding a subcommand: create `cmd/<name>.go` with a `*cobra.Command` var, set `RunE`, and
  register it with `rootCmd.AddCommand` in [cmd/root.go](cmd/root.go).

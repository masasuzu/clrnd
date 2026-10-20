---
name: add-resource-kind
description: Put a new Cloud Run resource kind (after services, jobs and worker pools) on clrnd's shared deploy path, from probing the real API to the E2E phase. Use when adding a kind or a kind-level capability such as revisions or a split; not for a new command on an existing kind.
---

# Add a resource kind

Jobs (#132) and worker pools (#142) were added this way. Each step's output feeds the next, so do
them in order.

## 1. Probe the real API first

Nothing about a kind's server-side behaviour is assumed: confirm each item below against a real
project before writing code, then write what you confirmed into the kind's comment ("confirmed
against the real API"), the way `workerPoolKind` does.

- **Compare-and-swap**: a stale `resourceVersion` is a 409, a malformed one a 400, and a write without
  one an unconditional overwrite.
- **Dry run**: it returns the definition with the server's defaults filled in.
- **Settling**: `observedGeneration` and the `Ready` condition settle on the new generation, also for
  a change that creates no revision.
- **Deletion**: whether it is asynchronous.
- **Server-managed metadata**: the annotations and labels the server writes go into the
  `serverManaged*` lists in `internal/cloudrun/cloudrun.go`, so a diff ignores them.
- **Integer fields where 0 differs from leaving the field out**: the generated types drop a 0 unless
  the field is in `ForceSendFields` (see `maxRetries` in `job.go`).
- **Kind-specific rules**: combinations the API rejects or silently reinterprets. For example,
  `scalingMode: manual` without a count stops a worker pool.

How to probe:

- Build clrnd from the working tree and drive it with throwaway manifests. When clrnd has no
  command for the call yet, use the REST API with `gcloud auth print-access-token`. Prefer
  `--dry-run` for validation questions; it changes nothing.
- Give every resource a throwaway name. Delete it from a `trap` so an interrupted probe still
  cleans up, and list the kind afterwards to confirm nothing is left.
- Read the project ID from an environment variable or a file outside the repository; never put it in
  the script. Redact the output before quoting it anywhere: the default service account in a diff
  carries the project number.

## 2. internal/cloudrun

- Write a `kindOf[T]` with:
  - the API calls;
  - `apiVersion`, `meta`, a `workload` extractor and `status`;
  - the hooks the kind needs.
- A kind holding a `RevisionTemplate`:
  - supplies `template`;
  - reuses `template.go` for `align` and `scaffold`;
  - gets a `revisions` hook (`revisionsOf[T]` in `revisionkind.go`) if its revisions can be listed,
    rolled back or split.
- Rules only this kind has go in `check`, like a worker pool's instance count.
- Add the type to `resource` and the kind to `kindHints`.
- Add a fake-API test for every API call.

## 3. cmd

- Add a `target` in `cmd/target.go` and add it to `targets`; `TestTargetsAreComplete` fails on a
  missing field.
- A kind with revisions also gets a `revisionSplit`, which names what it splits and the commands
  and flags built from it.
- Add a command group with `Args: cobra.NoArgs` and a `RunE` that prints the help; otherwise a
  mistyped subcommand exits 0.
- Register the per-target factories (`newDeployCmd(t)` and friends) on the group.
- Add a config key in `internal/config`; the config may name at most one kind.

## 4. Docs and E2E

- **README**: the commands table, the kind's own section, and a permissions-table row for every
  command.
- **test/e2e**: a scenario for the kind.
  - Record each resource with `own()` from the scenario that creates it.
  - The scenario runs in parallel with the others.
  - Run the full suite against the real project and check that the log has no identifiers.
- **AGENTS.md**: update "Where things live" and the Overview.

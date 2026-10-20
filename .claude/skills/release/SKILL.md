---
name: release
description: Cut a clrnd release from main — choose the version, draft the release notes' Highlights, have the maintainer push the tag, wait for the release workflow, and put the Highlights on the GitHub release. Use when asked to release, tag or publish a version.
---

# Cut a release

The release workflow (`.github/workflows/release.yml`) does the building, signing and publishing.
This skill covers what happens around it.

## 1. Check that main is ready

- Every PR meant for the release is merged, and you are on the latest `origin/main`.
- CI on `main` passed for that exact commit (`gh run list --branch main`). A release tag on a commit
  whose CI is still running or failed is the wrong commit.
- List what changed: `git log --oneline <previous tag>..origin/main`.

## 2. Choose the version

clrnd is pre-1.0:

- **Minor** (`v0.N+1.0`): a new command, flag or resource kind, or any breaking change.
- **Patch** (`v0.N.M+1`): fixes only.

A change is breaking when a script or CI job that worked before can now fail or do something
else. Examples: a flag, `--format json` key or exit code is renamed or removed, or a value is now
read from a different place.

## 3. Draft the Highlights

Write the Highlights in English, in Markdown. The GoReleaser changelog follows them, so they explain
what the commit subjects cannot. Use only the sections that apply, in this order:

```markdown
## Highlights

### ⚠️ Breaking changes
- **What changed** (#PR). Why it changed.
  - **Migration:** what a user has to do.

### New
### Safety
### Fixes
### For contributors
```

- Lead each item with a bold summary, then the PR numbers.
- Say what a user sees, not how the code changed.
- "For contributors" covers changes that do not reach the released binary, such as the E2E suite
  or CI, and says so.
- Follow the writing rules in AGENTS.md:
  - no count that summarizes a list;
  - no real Google Cloud identifier.
- Check the draft for the project ID, any number of 6 digits or more, `gserviceaccount.com` and
  `run.app`. Commit SHAs in the changelog are expected hits.

Show the draft and the chosen version to the maintainer, and wait for approval before tagging.

## 4. Tag

The release tag is a lightweight tag `vX.Y.Z` on the CI-green `main` commit:

```sh
git tag vX.Y.Z <commit>
git push origin vX.Y.Z
```

Pushing a release tag publishes a release. Leave it to the maintainer unless they have explicitly
allowed you to push it. Agent sandboxes commonly refuse it as a production deploy anyway.

## 5. Wait for the release workflow

- Find the run: `gh run list --workflow release.yml`.
- Wait for it with `gh run watch <id> --exit-status`. It has the following jobs:
  - `guard`: the commit is on `main`;
  - `verify`: the same checks as CI;
  - `goreleaser`.
- It usually takes longer than 10 minutes, so wait in the background.
- If it fails, report the failing job and log to the maintainer. Do not delete or move the tag
  yourself.

## 6. Publish the Highlights

GoReleaser creates the release with only the changelog. Put the approved Highlights in front of it
and keep the changelog as it is:

```sh
gh release view vX.Y.Z --json body -q .body > changelog.md
cat highlights.md changelog.md > notes.md
gh release edit vX.Y.Z --notes-file notes.md
```

Write those files outside the repository. Then confirm the release:

- it is not a draft or a prerelease;
- its assets are attached;
- the body starts with `## Highlights` and still ends with `## Changelog`.

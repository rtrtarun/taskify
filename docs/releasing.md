# Releasing Taskify

Releases are automatic. Every merge to `main` that passes the tests is versioned, tagged, packaged, and published. Nobody runs a release by hand.

## What happens on a merge to `main`

1. The **Test** job runs: vet, lint, build, and `go test -race` against PostgreSQL.
2. If it passes, the **Release** job works out the next version from the commit messages since the last tag (rules below).
3. If a release is due, it pushes an annotated tag such as `v0.2.0`.
4. GoReleaser builds and publishes:

| Output | Where |
| --- | --- |
| Server binaries for Linux, macOS, and Windows, each on amd64 and arm64, with the SQL migrations and README | Assets on the GitHub Release |
| `checksums.txt` (SHA-256 of every archive) | Assets on the GitHub Release |
| Container image for linux/amd64 and linux/arm64, with migrations at `/migrations` | `ghcr.io/rtrtarun/taskify`, tagged `X.Y.Z`, `X.Y`, and `latest` |
| Changelog grouped into breaking changes, features, fixes, and other | The GitHub Release notes |
| Go module version | The tag itself: `go get github.com/rtrtarun/taskify@vX.Y.Z` |

Versions with a suffix, such as `v0.3.0-rc.1`, are marked as pre-releases on GitHub; the automatic releases never carry one. The running server reports its version in its start-up log line and in `GET /healthz`.

The configuration is in [.goreleaser.yaml](../.goreleaser.yaml), the release job in [.github/workflows/ci.yml](../.github/workflows/ci.yml), and the version rules in [tools/nextversion](../tools/nextversion/main.go).

## How the version is chosen

Versions follow [Semantic Versioning](https://semver.org). The first release is `v0.1.0`. After that, the commits since the last tag are read as [Conventional Commits](https://www.conventionalcommits.org), and the largest change wins:

| Commit message | Version change before 1.0 | From 1.0 on |
| --- | --- | --- |
| `feat!: ...`, `fix(api)!: ...`, or a `BREAKING CHANGE:` line in the body | minor (`0.4.2` to `0.5.0`) | major (`1.4.2` to `2.0.0`) |
| `feat: ...` | minor | minor |
| `fix:`, `perf:`, `refactor:`, or any message that does not follow the convention | patch | patch |
| `docs:`, `chore:`, `ci:`, `test:`, `style:`, `build:` | no release | no release |

- A merge whose commits are all in the last row produces no release.
- A message that does not follow the convention counts as a patch, so a change is never silently left out of a release.
- Merge commits are ignored; the commits inside the merged branch are what count.
- Version 1.0.0 is not reached by a commit message. When the API is ready to be declared stable, push the `v1.0.0` tag by hand, on a commit on `main`; later merges continue from it.

### Writing commit messages

```text
feat(api): add POST /tasks/{id}/restore
fix(relay): stop retrying a row deleted mid-dispatch
docs: explain the outbox lease
feat!: rename the webhook signature header

BREAKING CHANGE: AgentPulse must now send X-AgentPulse-Signature-256.
```

With squash merges, the pull request title becomes the commit message, so write the title in this form.

## When a release fails

- **The tests fail.** Nothing is tagged or published. Fix the problem on a branch and merge again.
- **Publishing fails after the tag was pushed** (for example, a registry outage). Re-run the failed Release job from the Actions page. It finds the tag already on the commit and publishes that version again, replacing any assets uploaded the first time.
- **A bad version went out.** Do not delete or move the tag: Go's module proxy caches versions permanently. Merge a fix, which releases the next version.

## Dry run on your machine

GoReleaser can build everything without publishing. With Docker running:

```bash
docker run --rm -e IMAGE_REPO=local/taskify \
  -v "$PWD:/src" -v /var/run/docker.sock:/var/run/docker.sock -w /src \
  goreleaser/goreleaser:latest release --snapshot --clean
```

Archives land in `dist/`, and the images are loaded locally as `ghcr.io/local/taskify:<version>-amd64` and `-arm64`. Check the configuration alone with `goreleaser check`.

## One-time repository settings

These are done once by a repository admin on GitHub:

1. **Make the image public.** The first publish creates the `taskify` package under the account as private. In the package settings, change its visibility to public and link it to this repository.
2. **Protect `main`.** Require pull requests and the **Test** check before merging, so every commit on `main` is releasable.
3. **Allow the release job to push tags.** If a tag ruleset or protection rule covers `v*`, add GitHub Actions to its bypass list.

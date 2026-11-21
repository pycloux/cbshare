# Contributing to cbshare

The [README](README.md#how-the-application-is-built) explains how the code is organised and lists the common changes (a new setting, endpoint or screen). This page is about how changes get in and released.

## Branches

- `develop` is the default branch. Open pull requests against it. Dependabot does too, every Monday.
- `main` only receives `develop`, through a pull request, when a release is prepared.
- Releases are tags on `main` (`v0.2`, …).

## Before opening a pull request

```sh
make            # go vet, go test -race, build
make fmt        # CI fails on unformatted Go files
make e2e        # the P1 external tests (needs the Python setup in tests/e2e/README.md)
```

CI runs the Go tests on Linux and Windows and the P1 external tests in Chromium on every pull request. A pull request into `main` runs the full external suite in Chromium, Firefox and WebKit.

## Things that are easy to miss

- **User-facing sentences live in three places.** Error messages, console lines and the startup banner are compared word for word by the external tests (`tests/e2e`), and many are quoted in `docs/troubleshooting.md`. When you reword one, search `tests/e2e` and `docs` for the old text.
- **Every change to the chat calls `s.changed()`** in `internal/server`; otherwise other browsers don't see it until their next unrelated refresh.
- **Test cases and the manual plan.** A new test of the plan gets `@pytest.mark.case("ID")` and the case becomes `automated` in `tests/e2e/cases.yaml`. Anything left to a person (`manual`, `suite`, or a `manual_part`) is described in [tests/manual-plan.md](tests/manual-plan.md); `test_consistency.py` fails if the two disagree.
- **Test hooks.** Code in files with `//go:build testhooks` exists only in test builds. Release builds must never need it.
- **Schema changes** need a migration: see the comment on `migrate` in `internal/store/store.go`.
- **Visible interface changes**: run the **Screenshots** workflow (Actions tab) to regenerate `docs/images`.

## Documentation

The user documentation in `docs/` is an MkDocs site (`mkdocs serve` to preview, after `pip install -r docs/requirements.txt`). Pull requests check that it builds; it is **published only from `main`**, and only if GitHub Pages is enabled for the repository (Settings → Pages → Source: GitHub Actions). A docs change merged into `develop` goes online with the next merge into `main`.

## Releasing

1. Open a pull request from `develop` into `main`; it runs the full external suite. Merge it.
2. Tag the merge commit and push the tag: `git tag -a v0.3 -m "cbshare v0.3" && git push origin v0.3`.
3. The release workflow runs the tests again, then publishes the archives and `SHA256SUMS`. If it fails, nothing is published: fix the problem, delete the tag (`git push --delete origin v0.3`, `git tag -d v0.3`) and tag again.
4. Run the [manual test plan](tests/manual-plan.md) on the published archives, fill in its record, and only then announce the release.

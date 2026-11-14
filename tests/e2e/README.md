# External tests

Black-box tests of the built `cbshare` program, following the test plan case by case (see [#19](https://github.com/pycloux/cbshare/issues/19)); the case IDs, such as `LOCK-01`, are the plan's. They start the real executable and check it only from the outside, as a person would: its command line and console output, its files, its HTTPS API, and real browsers driven by [Playwright](https://playwright.dev/python/). They never import cbshare's Go code; the Go tests (`go test ./...`) cover the code from the inside.

## Running the tests

You need Python 3.12 or later, Go (to build cbshare) and Linux.

```sh
cd tests/e2e
python -m venv .venv && . .venv/bin/activate
pip install -r requirements.txt
python -m playwright install --with-deps chromium firefox webkit

pytest -m p1 -n auto                  # the P1 cases, in parallel (as on pull requests)
pytest -n auto -m "not slow"          # every automated case except the slow ones (~6 min)
pytest -n auto                        # everything, SET-07 included (it waits 15 minutes)
pytest -n auto -m browser --browser firefox   # the browser cases in Firefox (or webkit)
pytest --case LOCK-01                 # one case by its plan ID (repeatable)
pytest --case CHAT-02 --headed        # watch it in a visible browser
```

Markers: `p1`, `p2` and `p3` come from `cases.yaml`; `browser` is added to every test that opens a browser (only those are worth repeating in Firefox and WebKit); `slow` marks tests that wait for real timeouts.

From the repository root, `make e2e` runs the P1 cases.

The tests build cbshare from this repository. To test a binary you already have, set `CBSHARE_BIN=/path/to/cbshare`. To use an installed Chromium instead of Playwright's download, set `E2E_CHROMIUM_PATH=/usr/bin/google-chrome`.

Reports: add `--html report.html --self-contained-html` for an HTML report. When a test fails, its screenshots and Playwright traces are kept in `test-results/` (open a trace with `playwright show-trace <file>`), and the console output of every cbshare process it started is attached to the report.

## How it is organised

| File | Contents |
| --- | --- |
| `cases.yaml` | Every case of the plan: title, priority, and whether it is `automated`, `planned` (not yet), `manual` (with the reason), or covered by the whole `suite`; `manual_part` says what of an automated case is still checked by hand |
| `test_<group>.py` | The tests of one group of the plan (`test_su.py` for SU-01…SU-13, and so on) |
| `test_consistency.py` | Checks that `cases.yaml` and the tests agree |
| `harness.py` | Starting and stopping cbshare, reading its console, the API client, TOTP codes, certificates |
| `ui.py` | Browser helpers: screens, signing in, sending, finding messages |
| `conftest.py` | pytest fixtures (servers, browser clients) and the case and priority markers |
| `tools/summary.py` | Turns `--case-results results.json` into the Markdown table shown in CI |

Each test is marked with the case it automates:

```python
@pytest.mark.case("LOCK-01")
def test_lockout_after_5_failures(ready): ...
```

Its priority marker (`p1`, `p2`, `p3`) is added from `cases.yaml`. Useful fixtures:

- `server`: cbshare just started for the first time (`server.key`, `server.pin`, `server.fingerprint`, `server.output`, `server.wait_for(...)`, `server.restart()`).
- `ready`: a server whose setup is done: `ready.url`, `ready.secret` (the admin account's TOTP secret), `ready.accounts` (creates users on demand through the admin API, as the admin would), and `ready.api(source=None)` for an API client, optionally from another loopback address such as `127.0.0.2` (a second machine). `ready.api().login(ready.accounts, "anna")` signs in as anna, creating her first if needed.
- `make_server`, `make_ready`: the same, with extra command-line flags.
- `client()`: a new browser profile (its own cookies), like another person; `ui.sign_in(client(), ready.url, ready.accounts, "name")` signs it in.

cbshare accepts each one-time code only once per user. Use `harness.fresh_code(secret)` for a code to sign in with: it picks a time step not used yet, and waits if a test signs the same user in more than twice within 30 seconds. `harness.wrong_code(secret)` gives a code that is not valid.

The suite builds cbshare with `-tags testhooks`, which lets a test shorten the 24-hour window of a new QR code with the `CBSHARE_TEST_ENROLMENT_WINDOW` environment variable (USR-05). Release builds do not contain this hook. To run the suite against your own binary (`CBSHARE_BIN`), build it with that tag too.

Every test starts its own cbshare process, in its own folder and on its own port, so tests never depend on each other and run in parallel.

## Adding a test for a case

1. Write the test in the group's file and mark it with `@pytest.mark.case("ID")`.
2. In `cases.yaml`, change the case's `automation` from `planned` to `automated`.
3. Run `pytest --case ID`, then the whole suite.

`test_consistency.py` fails if a case marked `automated` has no test, or if a test names a case that is not marked `automated`.

## In continuous integration

The **External tests** workflow (`.github/workflows/e2e.yml`) has two scopes:

- `p1` (every pull request and `main`): the P1 cases in Chromium.
- `full` (every release, and by hand with *Run workflow*): every automated case in Chromium, slow ones included, plus every browser case in Firefox and WebKit, as three parallel jobs.

The release workflow (`.github/workflows/release.yml`, on `v*` tags) calls this workflow with `scope: full`, and publishes the release only if all three jobs pass. A failure is fixed like any other: fix the code or the test, delete the tag, and push it again.

Each job's page shows a table of results by case, and its `e2e-report-<name>` artifact holds the HTML report, JUnit XML, and the screenshots and traces of failed tests.

A few browser differences are handled in the tests: Playwright can grant clipboard access only in Chromium, so in Firefox and WebKit `ui.allow_clipboard` replaces the page's clipboard with an in-memory one that records what the Copy button writes.

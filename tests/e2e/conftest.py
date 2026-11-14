"""Fixtures and hooks of the external test suite.

Each test case of the manual test plan is a test marked with its ID:

    @pytest.mark.case("LOCK-01")
    def test_lockout_after_5_failures(ready): ...

The priority (p1, p2, p3) is added from cases.yaml, so `pytest -m p1`
runs the P1 cases. Every test gets its own cbshare process, folder and
port, so tests are independent and can run in parallel (pytest -n auto).
"""

from __future__ import annotations

import json
import os
import subprocess
import tempfile
from pathlib import Path
from types import SimpleNamespace

import pytest
import yaml

from harness import ADMIN_PASSWORD, Accounts, ApiClient, CbshareServer

HERE = Path(__file__).parent
REPO = HERE.parent.parent
CASES = yaml.safe_load((HERE / "cases.yaml").read_text())
BROWSER_FIXTURES = {"client", "page", "context", "new_context", "browser", "browser_type"}


def pytest_addoption(parser):
    parser.addoption("--case-results", default=None,
                     help="write the result of every case to this JSON file (for tools/summary.py)")
    parser.addoption("--case", action="append", default=[],
                     help="run only this test plan case, e.g. --case LOCK-01 (repeatable)")


def pytest_configure(config):
    config.addinivalue_line("markers", "case(id): the manual test plan case this test automates")
    for p in ("p1", "p2", "p3"):
        config.addinivalue_line("markers", f"{p}: priority {p.upper()} (added from cases.yaml)")
    config.addinivalue_line("markers", "slow: takes minutes (real timeouts); run before releases only")
    config.addinivalue_line("markers", "browser: uses a browser (added automatically); "
                            "the only tests worth repeating in Firefox and WebKit")


def pytest_collection_modifyitems(config, items):
    wanted = {c.upper() for c in config.getoption("--case")}
    unknown = wanted - set(CASES)
    if unknown:
        raise pytest.UsageError(f"--case: unknown case(s) {sorted(unknown)}")
    if wanted:
        keep = [i for i in items if (m := i.get_closest_marker("case")) and m.args[0] in wanted]
        config.hook.pytest_deselected(items=[i for i in items if i not in keep])
        items[:] = keep
    for item in items:
        m = item.get_closest_marker("case")
        if not m:
            continue
        cid = m.args[0]
        case = CASES.get(cid)
        if case is None:
            raise pytest.UsageError(f"{item.nodeid}: unknown case {cid!r} (not in cases.yaml)")
        if case["automation"] != "automated":
            raise pytest.UsageError(f"{item.nodeid}: case {cid} is {case['automation']!r} in cases.yaml, not 'automated'")
        item.add_marker(getattr(pytest.mark, case["priority"].lower()))
        item.user_properties.append(("case", cid))
    for item in items:
        if BROWSER_FIXTURES & set(getattr(item, "fixturenames", ())):
            item.add_marker(pytest.mark.browser)


# ---------- results for the summary ----------

_results: dict[str, dict] = {}


def pytest_runtest_logreport(report):
    props = dict(report.user_properties)
    cid = props.get("case")
    if not cid:
        return
    r = _results.setdefault(report.nodeid, {"case": cid, "nodeid": report.nodeid, "outcome": "passed", "duration": 0.0})
    r["duration"] += report.duration
    if report.failed:
        r["outcome"] = "failed" if report.when == "call" else "error"
    elif report.skipped and r["outcome"] == "passed":
        r["outcome"] = "skipped"


def pytest_sessionfinish(session):
    path = session.config.getoption("--case-results")
    if path and not hasattr(session.config, "workerinput"):  # controller only under xdist
        Path(path).write_text(json.dumps(sorted(_results.values(), key=lambda r: r["nodeid"]), indent=1))


# ---------- the cbshare binary ----------


@pytest.fixture(scope="session")
def cbshare_bin(tmp_path_factory) -> str:
    """The executable under test: $CBSHARE_BIN, or built from this repository."""
    path = os.environ.get("CBSHARE_BIN")
    if path:
        return str(Path(path).resolve())
    out = tmp_path_factory.mktemp("bin") / "cbshare"
    # testhooks: lets tests shorten the 24-hour enrolment window (USR-05).
    subprocess.run(["go", "build", "-tags", "testhooks", "-o", str(out), "./cmd/cbshare"], cwd=REPO, check=True,
                   env={**os.environ, "CGO_ENABLED": "0"})
    return str(out)


_consoles = pytest.StashKey[list]()


@pytest.fixture
def make_server(cbshare_bin, tmp_path, request):
    """Factory for cbshare processes; all are stopped after the test, and
    their console output is attached to the report of a failed test."""
    servers: list[CbshareServer] = []
    request.node.stash[_consoles] = servers

    def make(name: str = "server", **kw) -> CbshareServer:
        srv = CbshareServer(cbshare_bin, tmp_path / name, **kw)
        servers.append(srv)
        return srv

    yield make
    for srv in servers:
        try:
            srv.stop()
        except Exception:
            pass


@pytest.hookimpl(hookwrapper=True)
def pytest_runtest_makereport(item, call):
    outcome = yield
    report = outcome.get_result()
    if report.failed and report.when in ("setup", "call"):
        for i, srv in enumerate(item.stash.get(_consoles, [])):
            if srv.history:
                report.sections.append((f"cbshare console {i + 1}", "\n".join(srv.history)))


@pytest.fixture
def server(make_server) -> CbshareServer:
    """A cbshare started for the first time (setup not done yet)."""
    return make_server().start(password=ADMIN_PASSWORD)


def _ready(srv: CbshareServer) -> SimpleNamespace:
    secret = ApiClient(srv.base_url).setup(srv.pin)
    return SimpleNamespace(server=srv, secret=secret, url=srv.base_url,
                           accounts=Accounts(lambda: srv.base_url, secret),
                           api=lambda source=None: ApiClient(srv.base_url, source=source))


@pytest.fixture
def ready(server) -> SimpleNamespace:
    """A cbshare whose first-time setup is done: .server, .url, .api(),
    .secret (the admin account's TOTP secret) and .accounts, which creates
    users on demand: ready.api().login(ready.accounts, "anna")."""
    return _ready(server)


@pytest.fixture
def make_ready(make_server):
    """Factory for set-up servers started with extra flags."""
    def make(*extra: str, name: str = "server") -> SimpleNamespace:
        return _ready(make_server(name).start(*extra, password=ADMIN_PASSWORD))
    return make


# ---------- browsers ----------


@pytest.fixture(scope="session")
def browser_context_args(browser_context_args):
    return {
        **browser_context_args,
        "ignore_https_errors": True,  # cbshare's self-signed certificate
        "bypass_csp": True,           # lets tests evaluate scripts; CSP itself is tested in SEC cases
        "viewport": {"width": 1100, "height": 760},
    }


@pytest.fixture(scope="session")
def browser_type_launch_args(browser_type_launch_args, browser_name):
    """E2E_CHROMIUM_PATH points Chromium tests at an installed browser
    (useful where `playwright install` is not available)."""
    path = os.environ.get("E2E_CHROMIUM_PATH")
    if path and browser_name == "chromium":
        return {**browser_type_launch_args, "executable_path": path}
    return browser_type_launch_args


@pytest.fixture
def client(new_context):
    """Factory for browser clients: each call is a separate browser
    profile (own cookies), like another person on another machine.
    Screenshots and traces of every client are kept when a test fails."""
    def make(viewport: dict | None = None, **kw):
        page = new_context(**kw).new_page()
        if viewport:  # already set by browser_context_args; resize instead
            page.set_viewport_size(viewport)
        return page
    return make


@pytest.fixture
def scratch() -> Path:
    """A folder for files the test creates (uploads, downloads)."""
    with tempfile.TemporaryDirectory() as d:
        yield Path(d)

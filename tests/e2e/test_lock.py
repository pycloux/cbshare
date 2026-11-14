"""LOCK: lockout and failed sign-in alerts.

Different client addresses come from the loopback range (127.0.0.2,
127.0.0.3, ...), which Linux routes to the local machine.
"""

import time

import pytest
from playwright.sync_api import expect

from harness import fresh_code, wrong_code
from ui import SYSTEM_MESSAGES, sign_in


def login_body(username: str, code: str) -> dict:
    return {"username": username, "code": code}


@pytest.mark.case("LOCK-01")
def test_lockout_after_5_failures(ready):
    watcher = ready.api("127.0.0.2").login(ready.accounts, "nas")
    attacker = ready.api()  # 127.0.0.1

    for left in (4, 3, 2, 1):
        r = attacker.post("/api/login", login_body("admin", wrong_code(ready.secret)))
        assert r.status == 401 and r.body["remaining"] == left, r.body
        assert r.body["error"] == ("That username or code is not valid. "
                                   "Enter your username and the current 6-digit code from your authenticator app.")

    r = attacker.post("/api/login", login_body("admin", wrong_code(ready.secret)))
    assert r.status == 429 and r.body["code"] == "locked"
    assert r.body["error"] == "Too many failed attempts. Try again in 5 minutes."

    # Locked: even the right code is refused, and it is not reported.
    r = attacker.post("/api/login", login_body("admin", fresh_code(ready.secret)))
    assert r.status == 429 and r.body["error"].startswith("Too many failed attempts. Try again in")

    alerts = [m["body"] for m in watcher.messages() if m["body"].startswith("Failed sign-in attempt")]
    assert len(alerts) == 5, alerts
    assert alerts[-1].endswith("127.0.0.1 is locked out for 5 minutes.")


@pytest.mark.case("LOCK-03")
def test_the_lockout_only_affects_one_address(ready):
    ready.accounts.secret("other machine")  # created before 127.0.0.1 (the helper's address) is locked
    attacker = ready.api()
    for _ in range(5):
        attacker.post("/api/login", login_body("admin", wrong_code(ready.secret)))
    assert attacker.post("/api/login", login_body("admin", fresh_code(ready.secret))).status == 429

    other = ready.api("127.0.0.2")
    other.login(ready.accounts, "other machine")  # raises if refused


@pytest.mark.case("LOCK-06")
def test_alerts_reach_everyone_and_the_console(ready, client):
    pages = [sign_in(client(), ready.url, ready.accounts, name) for name in ("Anna laptop", "nas")]

    third = ready.api("127.0.0.3")
    assert third.post("/api/login", login_body("nas", wrong_code(ready.secret))).status == 401

    for page in pages:  # without reloading
        alert = page.locator(SYSTEM_MESSAGES, has_text="127.0.0.3")
        expect(alert).to_have_count(1)
        expect(alert).to_contain_text("Failed sign-in attempt from 127.0.0.3: wrong username or code.")
        expect(alert.locator(".msg-author")).to_have_text("cpsync")
        expect(alert.locator("button")).to_have_count(0)  # no Copy, Edit or Delete for normal users
    ready.server.wait_for(r"\[auth\] failed username or code from 127\.0\.0\.3 \(locked=false\)")


@pytest.mark.case("LOCK-02")
def test_the_lockout_expires(make_ready):
    r = make_ready("--lockout", "5s")  # 5 seconds instead of 5 minutes
    api = r.api()
    r.accounts.secret("back again")  # created before the lockout
    for _ in range(5):
        api.post("/api/login", login_body("admin", wrong_code(r.secret)))
    assert api.post("/api/login", login_body("admin", fresh_code(r.secret))).status == 429
    time.sleep(6)
    api.login(r.accounts, "back again")
    other = r.api()  # same address: a new series starts at 4 attempts left
    assert other.post("/api/login", login_body("admin", wrong_code(r.secret))).body["remaining"] == 4


@pytest.mark.case("LOCK-04")
def test_a_success_resets_the_count(ready):
    api = ready.api()
    ready.accounts.secret("anna")
    for _ in range(3):
        api.post("/api/login", login_body("anna", wrong_code(ready.secret)))
    api.login(ready.accounts, "anna")
    api.expect(api.post("/api/logout"), 200)
    r = api.post("/api/login", login_body("anna", wrong_code(ready.secret)))
    assert r.status == 401 and r.body["remaining"] == 4


@pytest.mark.case("LOCK-05")
def test_every_kind_of_credential_counts(ready):
    api = ready.api().login(ready.accounts, "admin")  # only the admin account may try the password
    for left in (4, 3):
        r = api.admin_login("not the admin password")
        assert r.status == 401 and r.body["remaining"] == left, r.body
    alerts = [m for m in api.messages() if "wrong admin password" in m["body"]]
    assert len(alerts) == 2
    api.expect(api.post("/api/logout"), 200)

    for left in (2, 1):
        r = api.post("/api/login", login_body("admin", wrong_code(ready.secret)))
        assert r.status == 401 and r.body["remaining"] == left, r.body
    r = api.post("/api/login", login_body("admin", wrong_code(ready.secret)))
    assert r.status == 429 and r.body["code"] == "locked"

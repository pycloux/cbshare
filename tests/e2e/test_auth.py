"""AUTH: sign-in, names and sessions."""

import re
import time

import pytest
from playwright.sync_api import expect

from harness import ApiClient, fresh_code, totp_code, wrong_code
from ui import expect_screen, form_error, message, send, sign_in, tool


@pytest.mark.case("AUTH-01")
def test_sign_in_with_the_current_code(ready, client):
    secret = ready.accounts.secret("Anna laptop")
    page = client()
    page.goto(ready.url + "/")
    expect_screen(page, "login")
    page.fill("#login-username", "Anna laptop")
    page.fill("#login-code", fresh_code(secret))
    page.click("#form-login button[type=submit]")
    expect_screen(page, "chat")  # no name to choose: it is the account's
    expect(page.locator("#me-name")).to_have_text("Anna laptop")
    expect(page.locator("#status-text")).to_have_text("Live")
    ready.server.wait_for(r'\[auth\] sign-in as "Anna laptop" from 127\.0\.0\.1')

    # A code typed with a space in the middle is accepted too.
    code = fresh_code(secret)
    r = ApiClient(ready.url).try_login("Anna laptop", f"{code[:3]} {code[3:]}")
    assert r.status == 200, r.body

    # The browser remembers the username: next time only the code is asked.
    page.click("#btn-logout")
    expect_screen(page, "login")
    expect(page.locator("#login-username")).to_have_value("Anna laptop")
    expect(page.locator("#login-code")).to_be_focused()


INVALID = ("That username or code is not valid. "
           "Enter your username and the current 6-digit code from your authenticator app.")


@pytest.mark.case("AUTH-02")
def test_a_wrong_code_is_refused(ready, client):
    watcher = ready.api("127.0.0.2").login(ready.accounts, "nas")
    anna = ready.accounts.secret("anna")
    page = client()
    page.goto(ready.url + "/")
    page.fill("#login-username", "anna")
    page.fill("#login-code", wrong_code(anna))
    page.click("#form-login button[type=submit]")
    expect(form_error(page, "#form-login")).to_have_text(f"{INVALID} 4 attempts left before a lockout.")
    expect_screen(page, "login")
    assert any(m["kind"] == "system" and m["body"] == "Failed sign-in attempt from 127.0.0.1: wrong username or code."
               for m in watcher.messages())

    # An unknown name, or someone else's code, gets exactly the same answer:
    # the response does not tell which names exist.
    page.fill("#login-username", "nobody")
    page.fill("#login-code", fresh_code(anna))
    page.click("#form-login button[type=submit]")
    expect(form_error(page, "#form-login")).to_have_text(f"{INVALID} 3 attempts left before a lockout.")
    page.fill("#login-username", "nas")
    page.fill("#login-code", fresh_code(anna))
    page.click("#form-login button[type=submit]")
    expect(form_error(page, "#form-login")).to_have_text(f"{INVALID} 2 attempts left before a lockout.")
    assert not any("nobody" in m["body"] for m in watcher.messages())


@pytest.mark.case("AUTH-06")
def test_sign_out(ready, client):
    page = sign_in(client(), ready.url, ready.accounts, "Anna laptop")
    send(page, "visible before signing out")
    page.click("#btn-logout")
    expect_screen(page, "login")
    expect(page.locator("#feed-inner article")).to_have_count(0)
    assert "visible before signing out" not in page.content()

    # Back, then Forward again (the page may come from the browser's
    # cache), then reload: the chat never comes back.
    page.go_back()
    assert "visible before signing out" not in page.content()
    page.go_forward()
    expect_screen(page, "login")
    page.reload()
    expect_screen(page, "login")
    assert "visible before signing out" not in page.content()


@pytest.mark.case("AUTH-07")
def test_closing_the_browser_ends_the_session(ready, browser_type, browser_type_launch_args, tmp_path):
    profile = tmp_path / "profile"
    args = {**browser_type_launch_args, "ignore_https_errors": True, "bypass_csp": True}

    ctx = browser_type.launch_persistent_context(profile, **args)
    sign_in(ctx.new_page(), ready.url, ready.accounts, "Anna laptop")
    session = [c for c in ctx.cookies() if c["name"] == "cbshare_session"]
    assert session and session[0]["expires"] == -1, "the session cookie must end with the browser"
    ctx.close()  # every window closed

    ctx = browser_type.launch_persistent_context(profile, **args)
    try:
        page = ctx.new_page()
        page.goto(ready.url + "/")
        expect_screen(page, "login")
    finally:
        ctx.close()


@pytest.mark.case("AUTH-08")
@pytest.mark.parametrize("mode", ["sse", "polling"])
def test_inactivity_timeout(make_ready, client, mode):
    # 10 s instead of 30 min keeps the test short; the server checks every 15 s.
    r = make_ready("--session-timeout", "10s", "--mode", mode, "--poll-interval", "1s")
    page = sign_in(client(), r.url, r.accounts, "Anna laptop")

    # The page stays open, untouched: background updates must not keep it alive.
    expect_screen(page, "login", timeout=45_000)
    expect(form_error(page, "#form-login")).to_have_text(
        re.compile(r"You were signed out|Your session has ended"))
    if mode == "sse":
        # Expired sessions are logged by the server's periodic clean-up. In
        # polling mode the browser's next request usually finds the session
        # expired first and ends it without a log line.
        r.server.wait_for(r"session\(s\) expired after 10s of inactivity")


@pytest.mark.case("AUTH-03")
def test_clock_tolerance(ready):
    secret = ready.accounts.secret("anna")
    now = time.time()
    previous = totp_code(secret, now - 30)  # the code just replaced in the app
    r = ApiClient(ready.url).try_login("anna", previous)
    assert r.status == 200, r.body

    old = totp_code(secret, now - 150)  # noted more than 2 minutes earlier
    if old not in {totp_code(secret, now + d) for d in (-30, 0, 30)}:
        r = ApiClient(ready.url).try_login("anna", old)
        assert r.status == 401


@pytest.mark.case("AUTH-04")
def test_usernames_match_whatever_their_case(ready, client):
    secret = ready.accounts.secret("Build Server")
    for typed in ("build server", "  BUILD   server "):
        api = ApiClient(ready.url)
        r = api.expect(api.try_login(typed, fresh_code(secret)), 200)
        assert r.body["username"] == "Build Server"  # the account's own spelling
        assert api.get("/api/state").body["username"] == "Build Server"

    page = client()
    page.goto(ready.url + "/")
    page.fill("#login-username", "y" * 40)
    assert len(page.input_value("#login-username")) == 32  # the field stops at 32
    # There is no name to choose any more: the old endpoint is gone.
    assert ApiClient(ready.url).post("/api/username", {"username": "x"}).status in (404, 405)


@pytest.mark.case("AUTH-05")
def test_same_user_in_two_browsers(ready, client):
    one = sign_in(client(), ready.url, ready.accounts, "ops")
    two = sign_in(client(), ready.url, ready.accounts, "ops")
    send(one, "posted from the first browser")
    for page in (one, two):
        msg = message(page, "posted from the first browser")
        expect(msg).to_have_class(re.compile(r"\bown\b"))
        expect(tool(msg, "Edit")).to_be_visible()
        expect(tool(msg, "Delete")).to_be_visible()


@pytest.mark.case("AUTH-09")
def test_activity_keeps_the_session_alive(make_ready, client):
    r = make_ready("--session-timeout", "10s")
    r.accounts.secret("anna")
    r.accounts.logout_admin()  # the admin's idle session would expire and be logged
    page = sign_in(client(), r.url, r.accounts, "anna")
    for i in range(7):  # 28 s of activity, well over the 10 s timeout
        send(page, f"still here {i}")
        page.wait_for_timeout(4000)
        expect_screen(page, "chat")
    assert "expired" not in r.server.output


@pytest.mark.case("AUTH-10")
def test_reloading_keeps_the_session(ready, client):
    page = sign_in(client(), ready.url, ready.accounts, "Anna laptop")
    page.reload()
    expect_screen(page, "chat")
    expect(page.locator("#me-name")).to_have_text("Anna laptop")
    tab = page.context.new_page()  # a new tab of the same browser
    tab.goto(ready.url + "/")
    expect_screen(tab, "chat")
    expect(tab.locator("#me-name")).to_have_text("Anna laptop")


@pytest.mark.case("AUTH-11")
def test_a_server_restart_ends_every_session(ready, client):
    pages = [sign_in(client(), ready.url, ready.accounts, name) for name in ("anna", "nas")]
    ready.server.restart(same_port=True)
    for page in pages:
        page.reload()
        expect_screen(page, "login")
    ApiClient(ready.server.base_url).login(ready.accounts, "anna")  # same account and code

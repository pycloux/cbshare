"""SEC: security checks, made with plain HTTP requests like the curl
commands of the manual plan."""

import pytest
import requests
from playwright.sync_api import expect

from harness import ADMIN_PASSWORD, Accounts, ApiClient, fresh_code, totp_code, wrong_code
from ui import sign_in


def user_messages(api):
    return [m for m in api.messages() if m["kind"] != "system"]


def raw(url, method="GET", **kw):
    """A request without cookies or browser headers."""
    s = requests.Session()
    s.trust_env = False
    return s.request(method, url, verify=False, timeout=15, **kw)


@pytest.mark.case("SEC-01")
def test_security_headers(ready):
    for path in ("/", "/api/state"):
        h = raw(ready.url + path).headers
        assert h["Content-Security-Policy"].startswith("default-src 'none'"), path
        assert h["X-Frame-Options"] == "DENY", path
        assert h["X-Content-Type-Options"] == "nosniff", path
        assert h["Referrer-Policy"] == "no-referrer", path
        assert h["Strict-Transport-Security"] == "max-age=86400", path
    assert raw(ready.url + "/api/state").headers["Cache-Control"] == "no-store"


@pytest.mark.case("SEC-02")
def test_the_api_requires_a_session(ready):
    for path in ("/api/messages", "/api/events", "/api/files/1"):
        r = raw(ready.url + path)
        assert r.status_code == 401, path
        assert r.json() == {"error": "Your session has ended. Sign in again.", "code": "unauthenticated"}, path


@pytest.mark.case("SEC-03")
def test_state_changes_require_the_csrf_token(ready):
    nas = ready.api().login(ready.accounts, "nas")
    token = nas.csrf

    nas.csrf = ""
    r = nas.post("/api/messages", {"text": "no token"})
    assert r.status == 403 and r.body["code"] == "csrf"
    nas.csrf = "wrong"
    r = nas.post("/api/messages", {"text": "wrong token"})
    assert r.status == 403 and r.body["code"] == "csrf"

    nas.csrf = token
    nas.expect(nas.post("/api/messages", {"text": "right token"}), 201)
    assert [(m["author"], m["body"]) for m in user_messages(nas)] == [("nas", "right token")]


@pytest.mark.case("SEC-04")
def test_the_server_enforces_ownership(ready):
    anna = ready.api().login(ready.accounts, "Anna laptop")
    nas = ready.api().login(ready.accounts, "nas")
    msg = anna.post_text("Anna's message")
    path = f"/api/messages/{msg['id']}"

    for r in (nas.put(path, {"text": "hacked"}), nas.delete(path)):
        assert r.status == 403 and r.body["error"] == "You can only change your own messages."
    assert [m["body"] for m in user_messages(anna)] == ["Anna's message"]

    ready.api("127.0.0.2").post("/api/login", {"username": "nas", "code": wrong_code(ready.secret)})
    system = next(m for m in nas.messages() if m["body"].startswith("Failed sign-in"))
    sys_path = f"/api/messages/{system['id']}"
    r = nas.put(sys_path, {"text": "x"})
    assert r.status == 403 and r.body["error"] == "System messages cannot be edited."
    r = nas.delete(sys_path)
    assert r.status == 403 and r.body["error"] == "Only the admin can delete system messages."


@pytest.mark.case("SEC-05")
def test_requests_from_other_sites_are_rejected(ready):
    nas = ready.api().login(ready.accounts, "nas")
    r = nas.post("/api/messages", {"text": "x"}, headers={"Origin": "https://evil.example"})
    assert r.status == 403 and r.body["code"] == "cross_origin"
    r = nas.post("/api/messages", {"text": "x"}, origin=False, headers={"Sec-Fetch-Site": "cross-site"})
    assert r.status == 403 and r.body["code"] == "cross_origin"
    assert user_messages(nas) == []

    r = raw(ready.url + "/api/login", "POST", data='{"code":"123456"}', headers={"Content-Type": "text/plain"})
    assert r.status_code == 415
    assert r.json()["error"] == "Send the request body as application/json."


@pytest.mark.case("SEC-06")
def test_admin_endpoints_require_admin_rights(ready):
    nas = ready.api().login(ready.accounts, "nas")
    r = nas.post("/api/admin/rotate")
    assert r.status == 403 and r.body["code"] == "admin_required"
    assert "qr" not in r.body


@pytest.mark.case("SEC-08")
def test_session_cookie_protection(ready, client):
    page = sign_in(client(), ready.url, ready.accounts, "Anna laptop")
    cookie = next(c for c in page.context.cookies() if c["name"] == "cpsync_session")
    assert cookie["httpOnly"] is True
    assert cookie["secure"] is True
    assert cookie["sameSite"] == "Strict"
    assert cookie["expires"] == -1, "ends with the browser session"
    assert "cpsync_session" not in page.evaluate("document.cookie")


@pytest.mark.case("SEC-07")
def test_the_setup_pin_is_useless_after_setup(ready):
    r = raw(ready.url + "/api/setup/pin", "POST", json={"pin": ready.server.pin},
            headers={"Origin": ready.url})
    assert r.status_code == 409
    assert r.json()["error"] == "Setup is already complete. Sign in with your authenticator code."
    assert "secret" not in r.json() and "qr" not in r.json()


@pytest.mark.timeout(90)
@pytest.mark.case("SEC-09")
def test_the_page_cannot_be_framed(ready, client, browser_name):
    if browser_name == "firefox":
        pytest.skip("Playwright's Firefox stops responding once a frame is refused; "
                    "the X-Frame-Options and CSP headers are checked in SEC-01")
    page = client()
    # A page on another site that tries to show cpsync in a frame.
    page.route("https://framer.test/", lambda route: route.fulfill(
        content_type="text/html",
        body=f'<iframe id="f" src="{ready.url}/" width="600" height="400"></iframe>'))
    page.goto("https://framer.test/", wait_until="domcontentloaded")  # a refused frame may never "load"
    expect(page.locator("#f")).to_be_attached()
    page.wait_for_timeout(2000)
    # The browser refuses to show cpsync in the frame: none of its screens
    # are there. (The frame's error page is not inspected: some engines
    # never answer scripts sent to it.)
    expect(page.frame_locator("#f").locator("#screen-login, #screen-setup-pin, #compose")).to_have_count(0)


@pytest.mark.case("SEC-10")
def test_secrets_never_reach_the_console(make_server, client, scratch):
    srv = make_server()
    srv.start(password=ADMIN_PASSWORD)
    banner, _, _ = srv.output.partition("listening on")
    key = srv.key
    api = ApiClient(srv.base_url)
    secret = api.setup(srv.pin)
    api.login(secret, "admin")
    text = "SEC10-private-message-6b1d"
    api.post_text(text)
    api.upload("private.txt", b"SEC10-private-file")
    assert api.admin_login(ADMIN_PASSWORD).status == 200
    # A user created by the admin: their secret must not be logged either.
    nas_secret = api.expect(api.create_user("nas"), 201).body["enrollment"]["secret"]
    page = sign_in(client(), srv.base_url, nas_secret, "nas")
    cookies = [c["value"] for c in page.context.cookies()]
    csrf = page.evaluate("fetch('/api/state').then(r => r.json()).then(s => s.csrf)")

    srv.restart()
    console = "\n".join(line for line in srv.history if not line.startswith("$ "))
    assert banner.count(key) >= 1
    assert console.count(key) == banner.count(key), "the key appears only in the first-start banner"
    for value, what in [(ADMIN_PASSWORD, "admin password"), (secret, "TOTP secret"), (nas_secret, "user's TOTP secret"),
                        (text, "message text"),
                        ("SEC10-private-file", "file content"), (csrf, "CSRF token")] + \
                       [(c, "session cookie") for c in cookies if len(c) >= 16]:
        assert value not in console, f"the {what} is in the console"

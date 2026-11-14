"""USR: user accounts managed by the admin (Admin tools → Users).

Each user signs in with their own QR code. The admin creates users,
gives them a new QR code, revokes their access or deletes them."""

import io
import re
import time
from types import SimpleNamespace
from urllib.parse import parse_qs, unquote, urlparse

import pytest
import zxingcpp
from PIL import Image
from playwright.sync_api import expect

from harness import ADMIN_PASSWORD, Accounts, ApiClient, fresh_code, totp_code, wrong_code
from ui import SYSTEM_MESSAGES, expect_screen, form_error, message, send, sign_in, toast, tool


def unlocked_admin_page(page, url, accounts):
    """Signs the admin in and unlocks admin tools in the browser."""
    sign_in(page, url, accounts, "admin")
    page.click("#btn-admin")
    page.fill("#admin-password", ADMIN_PASSWORD)
    page.click("#form-admin button[type=submit]")
    expect(page.locator("#admin-badge")).to_be_visible()
    return page


def open_users(page):
    page.click("#btn-admin")
    expect(page.locator("#dlg-admin-tools")).to_be_visible()
    page.click("#btn-users")
    expect(page.locator("#dlg-users")).to_be_visible()


def add_user_in_ui(page, name):
    page.fill("#new-username", name)
    page.click("#btn-add-user")


def user_row(page, name):
    return page.locator("#user-list li.user", has=page.locator(".user-name", has_text=re.compile(f"^{re.escape(name)}")))


def admin_api(ready) -> ApiClient:
    return ready.accounts.admin()


def read_qr(page) -> str:
    """Decodes the QR code shown in the user dialog, like an authenticator app."""
    shot = page.locator("#user-qr-img").screenshot()
    codes = zxingcpp.read_barcodes(Image.open(io.BytesIO(shot)))
    assert codes, "the QR code could not be decoded"
    return codes[0].text


@pytest.mark.case("USR-01")
def test_add_a_user_and_sign_in(ready, client):
    admin = unlocked_admin_page(client(), ready.url, ready.accounts)
    open_users(admin)
    expect(admin.locator("#users-count")).to_contain_text("1 of 10 users")
    add_user_in_ui(admin, "  Bob   Smith ")

    dlg = admin.locator("#dlg-user-qr")
    expect(dlg).to_be_visible()
    expect(admin.locator("#user-qr-title")).to_have_text("QR code for Bob Smith")
    expect(admin.locator("#user-qr-deadline")).to_contain_text('the username "Bob Smith"')
    expect(dlg.locator(".note")).to_contain_text("shown only once")
    uri = urlparse(read_qr(admin))
    host = ready.url.removeprefix("https://")
    assert unquote(uri.path) == f"/cbshare:Bob Smith@{host}"
    secret = parse_qs(uri.query)["secret"][0]
    dlg.locator("summary").click()
    assert admin.inner_text("#user-qr-secret").strip() == secret
    dlg.locator("button", has_text="Done").click()
    expect(dlg).to_be_hidden()

    row = user_row(admin, "Bob Smith")
    expect(row.locator(".user-status")).to_contain_text("Waiting for first sign-in, until")
    expect(admin.locator("#users-count")).to_contain_text("2 of 10 users")
    expect(admin.locator(SYSTEM_MESSAGES, has_text="The admin added Bob Smith.")).to_be_visible()
    ready.server.wait_for(r'\[users\] "admin" created user "Bob Smith"')

    # Bob signs in with the code from his app, on his own machine.
    bob = sign_in(client(), ready.url, secret, "Bob Smith")
    send(bob, "hello from Bob")
    expect(message(admin, "hello from Bob")).to_be_visible()
    ready.server.wait_for(r'\[users\] "Bob Smith" signed in for the first time')

    # The admin's list shows him active.
    admin.click("#dlg-users button[data-close]")
    open_users(admin)
    expect(user_row(admin, "Bob Smith").locator(".user-status")).to_have_text("Active")


@pytest.mark.case("USR-02")
def test_username_rules(ready, client):
    api = admin_api(ready)
    for name, error in (("   ", "Enter a username."),
                        ("cbshare", '"cbshare" is reserved for system messages.'),
                        ("CBSHARE", '"cbshare" is reserved for system messages.'),
                        ("admin", '"admin" is reserved for the admin account.'),
                        ("Admin", '"admin" is reserved for the admin account.'),
                        ("x" * 33, "Use 32 characters or fewer.")):
        r = api.create_user(name)
        assert r.status == 400 and r.body["code"] == "invalid_username" and r.body["error"] == error, (name, r.body)
    r = api.expect(api.create_user("  build    server  "), 201)
    assert r.body["user"]["name"] == "build server"
    r = api.create_user("BUILD SERVER")
    assert r.status == 409 and r.body["code"] == "name_taken", r.body
    assert r.body["error"] == 'There is already a user named "BUILD SERVER".'

    admin = unlocked_admin_page(client(), ready.url, ready.accounts)
    open_users(admin)
    admin.fill("#new-username", "y" * 40)
    assert len(admin.input_value("#new-username")) == 32  # the field stops at 32
    add_user_in_ui(admin, "Build Server")
    expect(form_error(admin, "#form-add-user")).to_have_text('There is already a user named "Build Server".')
    add_user_in_ui(admin, "admin")
    expect(form_error(admin, "#form-add-user")).to_have_text('"admin" is reserved for the admin account.')


@pytest.mark.case("USR-03")
def test_the_user_limit(make_ready, client):
    r = make_ready("--max-users", "3")
    assert "max users           3" in r.server.output
    api = r.accounts.admin()
    api.expect(api.create_user("one"), 201)
    api.expect(api.create_user("two"), 201)
    full = api.create_user("three")
    assert full.status == 409 and full.body["code"] == "user_limit", full.body
    assert "limited to 3 users" in full.body["error"]

    admin = unlocked_admin_page(client(), r.url, r.accounts)
    open_users(admin)
    expect(admin.locator("#users-count")).to_contain_text("3 of 3 users")
    expect(admin.locator("#new-username")).to_be_disabled()
    expect(admin.locator("#btn-add-user")).to_be_disabled()
    expect(admin.locator("#users-limit")).to_contain_text("The limit of 3 users is reached")

    # Deleting a user frees a place.
    user_row(admin, "two").locator("button", has_text="Delete").click()
    admin.locator("#dlg-user-confirm button[value=confirm]").click()
    expect(admin.locator("#users-count")).to_contain_text("2 of 3 users")
    expect(admin.locator("#btn-add-user")).to_be_enabled()


@pytest.mark.case("USR-04")
def test_the_qr_code_is_shown_only_once(ready, client):
    admin = unlocked_admin_page(client(), ready.url, ready.accounts)
    open_users(admin)
    add_user_in_ui(admin, "carol")
    expect(admin.locator("#dlg-user-qr")).to_be_visible()
    admin.locator("#dlg-user-qr summary").click()
    secret = admin.inner_text("#user-qr-secret").strip()
    admin.keyboard.press("Escape")  # closed without "Done"
    expect(admin.locator("#dlg-user-qr")).to_be_hidden()
    assert admin.get_attribute("#user-qr-img", "src") is None
    expect(admin.locator("#user-qr-secret")).to_have_text("")
    assert secret not in admin.content()

    # No API response carries it again.
    api = admin_api(ready)
    for path in ("/api/admin/users", "/api/state", "/api/messages"):
        assert secret not in api.get(path).raw.decode(), path
    assert secret not in ready.server.output, "a secret must never be logged"


@pytest.mark.case("USR-05")
def test_an_unused_qr_code_expires(make_server):
    srv = make_server().start(password=ADMIN_PASSWORD, env={"CBSHARE_TEST_ENROLMENT_WINDOW": "6s"})
    admin_secret = ApiClient(srv.base_url).setup(srv.pin)
    accounts = Accounts(lambda: srv.base_url, admin_secret)
    api = accounts.admin()
    late = api.expect(api.create_user("late"), 201).body["enrollment"]["secret"]
    early = api.expect(api.create_user("early"), 201).body["enrollment"]["secret"]
    ApiClient(srv.base_url).login(early, "early")  # signed in in time: stays active

    time.sleep(7)
    r = ApiClient(srv.base_url).try_login("late", fresh_code(late))
    assert r.status == 401, r.body
    users = {u["name"]: u for u in api.users()}
    assert users["late"]["status"] == "expired"
    assert users["early"]["status"] == "active"
    ApiClient(srv.base_url).login(early, "early")
    assert any(m["body"] == "The QR code of late expired before it was used. The admin can create a new one."
               for m in api.messages())
    srv.wait_for(r'\[users\] the QR code of "late" expired before its first sign-in')


@pytest.mark.case("USR-06")
def test_revoke(ready, client):
    dave = sign_in(client(), ready.url, ready.accounts, "dave")
    dave_api = ready.api("127.0.0.2").login(ready.accounts, "dave")
    send(dave, "dave was here")
    admin = unlocked_admin_page(client(), ready.url, ready.accounts)
    open_users(admin)
    row = user_row(admin, "dave")
    expect(row.locator(".user-meta")).to_contain_text("2 sessions open")
    row.locator("button", has_text="Revoke").click()
    expect(admin.locator("#user-confirm-text")).to_contain_text("dave is signed out")
    admin.locator("#dlg-user-confirm button[value=confirm]").click()
    expect(toast(admin)).to_have_text("dave can no longer sign in.")
    expect(row.locator(".user-status")).to_have_text("Revoked")
    expect(row.locator("button", has_text="Revoke")).to_have_count(0)

    expect_screen(dave, "login", timeout=5000)
    expect(form_error(dave, "#form-login")).to_contain_text("You were signed out")
    assert dave_api.get("/api/messages").status == 401
    r = ApiClient(ready.url).try_login("dave", fresh_code(ready.accounts.secret("dave")))
    assert r.status == 401
    expect(message(admin, "dave was here")).to_be_visible()  # messages stay
    expect(admin.locator(SYSTEM_MESSAGES, has_text="The admin revoked dave's access.")).to_be_visible()


@pytest.mark.case("USR-07")
def test_new_qr_code(ready, client):
    erin = sign_in(client(), ready.url, ready.accounts, "erin")
    old = ready.accounts.secret("erin")
    admin = unlocked_admin_page(client(), ready.url, ready.accounts)
    open_users(admin)
    user_row(admin, "erin").locator("button", has_text="New QR code").click()
    expect(admin.locator("#user-confirm-text")).to_contain_text("current code stops working")
    admin.locator("#dlg-user-confirm button[value=confirm]").click()
    expect(admin.locator("#dlg-user-qr")).to_be_visible()
    admin.locator("#dlg-user-qr summary").click()
    new = admin.inner_text("#user-qr-secret").strip()
    assert new and new != old
    admin.locator("#dlg-user-qr button", has_text="Done").click()
    expect(user_row(admin, "erin").locator(".user-status")).to_contain_text("Waiting for first sign-in")

    expect_screen(erin, "login", timeout=5000)
    if totp_code(old) != totp_code(new):
        assert ApiClient(ready.url).try_login("erin", fresh_code(old)).status == 401
    sign_in(erin, ready.url, new, "erin")


@pytest.mark.case("USR-08")
def test_delete_keeps_messages(ready, client):
    frank = ready.api().login(ready.accounts, "frank")
    frank.post_text("frank's last words")
    viewer = sign_in(client(), ready.url, ready.accounts, "nas")
    admin = unlocked_admin_page(client(), ready.url, ready.accounts)
    open_users(admin)
    user_row(admin, "frank").locator("button", has_text="Delete").click()
    expect(admin.locator("#user-confirm-text")).to_contain_text("Their messages stay in the chat")
    admin.locator("#dlg-user-confirm button[value=confirm]").click()
    expect(toast(admin)).to_have_text("frank was removed.")
    expect(user_row(admin, "frank")).to_have_count(0)

    assert frank.get("/api/messages").status == 401
    for page in (viewer, admin):
        msg = message(page, "frank's last words")
        expect(msg).to_be_visible()
        expect(msg.locator(".msg-author")).to_have_text("frank(deleted user)")
        expect(msg).to_have_class(re.compile(r"\bdeleted-author\b"))
    expect(admin.locator(SYSTEM_MESSAGES, has_text="The admin removed frank.")).to_be_visible()
    # The admin can still moderate them; others cannot.
    expect(tool(message(admin, "frank's last words"), "Delete")).to_be_visible()
    expect(tool(message(viewer, "frank's last words"), "Delete")).to_have_count(0)


@pytest.mark.case("USR-09")
def test_a_deleted_name_can_be_reused(ready):
    old = ready.api().login(ready.accounts, "gina")
    msg = old.post_text("from the first gina")
    api = admin_api(ready)
    api.expect(api.delete(f"/api/admin/users/{api.user_id('gina')}"), 200)

    new_secret = api.expect(api.create_user("Gina"), 201).body["enrollment"]["secret"]
    gina = ApiClient(ready.url).login(new_secret, "gina")
    assert gina.get("/api/state").body["username"] == "Gina"
    path = f"/api/messages/{msg['id']}"
    for r in (gina.put(path, {"text": "mine now"}), gina.delete(path)):
        assert r.status == 403 and r.body["error"] == "You can only change your own messages."
    kept = next(m for m in gina.messages() if m["id"] == msg["id"])
    assert kept["author"] == "gina" and kept.get("authorDeleted") is True


@pytest.mark.case("USR-10")
def test_codes_are_personal(ready):
    hank = ready.accounts.secret("hank")
    ivy = ready.accounts.secret("ivy")
    c = ApiClient(ready.url)
    assert c.try_login("ivy", fresh_code(hank)).status == 401
    assert c.try_login("hank", fresh_code(ivy)).status == 401
    assert c.try_login("hank", fresh_code(hank)).status == 200
    assert c.get("/api/state").body["username"] == "hank"


@pytest.mark.case("USR-11")
def test_the_admin_account_is_protected(ready, client):
    api = admin_api(ready)
    admin_id = next(u["id"] for u in api.users() if u["admin"])
    for r in (api.post(f"/api/admin/users/{admin_id}/revoke"), api.post(f"/api/admin/users/{admin_id}/qr"),
              api.delete(f"/api/admin/users/{admin_id}")):
        assert r.status == 403 and r.body["code"] == "admin_account", r.body

    admin = unlocked_admin_page(client(), ready.url, ready.accounts)
    open_users(admin)
    row = user_row(admin, "admin")
    expect(row.locator(".badge")).to_have_text("Admin")
    expect(row.locator("button")).to_have_count(0)  # no New QR code, Revoke or Delete


@pytest.mark.case("USR-12")
def test_only_the_admin_user_sees_admin(ready, client):
    jill = sign_in(client(), ready.url, ready.accounts, "jill")
    expect(jill.locator("#btn-admin")).to_be_hidden()
    api = ready.api().login(ready.accounts, "jill")
    assert api.get("/api/state").body["isAdminUser"] is False
    for _ in range(6):  # refused before the password is checked: never a lockout
        r = api.admin_login()
        assert r.status == 403 and r.body["code"] == "admin_only", r.body
    assert api.get("/api/admin/users").status == 403
    assert not any("wrong admin password" in m["body"] for m in api.messages())

    admin = sign_in(client(), ready.url, ready.accounts, "admin")
    expect(admin.locator("#btn-admin")).to_be_visible()
    expect(admin.locator("#btn-admin")).to_have_text("Admin")


@pytest.mark.case("USR-13")
def test_a_replayed_code_is_refused(ready):
    secret = ready.accounts.secret("kim")
    code = fresh_code(secret)
    first = ready.api()
    assert first.try_login("kim", code).status == 200
    second = ready.api("127.0.0.2")  # someone who saw the code being typed
    r = second.try_login("kim", code)
    assert r.status == 401, r.body
    ready.server.wait_for(r'\[auth\] reused one-time code for "kim"')

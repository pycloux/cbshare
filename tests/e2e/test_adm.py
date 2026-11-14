"""ADM: admin tools and replacing the admin's own code.

The admin is the "admin" account created at setup; in these tests its
browser is called anna, and nas is a regular user."""

import re

import pytest
from playwright.sync_api import expect

from harness import ADMIN_PASSWORD, ApiClient, fresh_code, totp_code, wrong_code
from ui import SYSTEM_MESSAGES, expect_screen, form_error, message, message_by_id, send, sign_in, toast, tool


@pytest.fixture
def two(ready, client):
    """The admin's browser (anna) and a regular user's (nas)."""
    anna = sign_in(client(), ready.url, ready.accounts, "admin")
    nas = sign_in(client(), ready.url, ready.accounts, "nas")
    return anna, nas


def unlock_admin(page, password=ADMIN_PASSWORD):
    page.click("#btn-admin")
    expect(page.locator("#dlg-admin")).to_be_visible()
    page.fill("#admin-password", password)
    page.click("#form-admin button[type=submit]")


@pytest.mark.case("ADM-01")
def test_unlock_admin(ready, two):
    anna, nas = two
    unlock_admin(anna, "not the password")
    expect(form_error(anna, "#form-admin")).to_have_text(
        "That password is not the admin password. 4 attempts left before a lockout.")
    anna.fill("#admin-password", ready.server.key)  # the database key is not the admin password
    anna.click("#form-admin button[type=submit]")
    expect(form_error(anna, "#form-admin")).to_have_text(
        "That password is not the admin password. 3 attempts left before a lockout.")
    expect(nas.locator(SYSTEM_MESSAGES, has_text="wrong admin password")).to_have_count(2)

    anna.fill("#admin-password", ADMIN_PASSWORD)
    anna.click("#form-admin button[type=submit]")
    expect(anna.locator("#dlg-admin")).to_be_hidden()
    expect(toast(anna)).to_have_text("Admin unlocked. You can edit and delete any message.")
    expect(anna.locator("#admin-badge")).to_be_visible()
    expect(anna.locator("#btn-admin")).to_have_text("Admin tools")
    ready.server.wait_for(r'\[admin\] "admin" unlocked admin')

    # Other users have no Admin button, and the server refuses them
    # without counting an attempt.
    expect(nas.locator("#admin-badge")).to_be_hidden()
    expect(nas.locator("#btn-admin")).to_be_hidden()
    api = ready.api().login(ready.accounts, "nas")
    r = api.admin_login()
    assert r.status == 403 and r.body["code"] == "admin_only", r.body


@pytest.mark.case("ADM-02")
def test_moderate_any_message(ready, two):
    anna, nas = two
    send(nas, "nas message one")
    send(nas, "nas message two")
    ready.api("127.0.0.2").post("/api/login", {"username": "nas", "code": wrong_code(ready.secret)})  # a system message
    failed = SYSTEM_MESSAGES + ":has-text('Failed sign-in')"
    expect(anna.locator(failed)).to_have_count(1)
    expect(tool(message(anna, "nas message one"), "Edit")).to_have_count(0)

    unlock_admin(anna)
    expect(anna.locator("#admin-badge")).to_be_visible()

    one = message_by_id(anna, message(anna, "nas message one").get_attribute("data-id"))
    tool(one, "Edit").click()
    one.locator(".edit-box textarea").fill("[redacted by admin]")
    one.locator(".edit-box button", has_text="Save").click()
    expect(message(nas, "[redacted by admin]")).to_be_visible(timeout=3000)

    tool(message(anna, "nas message two"), "Delete").click()
    anna.locator("#dlg-delete button[value=delete]").click()
    expect(message(nas, "nas message two")).to_have_count(0, timeout=3000)
    ready.server.wait_for(r'"admin" edited message #\d+ as admin')
    ready.server.wait_for(r'"admin" deleted message #\d+ as admin')

    alert = anna.locator(failed).first
    expect(tool(alert, "Edit")).to_have_count(0)
    tool(alert, "Delete").click()
    anna.locator("#dlg-delete button[value=delete]").click()
    expect(nas.locator(failed)).to_have_count(0, timeout=3000)


def open_tools(page):
    page.click("#btn-admin")
    expect(page.locator("#dlg-admin-tools")).to_be_visible()


@pytest.mark.case("ADM-05")
def test_prepare_a_new_code_then_keep_the_current_one(ready, two):
    anna, nas = two
    unlock_admin(anna)
    expect(anna.locator("#admin-badge")).to_be_visible()

    open_tools(anna)
    anna.click("#btn-rotate")
    step2 = anna.locator("#rotate-step-2")
    expect(step2).to_be_visible()
    expect(step2.locator("h2")).to_have_text("Your new admin code")
    assert anna.get_attribute("#rotate-qr-img", "src").startswith("data:image/png;base64,")
    step2.locator("summary").click()
    assert len(anna.inner_text("#rotate-secret").strip()) >= 32
    expect(step2.locator(".note")).to_contain_text("you are signed out")
    # The dialog fits in the window, or scrolls.
    assert anna.locator("#dlg-admin-tools").evaluate(
        "d => d.getBoundingClientRect().bottom <= window.innerHeight + 1 || d.scrollHeight > d.clientHeight")

    anna.click("#btn-rotate-cancel")
    expect(anna.locator("#dlg-admin-tools")).to_be_hidden()
    expect(toast(anna)).to_have_text("Your current code is still valid.")

    open_tools(anna)
    anna.click("#btn-rotate")
    expect(step2).to_be_visible()
    anna.keyboard.press("Escape")
    expect(anna.locator("#dlg-admin-tools")).to_be_hidden()

    # Nobody was signed out, and the current code still works.
    anna.wait_for_timeout(1000)
    expect_screen(nas, "chat")
    expect_screen(anna, "chat")
    ApiClient(ready.url, source="127.0.0.2").login(ready.secret, "admin")


@pytest.mark.case("ADM-06")
def test_replace_the_admin_code(ready, two, client):
    anna, nas = two
    phone = sign_in(client(viewport={"width": 390, "height": 760}), ready.url, ready.accounts, "phone")
    other_admin = sign_in(client(), ready.url, ready.accounts, "admin")  # the admin's second browser
    ready.accounts.logout_admin()  # the test helper's own admin session would be counted too
    unlock_admin(anna)
    expect(anna.locator("#admin-badge")).to_be_visible()

    open_tools(anna)
    anna.click("#btn-rotate")
    anna.locator("#rotate-step-2 summary").click()
    new_secret = anna.inner_text("#rotate-secret").strip()
    assert new_secret != ready.secret
    anna.click("#btn-rotate-confirm")

    # Only the admin's sessions end; everyone else keeps working.
    expect_screen(anna, "login")
    expect(form_error(anna, "#form-login")).to_have_text(
        "Your admin code was replaced. Sign in with the new code from your authenticator app.")
    expect_screen(other_admin, "login", timeout=3000)
    ready.server.wait_for(r"replaced the admin account's secret; 2 admin session\(s\) signed out")
    for page in (nas, phone):
        expect_screen(page, "chat")
        expect(page.locator(SYSTEM_MESSAGES, has_text="The admin replaced their own authenticator code.")).to_be_visible()

    old, new = totp_code(ready.secret), totp_code(new_secret)
    if old != new:  # one chance in a million to coincide
        r = ApiClient(ready.url, source="127.0.0.2").try_login("admin", old)
        assert r.status == 401, r.body

    expect(anna.locator("#login-username")).to_have_value("admin")
    anna.fill("#login-code", fresh_code(new_secret))
    anna.click("#form-login button[type=submit]")
    expect_screen(anna, "chat")
    expect(anna.locator("#admin-badge")).to_be_hidden()

    # The new secret is saved: it still works after a restart.
    ready.server.restart()
    ApiClient(ready.server.base_url).login(new_secret, "admin")
    assert re.search(r"unlocked", ready.server.output)


@pytest.mark.case("ADM-03")
def test_lock_admin(ready, two):
    anna, nas = two
    send(nas, "written by nas")
    unlock_admin(anna)
    expect(anna.locator("#admin-badge")).to_be_visible()
    expect(tool(message(anna, "written by nas"), "Edit")).to_be_visible()

    open_tools(anna)
    anna.click("#btn-admin-lock")
    expect(toast(anna)).to_have_text("Admin locked.")
    expect(anna.locator("#admin-badge")).to_be_hidden()
    expect(anna.locator("#btn-admin")).to_have_text("Admin")
    expect(tool(message(anna, "written by nas"), "Edit")).to_have_count(0)
    expect(tool(message(anna, "written by nas"), "Delete")).to_have_count(0)


@pytest.mark.case("ADM-04")
def test_admin_rights_end_with_sign_out(ready, two):
    anna, nas = two
    unlock_admin(anna)
    expect(anna.locator("#admin-badge")).to_be_visible()
    anna.click("#btn-logout")
    expect_screen(anna, "login")

    anna.fill("#login-username", "admin")
    anna.fill("#login-code", fresh_code(ready.secret))
    anna.click("#form-login button[type=submit]")
    expect_screen(anna, "chat")
    expect(anna.locator("#admin-badge")).to_be_hidden()
    expect(anna.locator("#btn-admin")).to_have_text("Admin")

    api = ready.api().login(ready.accounts, "admin")  # a new admin session has no rights yet
    assert api.post("/api/admin/rotate").status == 403
    assert api.get("/api/admin/users").status == 403


@pytest.mark.case("ADM-07")
def test_rotation_in_polling_mode(make_ready, client):
    r = make_ready("--mode", "polling")
    anna = sign_in(client(), r.url, r.accounts, "admin")
    other_admin = sign_in(client(), r.url, r.accounts, "admin")
    nas = sign_in(client(), r.url, r.accounts, "nas")
    expect(nas.locator("#status-text")).to_have_text("Updates every 3 s")
    unlock_admin(anna)
    expect(anna.locator("#admin-badge")).to_be_visible()
    open_tools(anna)
    anna.click("#btn-rotate")
    expect(anna.locator("#rotate-step-2")).to_be_visible()
    anna.click("#btn-rotate-confirm")
    expect_screen(anna, "login")
    expect_screen(other_admin, "login", timeout=5000)  # at the next poll
    # Polling has no "signed out" event (SSE has): the next poll finds the
    # session gone, so the generic message is shown.
    expect(form_error(other_admin, "#form-login")).to_have_text(
        re.compile(r"You were signed out|Your session has ended\. Sign in again\."))
    nas.wait_for_timeout(4000)
    expect_screen(nas, "chat")

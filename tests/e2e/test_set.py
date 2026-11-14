"""SET: first-time setup."""

import io
import time
from urllib.parse import parse_qs, unquote, urlparse

import pyotp
import pytest
import zxingcpp
from PIL import Image
from playwright.sync_api import expect

from harness import ApiClient, totp_code
from ui import expect_screen, form_error, toast


@pytest.mark.case("SET-01")
def test_every_visitor_sees_the_pin_screen_before_setup(server, client):
    for page in (client(), client()):
        page.goto(server.base_url + "/")
        expect_screen(page, "setup-pin")
        expect(page.locator("#screen-setup-pin h1")).to_have_text("Set up cbshare")
        page.reload()
        expect_screen(page, "setup-pin")
        for other in ("login", "chat"):
            expect(page.locator(f"#screen-{other}")).to_be_hidden()


@pytest.mark.case("SET-02")
def test_a_wrong_pin_is_refused_and_reported(server, client):
    page = client()
    page.goto(server.base_url + "/")
    page.fill("#setup-pin", "XXXXXX")
    page.click("#form-setup-pin button[type=submit]")
    expect(form_error(page, "#form-setup-pin")).to_have_text(
        "That PIN is not correct. Check the server console. 4 attempts left before a lockout.")
    server.wait_for(r"\[auth\] failed setup PIN from 127\.0\.0\.1")

    # Once setup is done, the chat shows the failed attempt.
    secret = ApiClient(server.base_url).setup(server.pin)
    msgs = ApiClient(server.base_url).login(secret, "admin").messages()
    assert any(m["kind"] == "system" and m["body"] == "Failed sign-in attempt from 127.0.0.1: wrong setup PIN."
               for m in msgs), msgs


@pytest.mark.case("SET-04")
def test_correct_pin_qr_code_and_confirmation(server, client):
    page = client()
    page.goto(server.base_url + "/")
    page.fill("#setup-pin", server.pin)
    page.click("#form-setup-pin button[type=submit]")

    expect_screen(page, "setup-qr")
    expect(page.locator("#screen-setup-qr h1")).to_have_text("Add your admin code to an authenticator")
    expect(page.locator("#screen-setup-qr p:not(.wordmark)").first).to_contain_text("don't share it")
    assert page.get_attribute("#setup-qr-img", "src").startswith("data:image/png;base64,")
    expect(page.locator("#setup-secret")).to_be_hidden()
    page.click("#screen-setup-qr summary")
    secret = page.inner_text("#setup-secret").strip()
    assert len(secret) >= 32
    expect(page.locator("#screen-setup-qr .note")).to_contain_text("only shown now")

    page.click("#btn-setup-confirm")
    expect(toast(page)).to_have_text("Setup complete. Sign in as admin with the code from your authenticator app.")
    expect_screen(page, "login")
    expect(page.locator("#login-username")).to_have_value("admin")
    expect(page.locator("#setup-secret")).to_have_text("")  # the secret left the page
    server.wait_for(r"\[auth\] setup complete: admin account created")

    other = client()
    other.goto(server.base_url + "/")
    expect_screen(other, "login")
    r = ApiClient(server.base_url).post("/api/setup/pin", {"pin": server.pin})
    assert r.status == 409 and r.body["error"].startswith("Setup is already complete")

    # The secret shown is the admin account's.
    ApiClient(server.base_url).login(secret, "admin")


@pytest.mark.case("SET-05")
def test_the_qr_code_is_the_admin_account(server, client):
    page = client()
    page.goto(server.base_url + "/")
    page.fill("#setup-pin", server.pin)
    page.click("#form-setup-pin button[type=submit]")
    expect_screen(page, "setup-qr")

    # "Photograph" the QR code and decode it, like an authenticator app.
    shot = page.locator("#setup-qr-img").screenshot()
    codes = zxingcpp.read_barcodes(Image.open(io.BytesIO(shot)))
    assert codes, "the QR code could not be decoded"
    uri = urlparse(codes[0].text)
    assert uri.scheme == "otpauth" and uri.netloc == "totp"
    host = server.base_url.removeprefix("https://")
    assert unquote(uri.path) == f"/cbshare:admin@{host}", "the entry is labelled with the account and server"
    params = parse_qs(uri.query)
    assert params["issuer"] == ["cbshare"]
    secret = params["secret"][0]

    page.click("#screen-setup-qr summary")
    assert page.inner_text("#setup-secret").strip() == secret, "manual key and QR code differ"
    page.click("#btn-setup-confirm")
    expect_screen(page, "login")

    # An app that scanned the code signs in as admin, and only as admin.
    app1, app2 = pyotp.TOTP(secret), pyotp.parse_uri(codes[0].text)
    assert app1.now() == app2.now()
    ApiClient(server.base_url).login(secret, "admin")
    r = ApiClient(server.base_url).try_login("anna", app1.now())
    assert r.status == 401


@pytest.mark.case("SET-03")
def test_the_pin_is_not_case_sensitive(server, client):
    page = client()
    page.goto(server.base_url + "/")
    expect_screen(page, "setup-pin")
    page.fill("#setup-pin", f" {server.pin.lower()} ")
    assert page.locator("#setup-pin").evaluate("e => getComputedStyle(e).textTransform") == "uppercase"
    page.click("#form-setup-pin button[type=submit]")
    expect_screen(page, "setup-qr")


@pytest.mark.case("SET-06")
def test_only_the_browser_that_entered_the_pin_can_confirm(server, client):
    pages = []
    for _ in range(2):
        page = client()
        page.goto(server.base_url + "/")
        page.fill("#setup-pin", server.pin)
        page.click("#form-setup-pin button[type=submit]")
        expect_screen(page, "setup-qr")
        page.click("#screen-setup-qr summary")
        pages.append((page, page.inner_text("#setup-secret").strip()))
    (first, secret_a), (second, secret_b) = pages
    assert secret_a != secret_b

    first.click("#btn-setup-confirm")  # replaced by the second enrollment
    expect_screen(first, "setup-pin")
    expect(form_error(first, "#form-setup-pin")).to_have_text("This setup session has expired. Enter the PIN again.")

    second.click("#btn-setup-confirm")
    expect_screen(second, "login")
    ApiClient(server.base_url).login(secret_b, "admin")
    if totp_code(secret_a) != totp_code(secret_b):
        assert ApiClient(server.base_url).try_login("admin", totp_code(secret_a)).status == 401


@pytest.mark.slow
@pytest.mark.timeout(20 * 60)
@pytest.mark.case("SET-07")
def test_a_setup_left_open_expires(server):
    api = ApiClient(server.base_url)
    first = api.expect(api.post("/api/setup/pin", {"pin": server.pin}), 200).body["secret"]
    time.sleep(15 * 60 + 5)  # the QR code stays valid for 15 minutes
    r = api.post("/api/setup/confirm")
    assert r.status == 403 and r.body["error"] == "This setup session has expired. Enter the PIN again."
    again = api.expect(api.post("/api/setup/pin", {"pin": server.pin}), 200).body["secret"]
    assert again != first


@pytest.mark.case("SET-08")
def test_restart_before_setup_is_completed(server):
    api = ApiClient(server.base_url)
    api.expect(api.post("/api/setup/pin", {"pin": server.pin}), 200)  # QR shown, not confirmed
    old_pin = server.pin

    server.restart(same_port=True)
    assert server.pin and server.pin != old_pin, "a new setup PIN is printed"
    r = api.post("/api/setup/confirm")  # the QR code from before the restart
    assert r.status == 403
    r = ApiClient(server.base_url).post("/api/setup/pin", {"pin": old_pin})
    assert r.status == 401
    ApiClient(server.base_url).setup(server.pin)

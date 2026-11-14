"""Regenerates the documentation screenshots in docs/images.

Starts real cbshare servers, fills them with the sample data shown in the
docs (users Anna and nas, a deploy script, a Wi-Fi password, nginx.conf) and
captures every screen with Playwright, like a person using the app.

Usage (from tests/e2e, with the e2e requirements and Chromium installed):

    python tools/screenshots.py --bin /path/to/cbshare --out ../../docs/images

The cbshare binary does not need the testhooks build tag. The images look
best with the Inter and JetBrains Mono fonts installed (the screenshots
workflow installs them).
"""

from __future__ import annotations

import argparse
import sys
import tempfile
import time
from pathlib import Path

from playwright.sync_api import Browser, Page, expect, sync_playwright

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from harness import ADMIN_PASSWORD, Accounts, ApiClient, CbshareServer, wrong_code  # noqa: E402
import ui  # noqa: E402

DESKTOP = {"viewport": {"width": 1100, "height": 760}, "device_scale_factor": 1}
MOBILE = {"viewport": {"width": 390, "height": 760}, "device_scale_factor": 2,
          "is_mobile": True, "has_touch": True}
COMMON = {"ignore_https_errors": True, "locale": "en-US", "timezone_id": "Europe/Paris"}

SCRIPT_BEFORE = """#!/bin/sh
set -e
rsync -avz ./build/ deploy@192.168.1.40:/srv/app/
ssh deploy@192.168.1.40 'systemctl restart app'
echo done"""
SCRIPT = SCRIPT_BEFORE.replace("-avz ./build/", "-avz --delete ./build/")
WIFI = "wifi guest password: Tr0ub4dor&3-horse"
NGINX = b"server {\n  listen 80;\n  proxy_pass http://app;\n}\n"  # 49 bytes


def settle(page: Page, ms: int = 700) -> None:
    """Lets animations (new messages, dialogs) finish before a capture."""
    page.wait_for_timeout(ms)


def shot(page: Page, out: Path, name: str) -> None:
    settle(page)
    page.screenshot(path=str(out / f"{name}.png"))
    print(f"  {name}.png")


def new_page(browser: Browser, **opts) -> Page:
    return browser.new_context(**COMMON, **(opts or DESKTOP)).new_page()


def setup_screens(browser: Browser, binary: str, work: Path, out: Path) -> None:
    """setup-pin and setup-qr, on a server of their own (a wrong PIN posts
    an alert in the chat, which the chat screenshots must not show)."""
    srv = CbshareServer(binary, work / "setup").start(password=ADMIN_PASSWORD)
    try:
        page = new_page(browser)
        page.goto(srv.base_url + "/")
        ui.expect_screen(page, "setup-pin")
        page.fill("#setup-pin", "XXXXXX")
        page.click("#form-setup-pin button[type=submit]")
        expect(page.locator("#form-setup-pin .error")).to_contain_text("attempts left")
        page.locator("#setup-pin").select_text()
        shot(page, out, "setup-pin")

        page.fill("#setup-pin", srv.pin)
        page.click("#form-setup-pin button[type=submit]")
        ui.expect_screen(page, "setup-qr")
        expect(page.locator("#setup-qr-img")).to_be_visible()
        shot(page, out, "setup-qr")
        page.context.close()
    finally:
        srv.stop()


def main_screens(browser: Browser, binary: str, work: Path, out: Path) -> None:
    srv = CbshareServer(binary, work / "main").start(password=ADMIN_PASSWORD)
    try:
        base = srv.base_url
        accounts = Accounts(base, ApiClient(base).setup(srv.pin))
        admin = accounts.admin()

        # Users, then a clean chat: only the failed sign-in alert remains.
        anna = ApiClient(base).login(accounts, "Anna")
        nas = ApiClient(base).login(accounts, "nas")
        for m in admin.messages():
            admin.expect(admin.delete(f"/api/messages/{m['id']}"), 200)
        ApiClient(base).try_login("Anna", wrong_code(accounts.secret("Anna")))

        script = anna.post_text(SCRIPT_BEFORE)
        nas.post_text(WIFI)
        anna.expect(anna.upload("nginx.conf", NGINX, caption="Updated proxy config"), 201)

        # chat: Anna edits her script (toast "Message updated.").
        print("chat")
        page = ui.sign_in(new_page(browser), base, accounts, "Anna")
        msg = ui.message_by_id(page, str(script["id"]))
        ui.tool(msg, "Edit").click()
        msg.locator("textarea").fill(SCRIPT)
        msg.get_by_role("button", name="Save").click()
        expect(ui.toast(page)).to_contain_text("Message updated")
        expect(msg).to_contain_text("(edited)")
        shot(page, out, "chat")

        dark = ui.sign_in(new_page(browser, color_scheme="dark", **DESKTOP), base, accounts, "Anna")
        dark.focus("#compose")
        shot(dark, out, "chat-dark")
        dark.context.close()

        mobile = ui.sign_in(new_page(browser, **MOBILE), base, accounts, "nas")
        mobile.focus("#compose")
        shot(mobile, out, "chat-mobile")
        mobile.context.close()

        # sign-in: the form filled in, not sent.
        login = new_page(browser)
        login.goto(base + "/")
        ui.expect_screen(login, "login")
        login.fill("#login-username", "Anna")
        login.fill("#login-code", "774563")
        login.focus("#login-code")
        shot(login, out, "sign-in")
        login.context.close()

        # signed-out: Anna's session ends while her tab is open.
        token = next(c["value"] for c in page.context.cookies() if c["name"] == "cbshare_session")
        ender = ApiClient(base)
        ender.s.cookies.set("cbshare_session", token, domain=base.split("//")[1].split(":")[0])
        ender.expect(ender.post("/api/logout"), 200)
        ui.expect_screen(page, "login")
        expect(ui.form_error(page, "#form-login")).to_contain_text("signed out")
        shot(page, out, "signed-out")
        page.context.close()

        # Admin screens.
        print("admin")
        accounts.logout_admin()
        time.sleep(1)
        adm = ui.sign_in(new_page(browser), base, accounts, "admin")
        adm.click("#btn-admin")
        adm.fill("#admin-password", ADMIN_PASSWORD)
        shot(adm, out, "admin-unlock")
        adm.click("#form-admin button[type=submit]")
        expect(adm.locator("#admin-badge")).to_be_visible()

        adm.click("#btn-admin")
        adm.click("#btn-users")
        expect(adm.locator("#user-list li")).to_have_count(3)
        adm.fill("#new-username", "build server")
        adm.click("#btn-add-user")
        expect(adm.locator("#dlg-user-qr")).to_be_visible()
        expect(adm.locator("#user-qr-img")).to_be_visible()
        shot(adm, out, "admin-new-user-qr")
        adm.locator("#dlg-user-qr [data-close]").click()

        helper = accounts.admin()
        helper.expect(helper.create_user("old laptop"), 201)
        helper.expect(helper.post(f"/api/admin/users/{helper.user_id('old laptop')}/revoke"), 200)
        adm.locator("#dlg-users [data-close]").click()
        adm.click("#btn-admin")
        adm.click("#btn-users")
        expect(adm.locator("#user-list li")).to_have_count(5)
        shot(adm, out, "admin-users")
        adm.locator("#dlg-users [data-close]").click()

        adm.click("#btn-admin")
        adm.click("#btn-rotate")
        expect(adm.locator("#rotate-qr-img")).to_be_visible()
        shot(adm, out, "admin-rotate")
        adm.click("#btn-rotate-cancel")
        adm.context.close()
    finally:
        srv.stop()


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--bin", required=True, help="cbshare executable")
    ap.add_argument("--out", required=True, help="folder for the PNG files (docs/images)")
    args = ap.parse_args()
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory() as tmp, sync_playwright() as p:
        browser = p.chromium.launch()
        print("setup")
        setup_screens(browser, args.bin, Path(tmp), out)
        main_screens(browser, args.bin, Path(tmp), out)
        browser.close()
    print("done")


if __name__ == "__main__":
    main()

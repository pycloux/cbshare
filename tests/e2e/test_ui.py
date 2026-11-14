"""UI: interface checks in each supported browser engine."""

import io
import os

import pytest
from PIL import Image
from playwright.sync_api import expect

from harness import fresh_code, sha256
from ui import allow_clipboard, expect_screen, form_error, message, send, sign_in


@pytest.mark.case("UI-01")
def test_main_flow_in_this_browser(ready, client, scratch):
    """The core of AUTH-01, CHAT-01, CHAT-03, FILE-01, FILE-04 and LIVE-01
    in the browser engine under test (Chromium, Firefox or WebKit)."""
    anna = sign_in(client(), ready.url, ready.accounts, "Anna laptop")
    nas = sign_in(client(), ready.url, ready.accounts, "nas")
    expect(anna.locator("#status-text")).to_have_text("Live")

    text = "multi-line\n\tsnippet  "
    send(anna, text)
    msg = message(nas, "snippet")
    expect(msg).to_be_visible(timeout=3000)

    allow_clipboard(nas, ready.url)
    copy = msg.locator("button.tool").first
    copy.click()
    expect(copy).to_have_text("Copied")
    assert nas.evaluate("navigator.clipboard.readText()") == text

    data = os.urandom(200_000)
    (scratch / "blob.bin").write_bytes(data)
    anna.set_input_files("#file-input", str(scratch / "blob.bin"))
    link = nas.locator("#feed-inner .file a", has_text="Download")
    expect(link).to_be_visible(timeout=5000)
    with nas.expect_download() as dl:
        link.click()
    dl.value.save_as(scratch / "got.bin")
    assert sha256((scratch / "got.bin").read_bytes()) == sha256(data)


NO_SIDE_SCROLL = "document.documentElement.scrollWidth <= document.documentElement.clientWidth"


@pytest.mark.case("UI-02")
def test_phone_layout(ready, client):
    page = client(viewport={"width": 390, "height": 844}, has_touch=True)
    sign_in(page, ready.url, ready.accounts, "admin")  # the admin account, which has the most buttons
    send(page, "a fairly long message that has to wrap on a narrow phone screen " * 3)
    assert page.evaluate(NO_SIDE_SCROLL)
    expect(page.locator("#status")).to_be_visible()  # the dot stays
    expect(page.locator("#status-text")).to_be_hidden()  # the label goes
    for sel in ("#compose", "#btn-send", "#btn-attach", "#btn-admin", "#btn-logout"):
        box = page.locator(sel).bounding_box()
        assert box and box["x"] >= 0 and box["x"] + box["width"] <= 390, sel

    page.click("#btn-admin")
    dialog = page.locator("#dlg-admin")
    expect(dialog).to_be_visible()
    box = dialog.bounding_box()
    assert box["x"] >= 0 and box["x"] + box["width"] <= 390 and box["y"] + box["height"] <= 844
    page.keyboard.press("Escape")

    page.set_viewport_size({"width": 844, "height": 390})  # landscape
    expect(page.locator("#compose")).to_be_in_viewport()
    assert page.evaluate(NO_SIDE_SCROLL)


CONTRAST = """el => {
    const rgb = c => (c.match(/[\\d.]+/g) || []).map(Number);
    const lum = ([r, g, b]) => {
        const f = v => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; };
        return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b);
    };
    let bg = el, c;
    while (bg) {
        c = rgb(getComputedStyle(bg).backgroundColor);
        if (c.length >= 3 && (c.length < 4 || c[3] > 0.5)) break;
        bg = bg.parentElement;
    }
    const back = bg ? lum(c) : 1, fore = lum(rgb(getComputedStyle(el).color));
    return { ratio: (Math.max(back, fore) + 0.05) / (Math.min(back, fore) + 0.05), background: back };
}"""


@pytest.mark.case("UI-03")
@pytest.mark.parametrize("scheme", ["light", "dark"])
def test_light_and_dark_mode(server, client, scheme, browser_name, browser_type, browser_type_launch_args):
    if browser_name == "firefox":
        # Playwright's colour-scheme emulation has no effect in its Firefox
        # (matchMedia still reports light), so Firefox is started with its
        # own preference instead, as a user's system setting would.
        browser = browser_type.launch(**browser_type_launch_args, firefox_user_prefs={
            "layout.css.prefers-color-scheme.content-override": 0 if scheme == "dark" else 1})
        page = browser.new_context(ignore_https_errors=True, bypass_csp=True).new_page()
    else:
        browser = None
        page = client(color_scheme=scheme)
    try:
        check_colour_scheme(page, server, scheme)
    finally:
        if browser:
            browser.close()


def check_colour_scheme(page, server, scheme):
    page.goto(server.base_url + "/")
    page.fill("#setup-pin", server.pin)
    page.click("#form-setup-pin button[type=submit]")
    expect(page.locator("#setup-qr-img")).to_be_visible()
    shot = Image.open(io.BytesIO(page.locator("#setup-qr-img").screenshot())).convert("L")
    assert shot.getpixel((5, shot.height // 2)) > 240, "the QR code keeps a white background"
    page.click("#btn-setup-confirm")
    expect(page.locator("#screen-login")).to_be_visible()

    body = page.locator("body").evaluate(CONTRAST)
    dark = page.evaluate("matchMedia('(prefers-color-scheme: dark)').matches")
    assert dark == (scheme == "dark"), f"the browser reports prefers-color-scheme dark={dark}"
    assert (body["background"] < 0.2) == (scheme == "dark"), body
    for sel in ("#screen-login h1, #screen-login h2", "#login-code", "#form-login button[type=submit]"):
        res = page.locator(sel).first.evaluate(CONTRAST)
        assert res["ratio"] >= 4.5, (sel, res)


@pytest.mark.case("UI-04")
def test_keyboard_only(ready, client):
    page = client()
    page.goto(ready.url + "/")
    expect(page.locator("#login-username")).to_be_focused()  # ready to type
    page.keyboard.type("admin")
    page.keyboard.press("Tab")
    expect(page.locator("#login-code")).to_be_focused()
    page.keyboard.type(fresh_code(ready.secret))
    page.keyboard.press("Enter")
    expect(page.locator("#compose")).to_be_focused()

    page.keyboard.type("sent without a mouse")
    page.keyboard.press("Control+Enter")
    expect(message(page, "sent without a mouse")).to_be_visible()

    # Tab reaches the message's buttons, with a visible focus outline.
    page.locator("#btn-logout").focus()  # the last button of the top bar; the feed comes next
    seen = []
    for _ in range(15):
        page.keyboard.press("Tab")
        label = page.evaluate("document.activeElement.textContent.trim()")
        seen.append(label)
        if label == "Copy":
            break
    assert "Copy" in seen, seen
    outline = page.evaluate("getComputedStyle(document.activeElement).outlineStyle")
    assert outline not in ("none", ""), "the focused element must be visible"

    page.locator("#btn-admin").focus()
    page.keyboard.press("Enter")
    expect(page.locator("#dlg-admin")).to_be_visible()
    expect(page.locator("#admin-password")).to_be_focused()
    page.keyboard.press("Escape")
    expect(page.locator("#dlg-admin")).to_be_hidden()


@pytest.mark.case("UI-06")
def test_zoom_and_reduced_motion(ready, client):
    page = client(reduced_motion="reduce")
    sign_in(page, ready.url, ready.accounts, "calm")
    send(page, "no animation please")
    msg = message(page, "no animation please")
    assert msg.evaluate("e => getComputedStyle(e).animationName") == "none"

    moving = sign_in(client(), ready.url, ready.accounts, "lively")
    send(moving, "animated")
    assert message(moving, "animated").evaluate("e => getComputedStyle(e).animationName") != "none"

    page.set_viewport_size({"width": 550, "height": 380})  # 1100x760 at 200% zoom
    expect(page.locator("#compose")).to_be_in_viewport()
    expect(page.locator("#btn-send")).to_be_in_viewport()
    assert page.evaluate(NO_SIDE_SCROLL)


@pytest.mark.case("UI-07")
def test_server_unreachable(ready, client):
    page = client()
    page.goto(ready.url + "/")
    expect_screen(page, "login")
    srv = ready.server
    srv.stop()
    page.fill("#login-username", "admin")
    page.fill("#login-code", fresh_code(ready.secret))
    page.click("#form-login button[type=submit]")
    expect(form_error(page, "#form-login")).to_have_text("The server cannot be reached.")
    expect_screen(page, "login")

    srv.start(key=srv.key, port=srv.port)
    page.fill("#login-code", fresh_code(ready.secret))
    page.click("#form-login button[type=submit]")
    expect_screen(page, "chat")

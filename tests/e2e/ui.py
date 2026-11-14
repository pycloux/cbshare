"""Browser helpers: the cbshare screens, driven like a person would."""

from __future__ import annotations

from playwright.sync_api import Page, expect

from harness import Accounts, fresh_code

# Messages in the chat feed (all, own, system, and from others).
MESSAGES = "#feed-inner article.msg"
USER_MESSAGES = "#feed-inner article.msg:not(.system)"
SYSTEM_MESSAGES = "#feed-inner article.msg.system"


def screen(page: Page, name: str):
    """The visible screen: setup-pin, setup-qr, login or chat."""
    return page.locator(f"#screen-{name}")


def expect_screen(page: Page, name: str, timeout: float = 10_000) -> None:
    expect(screen(page, name)).to_be_visible(timeout=timeout)


def sign_in(page: Page, base_url: str, secret: "str | Accounts", name: str) -> Page:
    """Opens cbshare and signs in as name with a current code. secret is the
    user's TOTP secret, or an Accounts that provides (or creates) the user."""
    if isinstance(secret, Accounts):
        secret = secret.secret(name)
    page.goto(base_url + "/")
    expect_screen(page, "login")
    page.fill("#login-username", name)
    page.fill("#login-code", fresh_code(secret))
    page.click("#form-login button[type=submit]")
    expect_screen(page, "chat")
    expect(page.locator("#me-name")).to_have_text(name)
    return page


def send(page: Page, text: str) -> None:
    """Types text in the message box and sends it with the Send button."""
    page.fill("#compose", text)
    page.click("#btn-send")
    expect(page.locator("#compose")).to_have_value("")


def message(page: Page, text: str):
    """The message (article) containing text."""
    return page.locator(MESSAGES, has_text=text)


def message_by_id(page: Page, msg_id: str):
    """A message by its id: stable even while it is being edited (the text
    then sits in a text box, so has_text no longer matches)."""
    return page.locator(f'#feed-inner article.msg[data-id="{msg_id}"]')


def body_text(locator) -> str:
    """The exact text of a message body, whitespace included."""
    return locator.locator("pre.msg-body").evaluate("e => e.textContent")


def tool(msg, label: str):
    """A message's Copy, Edit or Delete button."""
    return msg.locator("button.tool", has_text=label)


def toast(page: Page):
    return page.locator("#toast")


def form_error(page: Page, form: str):
    return page.locator(f"{form} .error")


# Firefox and WebKit give no way to grant clipboard access in Playwright, so
# there the page's clipboard is replaced by an in-memory one that records
# exactly what the Copy button writes.
CLIPBOARD_STUB = """(() => {
    let stored = "";
    const clip = { writeText: async t => { stored = String(t); }, readText: async () => stored };
    Object.defineProperty(navigator, "clipboard", { configurable: true, get: () => clip });
})()"""


def allow_clipboard(page: Page, origin: str) -> None:
    """Lets page use the clipboard; reload before use if it is not Chromium."""
    if page.context.browser.browser_type.name == "chromium":
        page.context.grant_permissions(["clipboard-read", "clipboard-write"], origin=origin)
    else:
        page.context.add_init_script(CLIPBOARD_STUB)
        page.reload()

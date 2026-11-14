"""CHAT: text messages."""

import datetime
import re

import pytest
from playwright.sync_api import expect

from harness import fnv_cable, totp_code
from ui import MESSAGES, allow_clipboard, SYSTEM_MESSAGES, USER_MESSAGES, body_text, message, send, sign_in, toast, tool

SCRIPT = (
    "#!/bin/sh\n"
    "set -e   \n"            # trailing spaces
    "\n"                     # blank line
    "for host in nas pi; do\n"
    "\trsync -avz ./build/ \"deploy@$host:/srv/app/\"\t\n"  # tabs
    "done\n"
    "\n"
    "echo 'done'  "
)


@pytest.fixture
def two(ready, client):
    """Anna's laptop and the NAS, both signed in."""
    anna = sign_in(client(), ready.url, ready.accounts, "Anna laptop")
    nas = sign_in(client(), ready.url, ready.accounts, "nas")
    return anna, nas


@pytest.mark.case("CHAT-01")
def test_post_a_message(ready, two):
    anna, nas = two
    send(anna, "hello from Anna")

    mine = message(anna, "hello from Anna")
    expect(mine).to_have_class(re.compile(r"\bown\b"))
    expect(mine.locator(".msg-author")).to_have_text("Anna laptop")
    expect(mine.locator(".msg-time")).to_have_text(re.compile(r"\d"))  # a time is shown
    assert mine.evaluate("e => getComputedStyle(e).alignSelf") == "flex-end"  # on the right

    theirs = message(nas, "hello from Anna")
    expect(theirs).to_be_visible(timeout=3000)  # live, without reloading
    expect(theirs).not_to_have_class(re.compile(r"\bown\b"))
    expect(theirs.locator(".msg-author")).to_have_text("Anna laptop")
    stripe = "e => getComputedStyle(e).borderLeftColor"
    assert mine.evaluate(stripe) == theirs.evaluate(stripe), "the author's colour must be the same for everyone"

    line = ready.server.wait_for(r'\[chat\] "Anna laptop" posted message #\d+').group(0)
    assert line
    assert "hello from Anna" not in ready.server.output, "message text must not be logged"


@pytest.mark.case("CHAT-02")
def test_scripts_keep_their_exact_formatting(two):
    anna, nas = two
    anna.fill("#compose", SCRIPT)
    anna.focus("#compose")
    anna.keyboard.press("End")
    anna.keyboard.press("Enter")  # Enter adds a line...
    expect(anna.locator("#compose")).to_have_value(SCRIPT + "\n")
    expect(anna.locator(USER_MESSAGES)).to_have_count(0)  # ...and sends nothing
    anna.keyboard.press("Backspace")
    anna.keyboard.press("Control+Enter")

    for page in (anna, nas):
        msg = message(page, "rsync")
        expect(msg).to_be_visible()
        assert body_text(msg) == SCRIPT, "the text must be kept exactly"
        pre = msg.locator("pre.msg-body")
        assert pre.evaluate("e => getComputedStyle(e).whiteSpace") == "pre-wrap"
        assert "mono" in pre.evaluate("e => getComputedStyle(e).fontFamily").lower()
    expect(anna.locator("#compose")).to_have_value("")

    # A very long line wraps instead of making the page scroll sideways.
    send(anna, "x" * 400)
    assert anna.evaluate("document.documentElement.scrollWidth <= document.documentElement.clientWidth")


@pytest.mark.case("CHAT-03")
def test_copy_gives_back_the_exact_text(ready, two):
    anna, nas = two
    allow_clipboard(nas, ready.url)
    send(anna, SCRIPT)

    msg = message(nas, "rsync")
    expect(tool(msg, "Copy")).to_be_visible()
    # The first tool button, not "the button labelled Copy": its label
    # changes to "Copied", which does not contain the word "Copy".
    copy = msg.locator("button.tool").first
    copy.click()
    expect(copy).to_have_text("Copied")
    expect(copy).to_have_class(re.compile(r"\bcopied\b"))
    assert nas.evaluate("navigator.clipboard.readText()") == SCRIPT
    expect(copy).to_have_text("Copy", timeout=3000)  # back to normal


@pytest.mark.case("CHAT-05")
def test_text_that_looks_like_html_is_shown_as_text(two):
    anna, nas = two
    dialogs = []
    for page in (anna, nas):
        page.on("dialog", lambda d: (dialogs.append(d.message), d.dismiss()))
    payload = "<img src=x onerror=alert(1)> <b>bold?</b> <script>alert(2)</script>"
    send(anna, payload)

    for page in (anna, nas):
        msg = page.locator(USER_MESSAGES).last
        expect(msg).to_contain_text("<b>bold?</b>")
        assert body_text(msg) == payload
        expect(page.locator("#feed-inner pre.msg-body *")).to_have_count(0)  # no element was created
    anna.wait_for_timeout(1000)
    assert dialogs == [], f"a script ran: {dialogs}"


@pytest.mark.case("CHAT-04")
def test_empty_messages_are_not_sent(ready, two):
    anna, nas = two
    for text in ("", "   ", "\n\n\t  \n"):
        anna.fill("#compose", text)
        anna.click("#btn-send")
        anna.focus("#compose")
        anna.keyboard.press("Control+Enter")
    anna.wait_for_timeout(1000)
    for page in (anna, nas):
        expect(page.locator(USER_MESSAGES)).to_have_count(0)
    api = ready.api().login(ready.accounts, "script")
    r = api.post("/api/messages", {"text": " \n "})
    assert r.status == 400 and r.body["error"] == "Write something before sending."


@pytest.mark.case("CHAT-06")
def test_international_text_and_emoji(two):
    anna, nas = two
    text = "Café, naïve, Grüße — 日本語のテキスト, العربية, emoji 🚀👍🏽 and 👨‍👩‍👧"
    send(anna, text)
    long_line = "word " * 60 + "unbroken" * 40  # 300+ characters
    send(anna, long_line)
    for page in (anna, nas):
        expect(message(page, "Grüße")).to_be_visible()
        assert body_text(message(page, "Grüße")) == text
        expect(message(page, "unbroken")).to_be_visible()
        assert page.evaluate("document.documentElement.scrollWidth <= document.documentElement.clientWidth")
        feed = page.locator("#feed")
        assert feed.evaluate("e => e.scrollWidth <= e.clientWidth"), "the feed must not scroll sideways"


@pytest.mark.case("CHAT-07")
def test_very_long_messages(ready, two):
    anna, nas = two
    big = ("0123456789abcdef" * 64 + "\n") * 290  # about 300 KB
    anna.fill("#compose", big)
    anna.click("#btn-send")
    expect(toast(anna)).to_have_text("The message is longer than 256 KB. Share it as a file instead.")
    expect(anna.locator("#compose")).to_have_value(big)  # nothing lost
    expect(nas.locator(USER_MESSAGES)).to_have_count(0)

    fits = "y" * (200 * 1024)
    send(anna, fits)
    expect(nas.locator(USER_MESSAGES)).to_have_count(1, timeout=5000)
    assert len(body_text(nas.locator(USER_MESSAGES).first)) == len(fits)


def names_with_distinct_colours(count):
    names, cables = [], set()
    for candidate in ("Anna laptop", "nas", "build server", "pi", "desktop", "phone", "tablet", "vm-1", "vm-2"):
        cable = fnv_cable(candidate)
        if cable not in cables:
            names.append(candidate)
            cables.add(cable)
        if len(names) == count:
            return names
    raise AssertionError("not enough names with distinct colours")


@pytest.mark.case("CHAT-08")
def test_authors_are_easy_to_tell_apart(ready, client):
    names = names_with_distinct_colours(3)
    for name in names:
        ready.api().login(ready.accounts, name).post_text(f"message from {name}")
    ready.api("127.0.0.3").post("/api/login", {"code": "000000" if totp_code(ready.secret) != "000000" else "111111"})

    viewers = [sign_in(client(), ready.url, ready.accounts, "viewer one"),
               sign_in(client(), ready.url, ready.accounts, "viewer two")]
    stripe = "e => getComputedStyle(e).borderLeftColor"
    colours = []
    for page in viewers:
        expect(page.locator(USER_MESSAGES)).to_have_count(3)
        seen = []
        for name in names:
            msg = message(page, f"message from {name}")
            expect(msg.locator(".msg-author")).to_have_text(name)
            seen.append(msg.evaluate(stripe))
        assert len(set(seen)) == 3, f"each author has their own colour: {seen}"
        colours.append(seen)

        system = page.locator(SYSTEM_MESSAGES).first
        expect(system).to_be_visible()
        assert system.evaluate("e => getComputedStyle(e).alignSelf") == "center"
        assert system.evaluate(stripe) not in seen

        title = page.locator(USER_MESSAGES).first.locator(".msg-time").get_attribute("title")
        assert str(datetime.date.today().year) in title, f"the full date is in the tooltip: {title}"
    assert colours[0] == colours[1], "colours are the same for everyone"

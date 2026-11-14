"""LIVE: live updates (Server-Sent Events and polling)."""

import json
import re
import threading
import time

import pytest
from playwright.sync_api import expect

from harness import CuttableProxy
from ui import USER_MESSAGES, expect_screen, message, send, sign_in

AT_BOTTOM = "e => e.scrollHeight - e.scrollTop - e.clientHeight < 80"


@pytest.mark.case("LIVE-01")
def test_live_updates_in_sse_mode(ready, client):
    anna = sign_in(client(), ready.url, ready.accounts, "Anna laptop")
    nas = sign_in(client(), ready.url, ready.accounts, "nas")
    for page in (anna, nas):
        expect(page.locator("#status-text")).to_have_text("Live")
        expect(page.locator("#status")).to_have_class(re.compile(r"\blive\b"))

    api = ready.api().login(ready.accounts, "Anna laptop")  # same name: may edit and delete
    send(anna, "posted live")
    expect(message(nas, "posted live")).to_be_visible(timeout=2000)

    msg_id = next(m["id"] for m in api.messages() if m["body"] == "posted live")
    api.expect(api.put(f"/api/messages/{msg_id}", {"text": "edited live"}), 200)
    expect(message(nas, "edited live")).to_contain_text("(edited)", timeout=2000)
    api.expect(api.delete(f"/api/messages/{msg_id}"), 200)
    expect(message(nas, "edited live")).to_have_count(0, timeout=2000)
    assert api.upload("live.txt", b"x").status == 201
    expect(nas.locator(".file-name", has_text="live.txt")).to_be_visible(timeout=2000)

    # Fill the feed so it scrolls.
    for i in range(14):
        api.post_text(f"filler {i}\n" + "line\n" * 4)
    expect(nas.locator(USER_MESSAGES)).to_have_count(15, timeout=5000)
    feed = nas.locator("#feed")

    # Reading older messages: a new one does not move the view.
    feed.evaluate("e => e.scrollTop = 0")
    api.post_text("while reading")
    expect(message(nas, "while reading")).to_have_count(1, timeout=2000)
    assert feed.evaluate("e => e.scrollTop") == 0

    # At the bottom: the view follows new messages.
    feed.evaluate("e => e.scrollTop = e.scrollHeight")
    api.post_text("follow me")
    expect(message(nas, "follow me")).to_be_in_viewport(timeout=2000)
    assert feed.evaluate(AT_BOTTOM)


@pytest.mark.case("LIVE-03")
def test_polling_mode(make_ready, client):
    r = make_ready("--mode", "polling")
    page = client()
    requests_seen = []
    page.on("request", lambda req: requests_seen.append(req.url))
    sign_in(page, r.url, r.accounts, "nas")
    expect(page.locator("#status-text")).to_have_text("Updates every 3 s")

    api = r.api().login(r.accounts, "Anna laptop")
    api.post_text("polled")
    expect(message(page, "polled")).to_be_visible(timeout=5000)

    # Quiet period: a small "unchanged" answer every 3 seconds.
    polls = []
    page.on("response", lambda resp: polls.append(resp) if "/api/messages?since=" in resp.url else None)
    time.sleep(7)
    page.wait_for_timeout(100)
    assert len(polls) >= 2, f"{len(polls)} polls in 7 s"
    for resp in polls:
        body = resp.body()
        assert json.loads(body).get("unchanged") is True
        assert len(body) < 100
    assert not any("/api/events" in u for u in requests_seen), "no event stream in polling mode"


@pytest.mark.case("LIVE-02")
def test_reconnection_after_a_network_outage(ready, client, request, browser_name):
    if browser_name == "webkit":
        request.applymarker(pytest.mark.xfail(
            strict=True, reason="WebKit never notices the closed event stream, so the page stays stale "
                                "(https://github.com/pycloux/cpsync/issues/21)"))
    proxy = CuttableProxy(ready.server.port)
    try:
        page = sign_in(client(), proxy.url, ready.accounts, "nas")
        expect(page.locator("#status-text")).to_have_text("Live")
        api = ready.api().login(ready.accounts, "Anna laptop")

        proxy.cut()
        api.post_text("sent during the outage 1")
        api.post_text("sent during the outage 2")
        expect(page.locator("#status-text")).to_have_text("Reconnecting", timeout=15_000)
        page.wait_for_timeout(2000)
        expect(message(page, "during the outage")).to_have_count(0)

        proxy.restore()
        expect(message(page, "during the outage")).to_have_count(2, timeout=30_000)
        expect(page.locator("#status-text")).to_have_text("Live", timeout=30_000)
        expect_screen(page, "chat")  # still signed in
    finally:
        proxy.close()


@pytest.mark.case("LIVE-04")
def test_polling_interval(make_ready, client):
    r = make_ready("--mode", "polling", "--poll-interval", "10s")
    page = sign_in(client(), r.url, r.accounts, "nas")
    expect(page.locator("#status-text")).to_have_text("Updates every 10 s")
    times = []
    page.on("request", lambda req: times.append(time.monotonic()) if "/api/messages?since=" in req.url else None)
    page.wait_for_timeout(32_000)  # not time.sleep: events are only handled while Playwright waits
    assert len(times) in (3, 4), f"{len(times)} polls in 32 s"
    gaps = [b - a for a, b in zip(times, times[1:])]
    assert all(8.5 < g < 12 for g in gaps), gaps


HIDE = """hidden => {
    Object.defineProperty(document, "hidden", { configurable: true, get: () => hidden });
    Object.defineProperty(document, "visibilityState", { configurable: true, get: () => hidden ? "hidden" : "visible" });
    document.dispatchEvent(new Event("visibilitychange"));
}"""


@pytest.mark.case("LIVE-05")
def test_unread_count_in_the_tab_title(ready, client):
    page = sign_in(client(), ready.url, ready.accounts, "nas")
    ready.accounts.secret("Anna laptop")  # created (and announced) before
    expect(page).to_have_title("cpsync")
    page.evaluate(HIDE, True)  # the tab goes to the background

    anna = ready.api().login(ready.accounts, "Anna laptop")
    me = ready.api().login(ready.accounts, "nas")  # the same user elsewhere
    for i in range(3):
        anna.post_text(f"while away {i}")
        expect(message(page, f"while away {i}")).to_be_visible()
    me.post_text("my own message")
    expect(message(page, "my own message")).to_be_visible()
    expect(page).to_have_title("(3) cpsync")  # own messages don't count

    page.evaluate(HIDE, False)
    expect(page).to_have_title("cpsync")


@pytest.mark.case("LIVE-06")
def test_many_clients_see_the_same_order(ready, client):
    pages = [sign_in(client(), ready.url, ready.accounts, f"client {i}") for i in range(5)]
    senders = [ready.api().login(ready.accounts, f"client {i}") for i in range(3)]

    def burst(api, who):
        for n in range(5):
            api.post_text(f"{who}-{n}")

    threads = [threading.Thread(target=burst, args=(api, i)) for i, api in enumerate(senders)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()

    expect(pages[0].locator(USER_MESSAGES)).to_have_count(15, timeout=10_000)
    order = [m["body"] for m in senders[0].messages() if m["kind"] != "system"]
    for page in pages:
        expect(page.locator(USER_MESSAGES)).to_have_count(15, timeout=10_000)
        shown = page.locator(f"{USER_MESSAGES} pre.msg-body").all_inner_texts()
        assert shown == order

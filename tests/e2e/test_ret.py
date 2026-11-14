"""RET: retention."""

import pytest
from playwright.sync_api import expect

from harness import ApiClient, totp_code
from ui import MESSAGES, SYSTEM_MESSAGES, USER_MESSAGES, sign_in


def wrong_code(secret):
    return "000000" if totp_code(secret) != "000000" else "111111"


@pytest.mark.case("RET-01")
@pytest.mark.parametrize("limit", [5, 20])
def test_only_the_latest_messages_are_kept(make_ready, client, limit):
    extra = ("--max-messages", str(limit)) if limit != 20 else ()  # 20 is the default
    r = make_ready(*extra)
    api = r.api().login(r.accounts, "script")
    page = sign_in(client(), r.url, r.accounts, "watcher")

    for i in range(1, limit + 1):
        api.post_text(str(i))
    expect(page.locator(USER_MESSAGES)).to_have_count(limit)
    api.post_text(str(limit + 1))

    # The oldest message disappears for everyone, at once.
    expect(page.locator(USER_MESSAGES)).to_have_count(limit)
    expect(page.locator(USER_MESSAGES).first.locator("pre.msg-body")).to_have_text("2")
    assert [m["body"] for m in api.messages()] == [str(i) for i in range(2, limit + 2)]

    page.reload()  # deleted on the server, not just hidden
    expect(page.locator(USER_MESSAGES)).to_have_count(limit)
    expect(page.locator(USER_MESSAGES).first.locator("pre.msg-body")).to_have_text("2")


@pytest.mark.case("RET-02")
def test_files_are_deleted_with_their_message(make_ready):
    r = make_ready("--max-messages", "5")
    api = r.api().login(r.accounts, "script")
    up = api.upload("small.txt", b"soon gone")
    assert up.status == 201, up.body
    href = f"/api/files/{up.body['file']['id']}"
    assert api.get(href).status == 200

    for i in range(5):
        api.post_text(f"push {i}")
    assert all(m.get("file") is None for m in api.messages())

    gone = api.get(href)
    assert gone.status == 404 and gone.body["error"] == "This message no longer exists."


@pytest.mark.case("RET-03")
def test_system_messages_count_toward_the_limit(make_ready, client):
    r = make_ready("--max-messages", "5")
    api = r.api().login(r.accounts, "script")
    page = sign_in(client(), r.url, r.accounts, "watcher")
    for i in range(1, 6):
        api.post_text(str(i))
    expect(page.locator(USER_MESSAGES)).to_have_count(5)

    stranger = r.api("127.0.0.3")
    assert stranger.post("/api/login", {"code": wrong_code(r.secret)}).status == 401

    expect(page.locator(SYSTEM_MESSAGES)).to_have_count(1)
    expect(page.locator(MESSAGES)).to_have_count(5)
    expect(page.locator(USER_MESSAGES).first.locator("pre.msg-body")).to_have_text("2")
    kinds = [m["kind"] for m in api.messages()]
    assert len(kinds) == 5 and kinds[-1] == "system"


@pytest.mark.case("RET-04")
def test_lowering_the_limit(make_ready, client):
    r = make_ready("--max-messages", "5")
    api = r.api().login(r.accounts, "script")
    r.accounts.secret("watcher")  # created now: adding a user posts a system message
    for i in range(1, 6):
        api.post_text(str(i))

    r.server.restart("--max-messages", "3", same_port=True)
    api = ApiClient(r.server.base_url).login(r.accounts, "script")
    assert len(api.messages()) == 5, "nothing is removed until the next message"
    page = sign_in(client(), r.server.base_url, r.accounts, "watcher")
    expect(page.locator(USER_MESSAGES)).to_have_count(5)
    expect(page.locator("#hint")).to_contain_text("latest 3 messages")

    api.post_text("6")
    expect(page.locator(USER_MESSAGES)).to_have_count(3)
    assert [m["body"] for m in api.messages()] == ["4", "5", "6"]

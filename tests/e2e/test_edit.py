"""EDIT: editing, deleting and ownership."""

import pytest
from playwright.sync_api import expect

from ui import USER_MESSAGES, body_text, message, message_by_id, send, sign_in, toast, tool


@pytest.fixture
def two(ready, client):
    anna = sign_in(client(), ready.url, ready.accounts, "Anna laptop")
    nas = sign_in(client(), ready.url, ready.accounts, "nas")
    return anna, nas


@pytest.mark.case("EDIT-01")
def test_edit_your_own_message(two):
    anna, nas = two
    for text in ("first", "second message", "third"):
        send(anna, text)
    expect(nas.locator(USER_MESSAGES)).to_have_count(3)

    msg = message_by_id(anna, message(anna, "second message").get_attribute("data-id"))
    tool(msg, "Edit").click()
    editor = msg.locator(".edit-box textarea")
    expect(editor).to_have_value("second message")
    expect(msg.locator(".edit-box button", has_text="Cancel")).to_be_visible()
    new_text = "second message, edited\n  over two lines"
    editor.fill(new_text)
    msg.locator(".edit-box button", has_text="Save").click()
    expect(toast(anna)).to_have_text("Message updated.")

    for page in (anna, nas):
        edited = page.locator(USER_MESSAGES).nth(1)  # keeps its place
        expect(edited.locator(".msg-time")).to_contain_text("(edited)", timeout=5000)
        assert body_text(edited) == new_text
        expect(page.locator(USER_MESSAGES).first).to_contain_text("first")
        expect(page.locator(USER_MESSAGES).last).to_contain_text("third")


@pytest.mark.case("EDIT-05")
def test_other_peoples_messages_cannot_be_changed(two):
    anna, nas = two
    send(anna, "written by Anna")
    msg = message(nas, "written by Anna")
    expect(msg).to_be_visible()
    expect(tool(msg, "Copy")).to_be_visible()
    expect(tool(msg, "Edit")).to_have_count(0)
    expect(tool(msg, "Delete")).to_have_count(0)


@pytest.mark.case("EDIT-06")
def test_delete_your_own_message(ready, two, scratch):
    anna, nas = two
    send(anna, "delete me")
    expect(message(nas, "delete me")).to_be_visible()

    dialog = anna.locator("#dlg-delete")
    tool(message(anna, "delete me"), "Delete").click()
    expect(dialog).to_be_visible()
    expect(dialog.locator("h2")).to_have_text("Delete this message?")
    dialog.locator("button", has_text="Keep it").click()
    expect(dialog).to_be_hidden()
    expect(message(anna, "delete me")).to_be_visible()

    tool(message(anna, "delete me"), "Delete").click()
    dialog.locator("button[value=delete]").click()
    expect(toast(anna)).to_have_text("Message deleted.")
    for page in (anna, nas):
        expect(message(page, "delete me")).to_have_count(0, timeout=5000)

    # Deleting a file message removes the file from the server.
    f = scratch / "small.txt"
    f.write_text("bye")
    anna.set_input_files("#file-input", str(f))
    file_msg = anna.locator(USER_MESSAGES, has=anna.locator(".file-name", has_text="small.txt"))
    expect(file_msg).to_be_visible()
    href = file_msg.locator(".file a").get_attribute("href")
    tool(file_msg, "Delete").click()
    dialog.locator("button[value=delete]").click()
    expect(file_msg).to_have_count(0)
    r = ready.api().login(ready.accounts, "check").get(href)
    assert r.status == 404 and r.body["error"] == "This message no longer exists."


def open_editor(page, text):
    msg = message_by_id(page, message(page, text).get_attribute("data-id"))
    tool(msg, "Edit").click()
    editor = msg.locator(".edit-box textarea")
    expect(editor).to_be_focused()
    return msg, editor


@pytest.mark.case("EDIT-02")
def test_cancel_an_edit(two):
    anna, nas = two
    send(anna, "keep this text")
    msg, editor = open_editor(anna, "keep this text")
    editor.fill("something else")
    msg.locator(".edit-box button", has_text="Cancel").click()
    expect(msg.locator(".edit-box")).to_have_count(0)
    assert body_text(msg) == "keep this text"

    msg, editor = open_editor(anna, "keep this text")
    editor.fill("something else again")
    editor.press("Escape")
    expect(msg.locator(".edit-box")).to_have_count(0)
    assert body_text(msg) == "keep this text"
    for page in (anna, nas):
        expect(message(page, "keep this text").locator(".msg-time")).not_to_contain_text("(edited)")


@pytest.mark.case("EDIT-03")
def test_save_with_the_keyboard(two):
    anna, nas = two
    send(anna, "typo hre")
    msg, editor = open_editor(anna, "typo hre")
    editor.fill("typo here\nsecond line")
    editor.press("Enter")  # Enter adds a line
    expect(msg.locator(".edit-box")).to_have_count(1)
    editor.press("Backspace")
    editor.press("Control+Enter")
    expect(toast(anna)).to_have_text("Message updated.")
    for page in (anna, nas):
        edited = message(page, "typo here")
        expect(edited.locator(".msg-time")).to_contain_text("(edited)", timeout=5000)
        assert body_text(edited) == "typo here\nsecond line"


@pytest.mark.case("EDIT-04")
def test_edit_a_file_caption(two, scratch):
    anna, nas = two
    path = scratch / "report.pdf"
    path.write_bytes(b"%PDF-1.4 test")
    anna.fill("#compose", "draft report")
    anna.set_input_files("#file-input", str(path))
    expect(toast(anna)).to_have_text("Shared report.pdf.")

    msg, editor = open_editor(anna, "draft report")
    expect(editor).to_have_attribute("aria-label", "File caption")
    editor.fill("final report")
    msg.locator(".edit-box button", has_text="Save").click()
    expect(toast(anna)).to_have_text("Message updated.")
    for page in (anna, nas):
        box = page.locator(USER_MESSAGES, has=page.locator(".file-name", has_text="report.pdf"))
        expect(box.locator("pre.msg-body")).to_have_text("final report", timeout=5000)
        expect(box.locator(".msg-time")).to_contain_text("(edited)")

    msg, editor = open_editor(anna, "final report")
    editor.fill("")  # a file may have no caption
    msg.locator(".edit-box button", has_text="Save").click()
    expect(toast(anna)).to_have_text("Message updated.")
    for page in (anna, nas):
        box = page.locator(USER_MESSAGES, has=page.locator(".file-name", has_text="report.pdf"))
        expect(box.locator("pre.msg-body")).to_have_count(0, timeout=5000)
        expect(box.locator(".file a", has_text="Download")).to_be_visible()


@pytest.mark.case("EDIT-07")
def test_changes_by_someone_else_during_an_edit(ready, two):
    anna, nas = two
    send(anna, "original text")
    msg_id = message(anna, "original text").get_attribute("data-id")
    other = ready.api().login(ready.accounts, "Anna laptop")  # the same name on another machine

    msg, editor = open_editor(anna, "original text")
    editor.fill("my version")
    other.expect(other.put(f"/api/messages/{msg_id}", {"text": "their version"}), 200)
    expect(message(nas, "their version")).to_be_visible(timeout=5000)
    anna.wait_for_timeout(1500)
    expect(editor).to_be_visible()  # the open editor is left alone
    expect(editor).to_have_value("my version")

    msg.locator(".edit-box button", has_text="Save").click()  # the last save wins
    expect(toast(anna)).to_have_text("Message updated.")
    for page in (anna, nas):
        expect(message(page, "my version")).to_be_visible(timeout=5000)
        expect(message(page, "their version")).to_have_count(0)

    msg, editor = open_editor(anna, "my version")
    other.expect(other.delete(f"/api/messages/{msg_id}"), 200)
    for page in (anna, nas):
        expect(message_by_id(page, msg_id)).to_have_count(0, timeout=5000)

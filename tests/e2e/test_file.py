"""FILE: file sharing."""

import os
import re
from urllib.parse import unquote

import pytest
from playwright.sync_api import Error as PlaywrightError
from playwright.sync_api import expect

from harness import sha256
from ui import MESSAGES, body_text, sign_in, toast

MB = 1024 * 1024


@pytest.fixture
def two(ready, client):
    anna = sign_in(client(), ready.url, ready.accounts, "Anna laptop")
    nas = sign_in(client(), ready.url, ready.accounts, "nas")
    return anna, nas


def share(page, path, caption=""):
    """Shares a file with the + button, the message box holding the caption."""
    page.fill("#compose", caption)
    page.set_input_files("#file-input", str(path))


def file_box(page, name):
    return page.locator("#feed-inner .file", has=page.locator(".file-name", has_text=name))


def download(page, name, target):
    with page.expect_download() as dl:
        file_box(page, name).locator("a", has_text="Download").click()
    dl.value.save_as(target)
    return dl.value


@pytest.mark.case("FILE-01")
def test_share_a_file_with_the_plus_button(ready, two, scratch):
    anna, nas = two
    path = scratch / "small.txt"
    path.write_text("line one\nline two\n")
    share(anna, path)

    expect(toast(anna)).to_have_text("Shared small.txt.")
    for page in (anna, nas):
        box = file_box(page, "small.txt")
        expect(box).to_be_visible(timeout=5000)
        expect(box.locator(".file-size")).to_have_text("18 bytes")
        expect(box.locator("a", has_text="Download")).to_be_visible()
        msg = page.locator(MESSAGES, has=page.locator(".file-name", has_text="small.txt"))
        expect(msg).to_have_count(1)
        expect(msg.locator("pre.msg-body")).to_have_count(0)  # no caption
    ready.server.wait_for(r'\[chat\] "Anna laptop" shared file "small\.txt" \(18 bytes\)')


@pytest.mark.case("FILE-04")
def test_downloads_are_identical_to_the_original(two, scratch):
    anna, nas = two
    path = scratch / "photo.jpg"
    path.write_bytes(os.urandom(3 * MB))
    share(anna, path, caption="holiday photo")
    expect(toast(anna)).to_have_text("Shared photo.jpg.", timeout=20000)

    dl = download(nas, "photo.jpg", scratch / "downloaded.jpg")
    assert dl.suggested_filename == "photo.jpg"
    assert sha256((scratch / "downloaded.jpg").read_bytes()) == sha256(path.read_bytes())


@pytest.mark.case("FILE-05")
def test_size_limit(ready, two, scratch):
    anna, nas = two
    exact = scratch / "exact20.bin"
    exact.write_bytes(os.urandom(20 * MB))
    share(anna, exact)
    expect(toast(anna)).to_have_text("Shared exact20.bin.", timeout=60000)
    download(nas, "exact20.bin", scratch / "exact20.out")
    assert sha256((scratch / "exact20.out").read_bytes()) == sha256(exact.read_bytes())

    before = anna.locator(MESSAGES).count()
    too_big = scratch / "too-big.bin"
    too_big.write_bytes(b"\0" * (21 * MB))
    share(anna, too_big)
    expect(toast(anna)).to_have_text("too-big.bin is larger than the 20 MB limit.")
    expect(anna.locator(MESSAGES)).to_have_count(before)

    # The server enforces the limit too, whatever the page does.
    api = ready.api().login(ready.accounts, "script")
    r = api.upload("too-big.bin", b"\0" * (20 * MB + 1))
    assert r.status == 413 and r.body["error"] == "The file is larger than the 20 MB limit."


@pytest.mark.case("FILE-07")
def test_shared_files_are_never_opened_in_the_page(ready, two, scratch):
    anna, nas = two
    page_html = scratch / "page.html"
    page_html.write_text("<html><body><script>alert('xss')</script>hello</body></html>")
    share(anna, page_html)
    expect(file_box(nas, "page.html")).to_be_visible(timeout=5000)

    dialogs = []
    nas.on("dialog", lambda d: (dialogs.append(d.message), d.dismiss()))
    dl = download(nas, "page.html", scratch / "got.html")
    assert dl.suggested_filename == "page.html"
    assert (scratch / "got.html").read_text() == page_html.read_text()

    # Opening the link directly in a tab downloads it as well.
    href = file_box(nas, "page.html").locator("a").get_attribute("href")
    tab = nas.context.new_page()
    tab.on("dialog", lambda d: (dialogs.append(d.message), d.dismiss()))
    with tab.expect_download() as dl2:
        try:
            tab.goto(ready.url + href)
        except PlaywrightError:  # each engine reports the replaced navigation differently
            pass
    assert dl2.value.suggested_filename == "page.html"
    assert dialogs == []

    api = ready.api().login(ready.accounts, "script")
    r = api.get(href)
    assert r.status == 200
    assert r.headers["Content-Type"] == "application/octet-stream"
    assert r.headers["Content-Disposition"].startswith("attachment")
    assert r.headers["X-Content-Type-Options"] == "nosniff"


@pytest.mark.case("FILE-08")
def test_downloads_require_a_session(ready, two, client, scratch):
    anna, _ = two
    secret_file = scratch / "small.txt"
    secret_file.write_text("TOP-SECRET-CONTENT")
    share(anna, secret_file)
    href = file_box(anna, "small.txt").locator("a").get_attribute("href")
    assert re.fullmatch(r"/api/files/\d+", href)

    stranger = client()  # another browser, not signed in
    resp = stranger.request.get(ready.url + href)
    assert resp.status == 401
    assert resp.json()["code"] == "unauthenticated"
    assert b"TOP-SECRET-CONTENT" not in resp.body()


@pytest.mark.case("FILE-02")
def test_the_message_box_becomes_the_caption(two, scratch):
    anna, nas = two
    path = scratch / "nginx.conf"
    path.write_text("server { listen 80; }\n")
    caption = "new nginx config\nfor the NAS"
    share(anna, path, caption=caption)
    expect(toast(anna)).to_have_text("Shared nginx.conf.")
    expect(anna.locator("#compose")).to_have_value("")  # the caption was used
    for page in (anna, nas):
        msg = page.locator(MESSAGES, has=page.locator(".file-name", has_text="nginx.conf"))
        expect(msg).to_be_visible(timeout=5000)
        assert body_text(msg) == caption


DROP = """async ([sel, type, name, content]) => {
    const dt = new DataTransfer();
    if (name) dt.items.add(new File([content], name, { type: "text/plain" }));
    document.querySelector(sel).dispatchEvent(new DragEvent(type, { bubbles: true, cancelable: true, dataTransfer: dt }));
}"""


@pytest.mark.case("FILE-03")
def test_drag_and_drop(two):
    anna, nas = two
    overlay = anna.locator("#drop")
    expect(overlay).to_be_hidden()
    anna.evaluate(DROP, ["#feed", "dragenter", "dropped.txt", "dropped content"])
    expect(overlay).to_be_visible()
    expect(overlay).to_have_text("Drop to share this file")
    anna.evaluate(DROP, ["#feed", "dragleave", "dropped.txt", "dropped content"])
    expect(overlay).to_be_hidden()  # dragged away: nothing shared

    anna.evaluate(DROP, ["#feed", "dragenter", "dropped.txt", "dropped content"])
    anna.evaluate(DROP, ["#feed", "drop", "dropped.txt", "dropped content"])
    expect(overlay).to_be_hidden()
    expect(toast(anna)).to_have_text("Shared dropped.txt.")
    expect(file_box(nas, "dropped.txt")).to_be_visible(timeout=5000)
    expect(file_box(nas, "dropped.txt").locator(".file-size")).to_have_text("15 bytes")


@pytest.mark.case("FILE-06")
def test_unusual_file_names(ready, two, scratch):
    anna, nas = two
    name = "weird name é #1 (copy).txt"
    path = scratch / name
    path.write_text("unusual\n")
    share(anna, path)
    expect(toast(anna)).to_have_text(f"Shared {name}.")
    expect(file_box(nas, name).locator(".file-name")).to_have_text(name, timeout=5000)
    dl = download(nas, name, scratch / "got.txt")
    # Playwright's WebKit replaces spaces in suggested names with "_"; the
    # name the server sends is checked exactly below.
    assert dl.suggested_filename in (name, name.replace(" ", "_")), dl.suggested_filename
    assert (scratch / "got.txt").read_text() == "unusual\n"
    href = file_box(nas, name).locator("a").get_attribute("href")
    disposition = nas.request.get(ready.url + href).headers["content-disposition"]
    sent = re.search(r"filename\*=utf-8''(\S+)", disposition, re.I)
    assert sent and unquote(sent.group(1)) == name, disposition

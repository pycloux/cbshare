"""SU: startup and configuration."""

import ipaddress
import re
import signal
import sqlite3
from pathlib import Path

import pytest
from playwright.sync_api import expect

from harness import ADMIN_PASSWORD, ApiClient, free_port
from ui import expect_screen, send, sign_in

KEY_FORMAT = re.compile(r"^[A-Z2-7]{5}(-[A-Z2-7]{1,5})+$")
PIN_ALPHABET = set("ABCDEFGHJKMNPQRSTUVWXYZ23456789")  # no 0, O, 1, I, L


@pytest.mark.case("SU-01")
def test_first_start_creates_the_database(make_server):
    srv = make_server(bind=None)  # default bind: the detected LAN address
    srv.start(password=ADMIN_PASSWORD)
    out = srv.output

    assert "Database cbshare.db created." in out or f"Database {srv.db} created." in out
    assert "DATABASE KEY" in out and srv.key and KEY_FORMAT.match(srv.key), out
    assert f"Restart with:  cbshare --key {srv.key}" in out
    for line in ("port ", "max messages        20", "max file size       20 MB", "update mode         sse",
                 "failed attempts     5 before a 5m0s lockout", "session timeout     30m0s"):
        assert line in out, f"missing {line!r} in the configuration"
    assert "Generated a new self-signed TLS certificate." in out
    assert srv.fingerprint, "fingerprint not printed"

    host = re.search(r"Open https://([^:/]+):(\d+)/", out)
    assert host and int(host.group(2)) == srv.port
    assert not ipaddress.ip_address(host.group(1)).is_loopback, "the address shown must be the LAN address"

    assert srv.pin and len(srv.pin) == 6 and set(srv.pin) <= PIN_ALPHABET, srv.pin
    assert srv.db.exists()


@pytest.mark.case("SU-02")
def test_first_start_is_refused_when_a_database_exists(server):
    server.stop()
    before = server.db.stat()
    res = server.run("--db", str(server.db), "--password", "Other-Pass-1", "--port", "1")
    assert res.returncode != 0
    assert "database already exists" in res.stderr
    assert "restart with --key, or remove the file to start over" in res.stderr
    after = server.db.stat()
    assert (after.st_size, after.st_mtime_ns) == (before.st_size, before.st_mtime_ns)


@pytest.mark.case("SU-03")
def test_the_database_file_is_encrypted(ready):
    marker = "SU03-secret-message-7f3a9"
    ready.api().login(ready.accounts, "anna").post_text(marker)
    ready.server.stop()

    with pytest.raises(sqlite3.DatabaseError, match="not a database"):
        con = sqlite3.connect(ready.server.db)
        try:
            con.execute("select name from sqlite_master").fetchall()
        finally:
            con.close()

    raw = ready.server.db.read_bytes()
    assert not raw.startswith(b"SQLite format 3")
    assert marker.encode() not in raw


@pytest.mark.case("SU-04")
def test_restart_with_the_key(ready):
    srv = ready.server
    ready.api().login(ready.accounts, "anna").post_text("before the restart")
    first_fingerprint = srv.fingerprint

    srv.restart()
    out = srv.output
    assert f"Database {srv.db} unlocked." in out
    assert "Using the stored self-signed TLS certificate." in out
    assert srv.fingerprint == first_fingerprint
    assert "SETUP PIN" not in out

    api = ApiClient(srv.base_url).login(ready.accounts, "anna")  # same account and code
    assert [m["body"] for m in api.messages() if m["kind"] != "system"] == ["before the restart"]


@pytest.mark.case("SU-06")
def test_a_key_from_another_database_is_refused(make_server):
    a = make_server("a").start(password=ADMIN_PASSWORD)
    b = make_server("b").start(password=ADMIN_PASSWORD)
    a.stop()
    b.stop()
    before = a.db.read_bytes()

    res = a.run("--db", str(a.db), "--key", b.key, "--port", "1")
    assert res.returncode != 0
    assert "wrong key: the database cannot be decrypted with it" in res.stderr
    assert a.db.read_bytes() == before

    a.start(key=a.key)  # still opens with its own key
    assert "unlocked" in a.output


def alter(key: str) -> str:
    """The key with one character changed (a typo)."""
    first = key[0]
    return ("B" if first == "A" else "A") + key[1:]


@pytest.mark.case("SU-05")
def test_mistyped_keys_are_recognised(ready):
    srv = ready.server
    srv.stop()
    res = srv.run("--db", str(srv.db), "--key", alter(srv.key), "--port", "1")
    assert res.returncode != 0
    assert "invalid key: check that it was copied correctly" in res.stderr

    srv.start(key=srv.key.lower().replace("-", ""))  # case and dashes don't matter
    assert "unlocked" in srv.output
    srv.stop()
    srv.start(key=f"  {srv.key}  ")  # nor surrounding spaces
    assert "unlocked" in srv.output


@pytest.mark.case("SU-07")
def test_password_and_key_are_mutually_exclusive_and_required(make_server):
    srv = make_server()
    res = srv.run()
    assert res.returncode != 0
    assert "missing --password (first start) or --key (restart); run cbshare --help" in res.stderr
    res = srv.run("--password", "x", "--key", "y")
    assert res.returncode != 0
    assert "use either --password (first start) or --key (restart), not both" in res.stderr
    assert not srv.db.exists()


def cmdline(pid: int) -> str:
    return Path(f"/proc/{pid}/cmdline").read_bytes().replace(b"\0", b" ").decode()


@pytest.mark.case("SU-08")
def test_password_and_key_from_environment_variables(make_server):
    srv = make_server()
    srv.start(env={"CBSHARE_PASSWORD": ADMIN_PASSWORD})
    assert "DATABASE KEY" in srv.output and srv.key
    assert ADMIN_PASSWORD not in cmdline(srv.proc.pid)
    key = srv.key
    srv.stop()

    srv.start(env={"CBSHARE_KEY": key})
    assert "unlocked" in srv.output
    assert key not in cmdline(srv.proc.pid)
    ApiClient(srv.base_url).setup(srv.pin)  # a working server


@pytest.mark.case("SU-09")
def test_flags_override_the_defaults(ready, client):
    srv = ready.server
    srv.restart("--max-messages", "5", "--max-file-size", "2", "--session-timeout", "10m",
                "--lockout", "1m", "--max-attempts", "3", "--max-users", "4")
    out = srv.output
    for line in (f"port                {srv.port}", "max messages        5", "max file size       2 MB",
                 "max users           4",
                 "failed attempts     3 before a 1m0s lockout", "session timeout     10m0s"):
        assert line in out, f"missing {line!r}"
    page = sign_in(client(), srv.base_url, ready.accounts, "anna")
    expect(page.locator("#hint")).to_contain_text("up to 2 MB")
    expect(page.locator("#hint")).to_contain_text("latest 5 messages")


@pytest.mark.case("SU-10")
def test_yaml_file_and_precedence(ready, scratch):
    srv = ready.server
    srv.stop()
    yaml_port = free_port()
    conf = scratch / "test.yaml"
    conf.write_text(f"port: {yaml_port}\nmax_messages: 7\n")

    srv.start("--config", str(conf), key=srv.key, port=yaml_port, port_flag=False)
    assert f"port                {yaml_port}" in srv.output
    assert "max messages        7" in srv.output
    assert "max file size       20 MB" in srv.output  # not in the file: default
    srv.stop()

    srv.start("--config", str(conf), key=srv.key)  # with --port: the flag wins
    assert f"port                {srv.port}" in srv.output and srv.port != yaml_port
    assert "max messages        7" in srv.output


@pytest.mark.case("SU-11")
def test_invalid_configuration_is_rejected(make_server, scratch):
    srv = make_server()
    bad = scratch / "bad.yaml"
    bad.write_text("prot: 9000\n")
    cases = {
        ("--config", str(bad)): "field prot not found",
        ("--mode", "websocket"): 'mode must be "sse" or "polling"',
        ("--tls-cert", "cert.pem"): "tls_cert and tls_key must be set together",
        ("--port", "70000"): "port must be between 1 and 65535",
    }
    for args, message in cases.items():
        res = srv.run("--db", str(srv.db), *args, "--password", "x")
        assert res.returncode != 0, args
        assert message in res.stderr, (args, res.stderr)
    assert not srv.db.exists(), "no server may start"


@pytest.mark.case("SU-12")
def test_help_and_version(make_server):
    srv = make_server()
    res = srv.run("--help")
    assert res.returncode == 0
    for text in ("First start:  cbshare --password", "Restart:      cbshare --key", "-max-messages",
                 "number of messages kept in the chat (default 20)", "-mode"):
        assert text in res.stderr, text
    res = srv.run("--version")
    assert res.returncode == 0 and re.fullmatch(r"cbshare \S+\n", res.stdout)
    assert not any(srv.workdir.iterdir()), "no database is created"


@pytest.mark.case("SU-13")
def test_clean_stop(ready, client):
    srv = ready.server
    pages = [sign_in(client(), ready.url, ready.accounts, name) for name in ("anna", "nas")]
    send(pages[0], "before the stop")

    assert srv.stop(signal.SIGINT) == 0
    assert "server stopped" in srv.output
    for page in pages:
        expect(page.locator("#feed-inner article")).to_have_count(0, timeout=10_000)
        expect(page.locator("#toast")).to_have_text("The server cannot be reached.", timeout=10_000)

    srv.start(key=srv.key, port=srv.port)  # same address: the pages reconnect by themselves
    for page in pages:
        expect_screen(page, "login", timeout=15_000)

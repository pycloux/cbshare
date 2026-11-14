"""Tests of the test harness itself (not plan cases)."""

import pytest

import harness
from harness import ADMIN_PASSWORD, fingerprint_of, peer_certificate


@pytest.mark.p1
def test_a_port_taken_by_another_server_is_detected(make_server, monkeypatch):
    """Two parallel tests may pick the same free port. The second cbshare
    then fails to bind, while the port briefly answers with the first
    one: the harness must notice and retry on another port."""
    first = make_server("first").start(password=ADMIN_PASSWORD)

    ports = iter([first.port])  # first attempt: the port already in use
    real_free_port = harness.free_port
    monkeypatch.setattr(harness, "free_port", lambda: next(ports, None) or real_free_port())

    second = make_server("second").start(password=ADMIN_PASSWORD)
    assert second.port != first.port
    assert second.fingerprint != first.fingerprint
    assert fingerprint_of(peer_certificate("127.0.0.1", second.port)) == second.fingerprint
    assert "address already in use" not in second.output  # the output of the successful start

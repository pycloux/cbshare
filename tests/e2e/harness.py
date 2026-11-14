"""Building blocks for the external tests.

Everything here talks to cbshare from the outside, the way a person would:
it starts the executable, reads what it prints, and uses its HTTPS API.
Nothing imports cbshare's Go code.
"""

from __future__ import annotations

import hashlib
import os
import re
import signal
import socket
import ssl
import subprocess
import threading
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Callable

import pyotp
import requests
import urllib3
from requests.adapters import HTTPAdapter

urllib3.disable_warnings(urllib3.exceptions.InsecureRequestWarning)

ADMIN_PASSWORD = "Adm1n-Test-42"

# Key printed at first start: groups of 5 base32 characters.
KEY_RE = re.compile(r"Restart with:\s+cbshare --key (\S+)")
PIN_RE = re.compile(r"SETUP PIN:\s+(\S+)")
FINGERPRINT_RE = re.compile(r"Certificate SHA-256 fingerprint:\s+([0-9A-F:]{95})")
URL_RE = re.compile(r"Open (https://\S+?)/ in a browser")


def free_port() -> int:
    """Returns a TCP port that is free right now."""
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def totp_code(secret: str, at: float | None = None) -> str:
    """The 6-digit code an authenticator app shows for secret at a time."""
    return pyotp.TOTP(secret).at(at if at is not None else time.time())


# cbshare accepts each code once: after a sign-in with the code of a 30-second
# step, codes of that step and older ones are refused for that user (replay
# guard). The server accepts the codes of the previous, current and next
# steps, so a test can sign the same user in twice in a row without waiting
# (current, then next step); fresh_code picks the next unused step and only
# waits when none is left. It never uses the previous step: the clock could
# move on between making the code and the server checking it.
_last_step: dict[str, int] = {}
_step_lock = threading.Lock()


def fresh_code(secret: str) -> str:
    """A valid code for secret whose time step was never used before."""
    while True:
        with _step_lock:
            now = int(time.time()) // 30
            step = max(now, _last_step.get(secret, now - 1) + 1)
            if step <= now + 1:
                _last_step[secret] = step
                # Aim at the middle of the step, away from its edges.
                return totp_code(secret, step * 30 + 15)
        time.sleep(1)


def wrong_code(secret: str) -> str:
    """A 6-digit code that is not valid for secret right now."""
    valid = {totp_code(secret, time.time() + d) for d in (-30, 0, 30)}
    return next(c for c in ("000000", "111111", "222222", "333333") if c not in valid)


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


class ServerError(AssertionError):
    pass


class CbshareServer:
    """One cbshare process with its own folder, database and port.

    The console output is collected continuously; wait_for() waits for a
    line to appear. Stop it with stop() (Ctrl+C), which the fixtures also
    do at the end of each test.
    """

    def __init__(self, binary: str, workdir: Path, bind: str = "127.0.0.1", db: str = "cbshare.db"):
        self.binary = binary
        self.workdir = Path(workdir)
        self.workdir.mkdir(parents=True, exist_ok=True)
        self.bind = bind
        self.db = self.workdir / db
        self.port = 0
        self.proc: subprocess.Popen | None = None
        self.key: str | None = None
        self.pin: str | None = None
        self.fingerprint: str | None = None
        self.url: str | None = None
        self.history: list[str] = []  # console output of every run, for reports
        self._out: list[str] = []
        self._lock = threading.Lock()

    # ---------- process ----------

    def argv(self, *extra: str, password: str | None = None, key: str | None = None,
             port_flag: bool = True) -> list[str]:
        args = [self.binary, "--db", str(self.db)]
        if port_flag:
            args += ["--port", str(self.port)]
        if self.bind is not None:
            args += ["--bind", self.bind]
        args += list(extra)
        if password is not None:
            args += ["--password", password]
        if key is not None:
            args += ["--key", key]
        return args

    def start(self, *extra: str, password: str | None = None, key: str | None = None,
              env: dict | None = None, wait: bool = True,
              port: int | None = None, port_flag: bool = True) -> "CbshareServer":
        """Starts cbshare and, unless wait is False, waits until it listens.

        port: listen on this port (e.g. the previous one, so that open
        browsers reconnect); otherwise a free port is picked. A picked port
        can be taken by another test before cbshare binds it; the start is
        then retried on a new port. port_flag=False leaves --port out (the
        port then comes from a configuration file; pass it as port).
        """
        for attempt in range(3 if port is None else 1):
            self.port = port if port is not None else free_port()
            self._launch(self.argv(*extra, password=password, key=key, port_flag=port_flag), env)
            if not wait:
                return self
            try:
                self.wait_for(r"listening on", timeout=30)
                self._wait_accepting(timeout=10)
            except ServerError:
                if "address already in use" in self.output and attempt < 2:
                    self.stop()
                    if password is not None:
                        # A first start creates the database before binding
                        # the port; start over from an empty folder.
                        for suffix in ("", "-journal", "-wal", "-shm"):
                            Path(str(self.db) + suffix).unlink(missing_ok=True)
                    continue
                raise
            self._parse_banner()
            return self
        raise ServerError("could not find a free port")

    def _wait_accepting(self, timeout: float) -> None:
        """Waits until our cbshare answers on its port.

        "listening on" is printed just before cbshare binds the port, and
        tests running in parallel can pick the same free port: the other
        cbshare then fails to bind and exits, but for a moment the port
        answers with the wrong server. So the certificate presented on the
        port must be the one this process printed (each cbshare generates
        its own); otherwise we wait for our process to exit, and start()
        retries on another port.
        """
        self._parse_banner()
        host = self.bind if self.bind not in (None, "", "0.0.0.0") else "127.0.0.1"
        if self.bind is None:  # default bind: the LAN address printed in the URL
            m = URL_RE.search(self.output)
            host = m.group(1).split("//")[1].rsplit(":", 1)[0] if m else "127.0.0.1"
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if self.proc.poll() is not None:
                time.sleep(0.2)
                raise ServerError(f"cbshare exited ({self.proc.returncode}):\n{self.output}")
            try:
                der = peer_certificate(host, self.port)
                if self.fingerprint is None or fingerprint_of(der) == self.fingerprint:
                    return
            except OSError:
                pass
            time.sleep(0.05)
        raise ServerError(f"port {self.port} not answering with our certificate within {timeout}s:\n{self.output}")

    def _launch(self, argv: list[str], env: dict | None) -> None:
        with self._lock:
            self._out = []
        full_env = dict(os.environ)
        full_env.pop("CBSHARE_PASSWORD", None)
        full_env.pop("CBSHARE_KEY", None)
        if env:
            full_env.update(env)
        self.proc = subprocess.Popen(
            argv, cwd=self.workdir, env=full_env, text=True,
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, bufsize=1,
        )
        self.history.append("$ " + " ".join(self._redact(a) for a in argv))
        self._reader = threading.Thread(target=self._read, args=(self.proc,), daemon=True)
        self._reader.start()

    @staticmethod
    def _redact(arg: str) -> str:
        return arg if arg != ADMIN_PASSWORD else "<admin password>"

    def _read(self, proc: subprocess.Popen) -> None:
        for line in proc.stdout:
            with self._lock:
                self._out.append(line)
                self.history.append(line.rstrip("\n"))

    @property
    def output(self) -> str:
        """Console output of the current (or last) run."""
        with self._lock:
            return "".join(self._out)

    def wait_for(self, pattern: str, timeout: float = 15) -> re.Match:
        """Waits until the console output matches pattern."""
        rx = re.compile(pattern)
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            m = rx.search(self.output)
            if m:
                return m
            if self.proc and self.proc.poll() is not None:
                self._reader.join(5)  # let the reader thread drain the pipe
                m = rx.search(self.output)
                if m:
                    return m
                raise ServerError(f"cbshare exited ({self.proc.returncode}) before printing {pattern!r}:\n{self.output}")
            time.sleep(0.05)
        raise ServerError(f"{pattern!r} not printed within {timeout}s:\n{self.output}")

    def _parse_banner(self) -> None:
        out = self.output
        if m := KEY_RE.search(out):
            self.key = m.group(1)
        m = PIN_RE.search(out)
        self.pin = m.group(1) if m else None
        if m := FINGERPRINT_RE.search(out):
            self.fingerprint = m.group(1)
        if m := URL_RE.search(out):
            self.url = m.group(1)

    @property
    def base_url(self) -> str:
        host = self.bind if self.bind not in (None, "", "0.0.0.0") else "127.0.0.1"
        return f"https://{host}:{self.port}"

    def stop(self, sig: int = signal.SIGINT, timeout: float = 15) -> int | None:
        """Stops cbshare like Ctrl+C and returns its exit code."""
        if not self.proc:
            return None
        if self.proc.poll() is not None:
            self._reader.join(5)
            return self.proc.returncode
        self.proc.send_signal(sig)
        try:
            code = self.proc.wait(timeout)
            self._reader.join(5)  # the last lines may still be in the pipe
            return code
        except subprocess.TimeoutExpired:
            self.proc.kill()
            self.proc.wait()
            raise ServerError(f"cbshare did not stop within {timeout}s after signal {sig}")

    def restart(self, *extra: str, key: str | None = None, same_port: bool = False) -> "CbshareServer":
        """Stops cbshare and starts it again with the key printed at first start."""
        port = self.port if same_port else None
        self.stop()
        return self.start(*extra, key=key or self.key, port=port)

    def run(self, *args: str, env: dict | None = None, timeout: float = 30) -> subprocess.CompletedProcess:
        """Runs cbshare to completion (for commands that exit by themselves)."""
        full_env = dict(os.environ)
        full_env.pop("CBSHARE_PASSWORD", None)
        full_env.pop("CBSHARE_KEY", None)
        if env:
            full_env.update(env)
        argv = [self.binary, *args]
        res = subprocess.run(argv, cwd=self.workdir, env=full_env, text=True,
                             capture_output=True, timeout=timeout)
        self.history.append("$ " + " ".join(self._redact(a) for a in argv))
        self.history.extend((res.stdout + res.stderr).splitlines())
        return res


# ---------- API client ----------


class SourceAddressAdapter(HTTPAdapter):
    """Sends requests from a given local address (e.g. 127.0.0.2), so one
    machine can act as several clients with different IP addresses."""

    def __init__(self, source: str, **kw):
        self.source = (source, 0)
        super().__init__(**kw)

    def init_poolmanager(self, *a, **kw):
        kw["source_address"] = self.source
        super().init_poolmanager(*a, **kw)


@dataclass
class Resp:
    status: int
    body: dict
    raw: bytes
    headers: dict


class ApiClient:
    """A browser stand-in for the JSON API: own cookies, the Origin header
    browsers send, and the anti-CSRF token once signed in."""

    def __init__(self, base_url: str, source: str | None = None, timeout: float = 30):
        self.base = base_url
        self.s = requests.Session()
        self.s.verify = False
        self.s.trust_env = False  # talk to the local server directly, never through a proxy
        if source:
            ad = SourceAddressAdapter(source)
            self.s.mount("https://", ad)
        self.csrf = ""
        self.timeout = timeout

    def request(self, method: str, path: str, json=None, headers=None, origin=True, **kw) -> Resp:
        h = {}
        if origin:
            h["Origin"] = self.base
        if self.csrf:
            h["X-CSRF-Token"] = self.csrf
        if headers:
            h.update(headers)
        r = self.s.request(method, self.base + path, json=json, headers=h, timeout=self.timeout, **kw)
        body = {}
        if r.headers.get("Content-Type", "").startswith("application/json"):
            try:
                body = r.json()
            except ValueError:
                body = {}
        return Resp(r.status_code, body, r.content, dict(r.headers))

    def get(self, path, **kw):
        return self.request("GET", path, **kw)

    def post(self, path, json=None, **kw):
        return self.request("POST", path, json=json, **kw)

    def put(self, path, json=None, **kw):
        return self.request("PUT", path, json=json, **kw)

    def delete(self, path, **kw):
        return self.request("DELETE", path, **kw)

    def expect(self, resp: Resp, status: int) -> Resp:
        assert resp.status == status, f"HTTP {resp.status}, want {status}: {resp.raw[:300]!r}"
        return resp

    # ---------- flows ----------

    def setup(self, pin: str) -> str:
        """Completes the first-time setup and returns the admin account's
        TOTP secret."""
        r = self.expect(self.post("/api/setup/pin", {"pin": pin}), 200)
        self.expect(self.post("/api/setup/confirm"), 200)
        return r.body["secret"]

    def login(self, secret: "str | Accounts", username: str) -> "ApiClient":
        """Signs in as username. secret is the user's TOTP secret, or an
        Accounts, which then provides (and if needed creates) the user."""
        if isinstance(secret, Accounts):
            secret = secret.secret(username)
        r = self.expect(self.try_login(username, fresh_code(secret)), 200)
        return self

    def try_login(self, username: str, code: str) -> Resp:
        r = self.post("/api/login", {"username": username, "code": code})
        if r.status == 200:
            self.csrf = r.body["csrf"]
        return r

    def create_user(self, name: str) -> Resp:
        """Admin: creates a user; the response carries the QR code."""
        return self.post("/api/admin/users", {"name": name})

    def users(self) -> list[dict]:
        return self.expect(self.get("/api/admin/users"), 200).body["users"]

    def user_id(self, name: str) -> int:
        return next(u["id"] for u in self.users() if u["name"].lower() == name.lower())

    def post_text(self, text: str) -> dict:
        return self.expect(self.post("/api/messages", {"text": text}), 201).body

    def messages(self) -> list[dict]:
        return self.expect(self.get("/api/messages"), 200).body.get("messages", [])

    def upload(self, name: str, data: bytes, caption: str = "") -> Resp:
        files = {"file": (name, data, "application/octet-stream")}
        form = {"caption": caption} if caption else {}
        h = {"Origin": self.base, "X-CSRF-Token": self.csrf}
        r = self.s.post(self.base + "/api/files", files=files, data=form, headers=h, timeout=self.timeout)
        body = r.json() if r.headers.get("Content-Type", "").startswith("application/json") else {}
        return Resp(r.status_code, body, r.content, dict(r.headers))

    def admin_login(self, password: str = ADMIN_PASSWORD) -> Resp:
        return self.post("/api/admin/login", {"password": password})


class Accounts:
    """The user accounts of one cbshare server, for tests.

    secret(name) returns a user's TOTP secret, creating the user through the
    admin API on first use, as the admin would. "admin" is the account made
    at setup."""

    def __init__(self, base_url: "str | Callable[[], str]", admin_secret: str):
        # A function follows the server when a restart moves it to a new port.
        self._base = base_url if callable(base_url) else (lambda: base_url)
        self.admin_secret = admin_secret
        self._secrets: dict[str, str] = {"admin": admin_secret}
        self._admin: ApiClient | None = None
        self._lock = threading.Lock()

    def admin(self) -> ApiClient:
        """An API client signed in as the admin, with admin rights unlocked."""
        if self._admin is not None and self._admin.base != self._base():
            self._admin = None  # the server moved
        if self._admin is None:
            c = ApiClient(self._base()).login(self.admin_secret, "admin")
            c.expect(c.admin_login(), 200)
            self._admin = c
        return self._admin

    def secret(self, name: str) -> str:
        key = name.lower()
        with self._lock:
            if key not in self._secrets:
                r = self.admin().create_user(name)
                if r.status == 401:  # the helper session ended (restart, timeout): sign in again
                    self._admin = None
                    r = self.admin().create_user(name)
                self.admin().expect(r, 201)
                self._secrets[key] = r.body["enrollment"]["secret"]
            return self._secrets[key]

    def logout_admin(self) -> None:
        """Signs the helper admin session out (it is opened again if needed)."""
        if self._admin is not None:
            self._admin.post("/api/logout")
            self._admin = None

    def set_secret(self, name: str, secret: str) -> None:
        with self._lock:
            self._secrets[name.lower()] = secret


# ---------- TLS ----------


def peer_certificate(host: str, port: int) -> bytes:
    """Returns the DER certificate the server presents."""
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    with socket.create_connection((host, port), timeout=10) as sock:
        with ctx.wrap_socket(sock, server_hostname=host) as tls:
            return tls.getpeercert(binary_form=True)


def fingerprint_of(der: bytes) -> str:
    """SHA-256 fingerprint formatted like cbshare's console and browsers."""
    return ":".join(f"{b:02X}" for b in hashlib.sha256(der).digest())


# ---------- network cut ----------


class CuttableProxy:
    """A TCP proxy in front of cbshare that can cut the network: cut()
    drops every open connection and refuses new ones, restore() lets
    connections through again. Browsers that use it see a real network
    outage, while the server keeps running."""

    def __init__(self, target_port: int, host: str = "127.0.0.1"):
        self.target = (host, target_port)
        self.lsock = socket.socket()
        self.lsock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self.lsock.bind((host, 0))
        self.lsock.listen(64)
        self.port = self.lsock.getsockname()[1]
        self.url = f"https://{host}:{self.port}"
        self.cut_off = False
        self.conns: set[socket.socket] = set()
        self.lock = threading.Lock()
        self.running = True
        threading.Thread(target=self._accept, daemon=True).start()

    def _accept(self):
        while self.running:
            try:
                client, _ = self.lsock.accept()
            except OSError:
                return
            if self.cut_off:
                client.close()
                continue
            try:
                upstream = socket.create_connection(self.target, timeout=5)
                upstream.settimeout(None)
            except OSError:
                client.close()
                continue
            with self.lock:
                self.conns |= {client, upstream}
            for a, b in ((client, upstream), (upstream, client)):
                threading.Thread(target=self._pump, args=(a, b), daemon=True).start()

    def _pump(self, src, dst):
        try:
            while data := src.recv(65536):
                dst.sendall(data)
        except OSError:
            pass
        finally:
            for s in (src, dst):
                self._close(s)

    def _close(self, s):
        with self.lock:
            self.conns.discard(s)
        try:
            s.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass
        s.close()

    def cut(self):
        self.cut_off = True
        with self.lock:
            conns = list(self.conns)
        for s in conns:
            self._close(s)

    def restore(self):
        self.cut_off = False

    def close(self):
        self.running = False
        self.cut()
        self.lsock.close()


def fnv_cable(name: str) -> int:
    """The colour index (0-7) the front end gives an author (cableFor)."""
    h = 2166136261
    for ch in name:
        h = ((h ^ ord(ch)) * 16777619) & 0xFFFFFFFF
    return h % 8

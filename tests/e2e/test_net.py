"""NET: TLS and network."""

import datetime
import ipaddress
import re
import socket

import pytest
import requests
from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.x509.oid import NameOID
from playwright.sync_api import expect

from harness import ADMIN_PASSWORD, fingerprint_of, peer_certificate
from ui import expect_screen


@pytest.mark.case("NET-01")
def test_self_signed_certificate_and_fingerprint(server, client):
    der = peer_certificate("127.0.0.1", server.port)
    assert fingerprint_of(der) == server.fingerprint, "certificate differs from the console fingerprint"

    cert = x509.load_der_x509_certificate(der)
    assert cert.subject.get_attributes_for_oid(NameOID.COMMON_NAME)[0].value == "cbshare"
    ips = cert.extensions.get_extension_for_class(x509.SubjectAlternativeName).value.get_values_for_type(x509.IPAddress)
    assert ipaddress.ip_address("127.0.0.1") in ips

    # A client that checks certificates refuses it: it is self-signed.
    with pytest.raises(requests.exceptions.SSLError):
        s = requests.Session()
        s.trust_env = False
        s.get(server.base_url + "/", timeout=10)

    # After accepting the certificate, the setup PIN screen loads over HTTPS.
    page = client()
    page.goto(server.base_url + "/")
    expect_screen(page, "setup-pin")
    assert page.url.startswith("https://")
    expect(page.locator("#setup-pin")).to_be_visible()


def make_ca_and_cert(folder, ip="127.0.0.1"):
    """A test certificate authority and a server certificate it signed."""
    def key():
        return ec.generate_private_key(ec.SECP256R1())

    now = datetime.datetime.now(datetime.timezone.utc)
    ca_key = key()
    ca_name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "cbshare test CA")])
    ca = (x509.CertificateBuilder().subject_name(ca_name).issuer_name(ca_name)
          .public_key(ca_key.public_key()).serial_number(x509.random_serial_number())
          .not_valid_before(now - datetime.timedelta(hours=1)).not_valid_after(now + datetime.timedelta(days=30))
          .add_extension(x509.BasicConstraints(ca=True, path_length=None), critical=True)
          .add_extension(x509.KeyUsage(digital_signature=True, key_cert_sign=True, crl_sign=True,
                                       content_commitment=False, key_encipherment=False, data_encipherment=False,
                                       key_agreement=False, encipher_only=False, decipher_only=False), critical=True)
          .add_extension(x509.SubjectKeyIdentifier.from_public_key(ca_key.public_key()), critical=False)
          .sign(ca_key, hashes.SHA256()))
    srv_key = key()
    cert = (x509.CertificateBuilder()
            .subject_name(x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "cbshare.lan")]))
            .issuer_name(ca_name).public_key(srv_key.public_key()).serial_number(x509.random_serial_number())
            .not_valid_before(now - datetime.timedelta(hours=1)).not_valid_after(now + datetime.timedelta(days=30))
            .add_extension(x509.SubjectAlternativeName([x509.IPAddress(ipaddress.ip_address(ip))]), critical=False)
            .add_extension(x509.ExtendedKeyUsage([x509.oid.ExtendedKeyUsageOID.SERVER_AUTH]), critical=False)
            .add_extension(x509.SubjectKeyIdentifier.from_public_key(srv_key.public_key()), critical=False)
            .add_extension(x509.AuthorityKeyIdentifier.from_issuer_public_key(ca_key.public_key()), critical=False)
            .sign(ca_key, hashes.SHA256()))
    pem = serialization.Encoding.PEM
    paths = {name: folder / name for name in ("ca.pem", "cert.pem", "key.pem")}
    paths["ca.pem"].write_bytes(ca.public_bytes(pem))
    paths["cert.pem"].write_bytes(cert.public_bytes(pem))
    paths["key.pem"].write_bytes(srv_key.private_bytes(pem, serialization.PrivateFormat.PKCS8,
                                                       serialization.NoEncryption()))
    return paths, fingerprint_of(cert.public_bytes(serialization.Encoding.DER))


def get(url, **kw):
    s = requests.Session()
    s.trust_env = False
    return s.get(url, timeout=10, **kw)


@pytest.mark.case("NET-03")
def test_plain_http_is_not_served(ready):
    r = get(f"http://127.0.0.1:{ready.server.port}/api/state")
    assert r.status_code == 400
    assert "HTTP request to an HTTPS server" in r.text
    assert "csrf" not in r.text and "cbshare" not in r.text.lower()


@pytest.mark.case("NET-04")
def test_custom_certificate(ready, scratch):
    srv = ready.server
    self_signed = srv.fingerprint
    paths, fp = make_ca_and_cert(scratch)

    srv.restart("--tls-cert", str(paths["cert.pem"]), "--tls-key", str(paths["key.pem"]))
    assert srv.fingerprint == fp
    assert "self-signed" not in srv.output
    r = get(srv.base_url + "/api/health", verify=str(paths["ca.pem"]))  # trusted: no warning
    assert r.status_code == 200

    srv.restart()  # without the flags: back to the stored self-signed certificate
    assert "Using the stored self-signed TLS certificate." in srv.output
    assert srv.fingerprint == self_signed

    srv.stop()
    res = srv.run("--db", str(srv.db), "--key", srv.key, "--tls-cert", str(paths["cert.pem"]),
                  "--tls-key", str(scratch / "missing.pem"), "--bind", "127.0.0.1", "--port", "1")
    assert res.returncode != 0 and "load TLS certificate:" in res.stderr


def reachable(host, port):
    try:
        with socket.create_connection((host, port), timeout=2):
            return True
    except OSError:
        return False


def lan_ip(output):
    return re.search(r"Open https://([0-9.]+):\d+/", output).group(1)


@pytest.mark.case("NET-05")
def test_bind_address(make_server):
    srv = make_server(bind=None)  # default: the main LAN address
    srv.start(password=ADMIN_PASSWORD)
    lan = lan_ip(srv.output)
    assert not ipaddress.ip_address(lan).is_loopback
    assert reachable(lan, srv.port) and not reachable("127.0.0.1", srv.port)

    srv.stop()
    srv.bind = "127.0.0.1"
    srv.start(key=srv.key)
    assert reachable("127.0.0.1", srv.port) and not reachable(lan, srv.port)

    srv.stop()
    srv.bind = "0.0.0.0"
    srv.start(key=srv.key)
    assert reachable("127.0.0.1", srv.port) and reachable(lan, srv.port)
    assert lan_ip(srv.output) == lan, "the printed address is the LAN address, not 0.0.0.0"


@pytest.mark.case("NET-06")
def test_certificate_renewed_when_the_address_changes(make_server):
    srv = make_server()  # bound to 127.0.0.1
    srv.start(password=ADMIN_PASSWORD)
    first = srv.fingerprint

    srv.stop()
    srv.bind = None  # now the LAN address, which the certificate does not cover
    srv.start(key=srv.key)
    assert "Generated a new self-signed TLS certificate." in srv.output
    second = srv.fingerprint
    assert second != first

    srv.restart()
    assert "Using the stored self-signed TLS certificate." in srv.output
    assert srv.fingerprint == second

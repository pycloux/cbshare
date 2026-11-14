# Configuration

cbshare works without any configuration. Every setting has a default that can be changed with a command-line flag or in a YAML file. When both are given, the flag wins.

```sh
cbshare --key <key> --config cbshare.yaml --port 9443
```

## Settings

| Flag | YAML key | Default | Meaning |
| --- | --- | --- | --- |
| `--port` | `port` | `8443` | HTTPS port. |
| `--bind` | `bind` | primary LAN IP | Address to listen on. Use `0.0.0.0` for every interface. |
| `--db` | `database` | `cbshare.db` | Path of the encrypted database. |
| `--tls-cert` | `tls_cert` | | Your own certificate (PEM). Requires `--tls-key`. |
| `--tls-key` | `tls_key` | | Private key for `--tls-cert` (PEM). |
| `--max-messages` | `max_messages` | `20` | Messages kept in the chat, system messages included. |
| `--max-file-size` | `max_file_size_mb` | `20` | Maximum file size, in MB. |
| `--max-users` | `max_users` | `10` | User accounts, the admin included (1 to 100). |
| `--mode` | `mode` | `sse` | `sse` for live updates, `polling` as a fallback. |
| `--poll-interval` | `poll_interval` | `3s` | How often browsers check for news in polling mode. |
| `--max-attempts` | `max_failed_attempts` | `5` | Failed attempts before an IP address is locked out. |
| `--lockout` | `lockout_duration` | `5m` | How long a lockout lasts. |
| `--session-timeout` | `session_timeout` | `30m` | Sign-out after this much inactivity. |
| `--pin-length` | `pin_length` | `6` | Length of the setup PIN (4 to 32). |

Durations use Go's format: `500ms`, `3s`, `5m`, `1h30m`.

These options are only accepted on the command line:

| Flag | Meaning |
| --- | --- |
| `--password` | Admin password. First start only: creates the database and prints the key. Or set `CBSHARE_PASSWORD`. |
| `--key` | Database key printed at first start, to restart the server. Or set `CBSHARE_KEY`. |
| `--config` | YAML file to read. |
| `--version` | Print the version and exit. |
| `--help` | List every flag. |

## YAML file

Every key is optional. Unknown keys are rejected so that a typo doesn't silently fall back to a default.

```yaml
port: 8443
# bind: 192.168.1.10
database: /var/lib/cbshare/cbshare.db

# tls_cert: /etc/cbshare/cert.pem
# tls_key: /etc/cbshare/key.pem

max_messages: 20
max_file_size_mb: 20

max_users: 10

mode: sse
poll_interval: 3s

max_failed_attempts: 5
lockout_duration: 5m
session_timeout: 30m
pin_length: 6
```

The full example is in [`cbshare.example.yaml`](https://github.com/pycloux/cbshare/blob/main/cbshare.example.yaml).

## Network address

By default cbshare listens on the machine's main LAN address, the one it uses to reach other networks, and prints the URL to open. On a machine with several network cards, choose one with `--bind`, or listen everywhere with `--bind 0.0.0.0`.

Ports below 1024, such as 443, need administrator rights on Linux. Keep 8443, or allow the binary to use low ports with `sudo setcap cap_net_bind_service=+ep ./cbshare`.

## TLS certificate

Without settings, cbshare creates a self-signed certificate on first start. It covers the bound address, the host name and `localhost`, is stored in the encrypted database and is reused across restarts. A new one is created if the address changes or the certificate is within 30 days of expiring; browsers then show the warning once more.

If your LAN has its own certificate authority, or you have a certificate for a local domain name, use it:

```sh
cbshare --key <key> --tls-cert cert.pem --tls-key key.pem
```

The certificate must cover the address or name people type in the browser.

## Live updates

In `sse` mode (the default), browsers keep a Server-Sent Events connection open and receive changes instantly.

Some proxies, antivirus products and corporate network devices hold back streamed responses, so new messages only appear after a long delay or a reload. In that case, start cbshare with `--mode polling`. Browsers then ask the server every few seconds whether anything changed (`--poll-interval`, 3 seconds by default). The answer is a few bytes when nothing changed.

## Retention and limits

`max_messages` caps the conversation. When a message beyond the limit arrives, the oldest message is deleted, with its file. System messages, such as failed sign-in alerts, count toward the limit.

`max_file_size_mb` caps each file. Text messages are limited to 256 KB regardless; larger text should be shared as a file.

## Users

`max_users` is how many accounts the admin can have, the admin account included, whatever their status: revoked users and users whose QR code expired count until they are deleted. At the limit, **Add user** is disabled until a user is deleted. Lowering the limit below the current number of users removes no one; it only prevents adding more.

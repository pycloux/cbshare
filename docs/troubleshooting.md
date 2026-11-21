# Troubleshooting

## The server doesn't start

**`first start interrupted: no database was created; start again with --password`**
: Ctrl+C was pressed before the key was printed. cbshare removed the incomplete database, so you can simply start again with `--password`.

**`database already exists ... (restart with --key, or remove the file to start over)`**
: `--password` is only for the first start. Restart with `--key` and the key printed at first start.

**`this database uses an old format with one authenticator code shared by everyone, which this version cannot open`**
: The database comes from cpsync v0.2, the project cbshare grew out of, which used one authenticator code shared by everyone. cbshare uses a separate code for each user and does not convert those databases. Stop the server, delete the database file, and start again with `--password`. The old messages are lost, and everyone needs a new QR code from the admin.

**`database not found ... (start with --password to create it)`**
: The `--db` path doesn't point to an existing database. Check the path, or create a new database with `--password`.

**`invalid key: check that it was copied correctly`**
: The key's checksum doesn't match: a character is wrong or missing. Copy it again; dashes, spaces and letter case don't matter.

**`wrong key: the database cannot be decrypted with it`**
: The key is well formed but belongs to another database, for example one created by a different first start. Use the key printed when this database was created.

**`bind: address already in use`**
: Another program uses the port. Choose another one with `--port`.

**`bind: permission denied`**
: On Linux, ports below 1024 need administrator rights. Use the default 8443, or see [Network address](configuration.md#network-address).

## The page doesn't open from another machine

If the server printed **"WARNING: no network address was found"**, it could not find a network connection when it started and listens on `127.0.0.1` only, which other machines can't reach. Connect the server to the network and restart it, or choose an address with `--bind` (`--bind 0.0.0.0` listens on every interface). Otherwise:

- Check that both machines are on the same network, and that you use the address printed by the server.
- Allow cbshare through the firewall of the server: on Windows, allow it on private networks when asked; on Linux, open the port (for example `sudo ufw allow 8443/tcp`).
- cbshare listens on one address by default. If the server has several network cards and you use a different one, start it with `--bind 0.0.0.0`.
- The address must start with `https://`.

## The browser warns about the certificate

This is expected with the self-signed certificate: it isn't issued by an authority your browser knows. Compare the fingerprint in the certificate details with the one printed in the console, then continue. Browsers remember the choice, and cbshare reuses its certificate across restarts.

You see the warning again if the server address changed, or the certificate was renewed close to its expiry. To avoid it completely, use a [certificate of your own](configuration.md#tls-certificate).

## "That username or code is not valid"

- Check your username: it is the one the admin created for you (letter case doesn't matter).
- Use your own `cbshare` entry for this server in your authenticator app (labelled with your username and the server address), and the code currently shown.
- One-time codes depend on the time. Check that the clocks of your phone and of the server are correct; a difference of more than about 30 seconds makes codes fail.
- Each code works only once. If you just signed in somewhere else, wait for the next code.
- If you never signed in with a new QR code, it expires after 24 hours. If the admin gave you a new QR code, revoked your access or removed your account, your code no longer works. In each case, ask the admin for a new QR code.

## I lost my phone or deleted the authenticator entry

Ask the admin for a new QR code (**Admin tools → Manage users → New QR code**). Your old code stops working at once, which also protects you if someone else has the phone. If you are the admin and lost the admin code, there is no way back in: delete the database and start again with `--password`.

## "Too many failed attempts"

Your IP address is locked out after 5 failures. Wait for the time shown (5 minutes by default) and try again. Everyone behind the same router, VPN address or reverse proxy shares the lockout; cbshare is meant to be reached directly, without a proxy (see [What is not protected](security.md#what-is-not-protected)).

## "Too many attempts at once" or "The server is busy checking other passwords"

Several sign-in or admin password attempts from your address are still being checked; their results count toward the lockout. Wait a second and try again. The busy message means other admin password checks are running: wait a few seconds.

## New messages don't appear

If the indicator says **Live** but messages only appear after a reload, a device on your network holds back streamed responses. Restart the server with `--mode polling`. See [Live updates](configuration.md#live-updates).

If it says **Reconnecting**, the server is unreachable or was restarted. After a restart, everyone signs in again.

## "Your session has ended"

Sessions end after 30 minutes without activity, when the browser is closed, when the server restarts, or when the admin gives you a new QR code, revokes your access or removes your account. Sign in again with a current code.

## Uploading fails

- "The file is larger than the 20 MB limit": the server limits file size; the admin can raise it with `--max-file-size`.
- "This page is out of date. Reload it and try again.": the page lost its session token, for example after a server restart. Reload the page.

## The key is lost

The database can't be decrypted without it, by design. Stop the server, delete the database file, and start again with `--password`. The admin then adds everyone again, and everyone needs a new QR code.

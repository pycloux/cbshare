# Security model

cbshare is built for a small, trusted group on a private network. This page explains what it protects against, and what it doesn't.

## What is protected

### In transit

All traffic uses HTTPS with TLS 1.2 or later. Without a certificate of your own, cbshare uses a self-signed ECDSA certificate. Browsers can't verify it automatically, so the first visit shows a warning; compare the SHA-256 fingerprint printed in the console with the one the browser shows to be sure you are talking to your server.

### At rest

Everything cbshare stores is in one SQLite database encrypted with the [Adiantum](https://github.com/ncruces/go-sqlite3/tree/main/vfs/adiantum) construction: every page of the database and of its journal is encrypted. The file cannot be opened with a SQLite client, and its content, including messages, files, every user's authenticator secret and the TLS key, is unreadable without the key.

The 256-bit key is derived at first start with Argon2id from the admin password, a random 16-byte salt and the current time. Starting twice with the same password therefore gives two different keys. The key is printed once, with a checksum that catches typos, and held only in the server's memory while it runs.

The admin password is not stored; only an Argon2id hash of it is, for the admin unlock.

### Access

- **Setup** is protected by a random PIN printed only in the server console, and the admin account's QR code is shown once.
- **Personal codes**: every user has their own TOTP secret, created by the admin. Sign-in requires the username and a valid time-based one-time code for that user's secret (RFC 6238, 6 digits, 30-second steps, one step of tolerance either way). A code proves who someone is, and the account name is the name shown next to their messages.
- **Each code works once**: after a sign-in, codes of the same or an earlier 30-second step are refused for that user, so someone who sees a code being typed cannot reuse it.
- **No guessing of names**: an unknown username, a wrong code, a revoked or expired account all get the same answer, in about the same time, and the chat alert doesn't name the account that was tried.
- **QR codes** are shown once: a user's when the admin creates the account or gives it a new code, the admin's at setup or when they replace it. They are never shown again, never logged, and responses that carry them are never cached. A new user's code must be used within 24 hours, or it expires and its secret is erased.
- **Brute force**: after 5 failed attempts (PIN, code or admin password) an IP address is locked out for 5 minutes, and every failure is announced to everyone in the chat with the source address. Attempts count from the moment they start, so sending many guesses at once doesn't get more than 5 checked. Admin password checks are deliberately slow (Argon2id), so at most two run at a time, whoever sends them; this keeps the server responsive.
- **Sessions** exist only in the server's memory. The browser holds a random 256-bit token in a cookie marked `Secure`, `HttpOnly` and `SameSite=Strict`, without an expiry date, so closing the browser ends the session. Sessions end after 30 minutes of inactivity; keeping the page open does not count as activity.
- **Revocation**: the admin can revoke a user's access, give them a new QR code, or delete the account. Each takes effect at once: the old code stops working, its secret is erased, and the user's sessions end.
- **Admin**: only the admin account can unlock admin rights, with the admin password; for other users the password is not even checked. The admin account cannot be revoked or deleted.

### In the browser

- A strict Content Security Policy allows only cbshare's own script and styles: no inline code, no third-party content, no framing.
- State-changing requests need a per-session anti-CSRF token and must come from the cbshare page itself (checked with the `Origin` and `Sec-Fetch-Site` headers). JSON endpoints only accept `application/json`.
- Messages are inserted as text, never as HTML.
- Shared files are always served as downloads (`Content-Disposition: attachment`, `application/octet-stream`, `nosniff`), so a malicious file can't run inside cbshare's page.

## What is not protected

!!! warning "Know these limits"
    - **An account is only as safe as its authenticator.** Anyone holding a user's phone or authenticator entry can sign in as them. Revoke the account or give it a new QR code when a phone is lost.
    - **QR codes travel outside cbshare.** The admin passes each code on by some other means; a photo sent through an insecure channel can be copied. Use a private channel, and give a new code if in doubt.
    - **The admin code cannot be recovered.** If the admin loses it, nobody can add users or replace codes; the only way out is a new database.
    - **Everyone signed in sees everything.** There are no private messages. Don't post something the whole group shouldn't see.
    - **The server machine is trusted.** While cbshare runs, the key is in its memory. Someone with administrator access to that machine can read it.
    - **The key and password are visible on the command line** to other users of the same machine (process list, shell history). Use the `CBSHARE_KEY` and `CBSHARE_PASSWORD` environment variables on shared machines.
    - **Deleted data**: cbshare enables SQLite's secure delete, which overwrites deleted content, and the database is encrypted anyway. Copies made by backups or file-system snapshots are outside cbshare's control.
    - **Self-signed certificates** are only as good as your habit of checking the fingerprint. Use your own certificate if your network has a certificate authority.
    - **No reverse proxy.** cbshare is meant to be reached directly. It identifies clients by their network address and deliberately ignores headers such as `X-Forwarded-For`, which a client could forge. Behind a reverse proxy, every client has the proxy's address: one person's failed attempts lock everyone out, and alerts show the proxy's address. The proxy must also pass on the `Host` header unchanged, or requests are refused as coming from another site.

## Reporting a problem

Open an issue on [GitHub](https://github.com/pycloux/cbshare/issues). For a vulnerability, contact the maintainer privately first.

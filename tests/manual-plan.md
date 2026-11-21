# cbshare manual test plan

## 1. Purpose and how to use this plan

This plan lists the checks a person still does before a release: what a machine can't check, such as real phones and authenticator apps, a Windows server, arm64 hardware, screen readers, a real 24-hour wait and reboots. It has **10 cases**:

- **5 cases that cannot be automated**: NET-02, USR-14, UI-05, PLAT-01, PLAT-03.
- **The manual part of 5 other cases** whose main part is automated: SET-05, USR-05, UI-01, UI-02, PLAT-02.

**Automated cases.** The external tests in [`tests/e2e`](e2e) (pytest and Playwright) run the 112 automated cases against the built program on Linux: every case in Chromium, and the browser cases in Firefox and WebKit too. The release workflow runs them all, and a release isn't published unless they pass. Pull requests into `develop` run the P1 cases; pull requests into `main` run them all. [`tests/e2e/cases.yaml`](e2e/cases.yaml) lists every case with its status; `manual` cases and the `manual_part` of other cases are the ones described here, and `tests/e2e/test_consistency.py` fails if this file and `cases.yaml` disagree.

**Scope:** real client devices on a LAN, authenticator apps on real phones, Edge and phone browsers, the on-screen keyboard, screen readers, a Windows server, arm64, the real 24-hour QR code window, a database in the old format, and running as a service.

**Out of scope:** everything automated (run in CI), load and performance testing, and penetration testing.

This file replaces the plan written for cpsync, the project cbshare grew out of, which was kept as an external document.

### Test case format

Each test case has:

- **ID**: the case ID from `tests/e2e/cases.yaml`, such as `NET-02`. IDs are stable; new cases get new numbers.
- **Priority**: **P1** blocks a release if it fails; **P2** should be fixed before release; **P3** can be fixed later.
- **Preconditions**: the state to be in before the first step.
- **Steps**: what to do, in order.
- **Expected result**: what must be true for the test to pass. Every point must hold.

### Rules

- Run a release's manual tests after the release workflow's external tests have passed, with the archives it published.
- A case **passes** only when every expected point holds. Record anything else as **Fail**, with what happened, a screenshot and the server console output.
- Record **Blocked** when a precondition cannot be reached, and name the failing case that caused it. Record **N/A** only where a case says so.
- Every P1 case must pass before a release (see section 4).

## 2. Test environment

### Machines

| Role | What to use | Used in |
| --- | --- | --- |
| Server A | Windows 10 or 11, 64-bit | PLAT-01, PLAT-03 |
| Server B | Linux (Ubuntu 22.04 or later, or Debian 12), 64-bit | NET-02, SET-05, USR-05, USR-14, UI-01, UI-02, UI-05, PLAT-03 |
| Server C | Raspberry Pi 4 or 5 running 64-bit Linux | PLAT-02 |
| Client 1 | A Windows computer on the same LAN with Edge, latest | NET-02, UI-01, PLAT-01 |
| Client 2 | Another computer, or another browser on Client 1 | The second person in the smoke run |
| Phones | An iPhone (Safari) and an Android phone (Chrome) on the same Wi-Fi, each with an authenticator app | NET-02, SET-05, UI-01, UI-02, UI-05 |

### Software and accounts

- The cbshare release archives under test (`linux-amd64`, `linux-arm64`, `windows-amd64`, `windows-arm64`), as published by the release workflow, and their `SHA256SUMS`. Record the version printed by `cbshare --version`.
- Authenticator apps on two devices (Google Authenticator, Microsoft Authenticator, 2FAS or Aegis), for SET-05.
- A screen reader: NVDA (Windows), VoiceOver (macOS or iPhone) or TalkBack (Android), for UI-05.
- For USR-14 only: a database created by cpsync v0.2 and its key, if one is available.

### Test data

On Client 1 and on one phone: `small.txt` (a few lines of text) and `photo.jpg` (any photo of 2 to 5 MB).

### Starting from a clean state

Unless a case says otherwise, a **fresh server** means:

1. Stop any running cbshare (Ctrl+C).
2. Delete `cbshare.db` (or the file given with `--db`).
3. In the authenticator apps, delete old `cbshare` entries for this server (they are labelled `<username>@<address>:<port>`) to avoid confusion.
4. Close every browser window used for testing (this ends sessions).
5. Start with `cbshare --password Adm1n-Test-42` and note the key, the certificate fingerprint and the setup PIN from the console.

Use the admin password `Adm1n-Test-42` everywhere so results are comparable. Keep the console visible during every test.

### Smoke run

UI-01, PLAT-01 and PLAT-02 repeat this short run on the browser or platform under test. It is the core of the automated cases SU-04, SET-04, AUTH-01, USR-01, CHAT-01, CHAT-03, FILE-01, FILE-04 and LIVE-01.

1. Start a fresh server. On the first device, open the address printed in the console, check that the certificate fingerprint matches the console and accept it, enter the setup PIN, scan the QR code with the authenticator app and choose **I've added it, continue**.
2. Sign in as `admin` with the current code. Choose **Admin**, enter the admin password, then **Admin tools → Manage users**, add the user `nas` and scan the QR code shown with the authenticator app of the second device. Choose **Done**.
3. On the second device or browser, sign in as `nas` with the code from its app.
4. As `admin`, send a message of three lines with an indented line. It appears for `nas` within 2 seconds without reloading, and the status shows **Live**.
5. As `nas`, click **Copy** on that message and paste it into a text editor: the text is identical, indentation included.
6. As `admin`, share `photo.jpg` with **+**. As `nas`, download it: it has the same size and opens.
7. Stop the server with Ctrl+C and restart it with `--key` and the key from step 1. Both browsers return to the sign-in screen; signing in again with the same authenticator entries works for both users, and the message and the photo are still there.

## 3. Manual test cases

### NET-02 Access from every client

**Priority:** P1. **Preconditions:** a fresh server on Server B, started without `--bind`.

**Steps**

1. Open the address printed in the console from Client 1, Client 2 and both phones.

**Expected**

- The page loads on every device after accepting the certificate, and the fingerprint shown by each browser matches the console.
- On a Windows server (PLAT-01), the first start showed a Windows Defender Firewall prompt; with access allowed on private networks, clients can connect.

### SET-05 The setup QR code is the admin account's own entry

**Priority:** P1. **Preconditions:** a fresh server on Server B; during setup, take a photo of the QR code on the screen before confirming. The automated test decodes the QR code and checks it is the admin account's entry; this case checks real authenticator apps.

**Steps**

1. Scan the photo (displayed on another screen) with the authenticator apps of both phones, then confirm setup in the browser.
2. Look at how each app labels the new entry, and compare the codes both apps show at the same moment.
3. Sign in as `admin` with a code from one app; sign out; sign in again with a code from the other app (wait for the next code: each code works once).
4. Unlock admin, add the user `nas`, and scan the user's QR code with one app. Sign in as `nas` with the admin entry's code, then with the `nas` entry's code.

**Expected**

- Both apps label the entry `cbshare` with the account `admin@<server address>:<port>`, and show the same code at the same time.
- Codes from both apps sign in as `admin`.
- The `nas` entry is labelled `nas@<server address>:<port>`. The admin entry's code does not sign in as `nas` ("That username or code is not valid…"); the `nas` entry's code does.

### USR-05 An unused QR code expires after 24 hours

**Priority:** P1. **Preconditions:** a fresh server on Server B, started from the release archive with `CBSHARE_TEST_ENROLMENT_WINDOW=1m` set in its environment (for example `CBSHARE_TEST_ENROLMENT_WINDOW=1m ./cbshare --password Adm1n-Test-42`). The automated test shortens the window to a few seconds with that variable, which only test builds read; this case checks the real 24-hour window and that release builds ignore the variable.

**Steps**

1. Sign in as `admin`, unlock admin and add the users `early` and `late`. Note the deadline shown under each QR code. Scan `early`'s QR code and sign in as `early`. Do not use `late`'s QR code; keep a photo of it.
2. Keep the server running for more than 24 hours.
3. Sign in as `late` with a code from the photographed QR code.
4. As `admin`, open **Admin tools → Manage users**, give `late` a new QR code and sign in as `late` with it.

**Expected**

- In step 1, each deadline is 24 hours after the user was added, not one minute: the release build ignores `CBSHARE_TEST_ENROLMENT_WINDOW`. The user list shows `late` as "Waiting for first sign-in, until …" and `early` as "Active".
- In step 3, the sign-in is refused with the usual "That username or code is not valid…" message. The user list shows `late` as "QR code expired" and `early` still "Active".
- The chat shows "The QR code of late expired before it was used. The admin can create a new one.", and the console shows `[users] the QR code of "late" expired before its first sign-in`.
- In step 4, the new QR code works.

### USR-14 A database in the old shared-code format is refused with a clear message

**Priority:** P2. **Preconditions:** a database created by cpsync v0.2 and its key. No cbshare release ever wrote this format, so if no such database is available, record **N/A**; the Go test `TestOldSchemaRefused` covers the refusal.

**Steps**

1. Note the size and modification time of the old database file.
2. Run `cbshare --db <old database> --key <its key>`.

**Expected**

- cbshare stops with `error: this database uses an old format with one authenticator code shared by everyone, which this version cannot open: delete it and start again with --password` and an exit status other than 0.
- The file's size and modification time are unchanged.

### UI-01 Supported browsers

**Priority:** P1. The automated tests run the main flow in Chromium, Firefox and WebKit on Linux; this case covers the browsers people use on other systems.

**Steps**

1. Do the smoke run with Edge on Windows (Client 1) as `admin` and Safari on iPhone as `nas`.
2. Do it again with Chrome on Android as `nas`.

**Expected**

- Every step of the smoke run passes in every browser, with no layout breakage.

### UI-02 Phone layout

**Priority:** P2. **Preconditions:** signed in on a phone, chat with long messages and a file. The automated test checks the layout at phone size; this case checks the real on-screen keyboard.

**Steps**

1. Use the chat in portrait orientation: read, scroll, copy, open the keyboard and send, share a photo with **+**.
2. Rotate to landscape and send another message.
3. Signed in as `admin` on the phone, unlock admin and add a user from **Admin tools → Manage users**, typing the name with the on-screen keyboard.

**Expected**

- The message box stays visible above the on-screen keyboard in both orientations; sending and sharing work.
- No horizontal scrolling while the keyboard is open.
- The admin dialogs and the new user's QR code fit on the screen and can be used with the keyboard open.

### UI-05 Screen reader basics

**Priority:** P3.

**Steps**

1. With NVDA (Windows), VoiceOver (macOS or iPhone) or TalkBack, go through sign-in and the chat.
2. Have someone post a message from another client.
3. Enter a wrong code once, then sign in.

**Expected**

- Fields are announced with their labels ("Setup PIN", "Username", "One-time code", "Message", and "Admin password" in the admin dialog); the + button as "Attach a file".
- Error messages are announced when they appear; new messages are announced politely.
- Dialogs are announced with their title ("Admin", "Admin tools", "Users", "Delete this message?").

### PLAT-01 Windows server

**Priority:** P1. The external tests run on Linux only; the Go tests also run on Windows in CI.

**Steps**

1. On Server A, extract the `windows-amd64` archive. Start with `--db "C:\Users\<you>\My Data\cbshare.db"` (a path with spaces), then do the smoke run from Client 1 and a phone, restarting with `--key` and the same `--db`.
2. Restart once more with the key in the environment instead of the command line: in PowerShell, `$env:CBSHARE_KEY = "<key>"`, then start with only `--db`.
3. Run NET-02 against Server A.
4. If a Windows on Arm computer is available, repeat step 1 with the `windows-arm64` archive; otherwise note N/A for this step.

**Expected**

- The smoke run passes. The database is created in the folder with spaces and reopens on restart, also with the key from `CBSHARE_KEY`.
- NET-02 passes.

### PLAT-02 Linux server

**Priority:** P1. The external tests cover Linux on amd64; this case covers the arm64 build.

**Steps**

1. On Server C, check the archive against `SHA256SUMS`, extract the `linux-arm64` archive and do the smoke run.

**Expected**

- The checksum matches and the smoke run passes.

### PLAT-03 Running as a service

**Priority:** P3.

**Steps**

1. Follow the documentation's "Running cbshare as a service" section (`docs/administration.md`: systemd on Linux, Task Scheduler on Windows), on Server B and Server A.
2. Reboot the server.
3. Look at the service definition and at the running process's command line (`ps -ef` or `/proc/<pid>/cmdline` on Linux, the Details tab of Task Manager with the "Command line" column on Windows).

**Expected**

- cbshare starts on boot with the key from the environment; clients can sign in with their existing authenticator entries.
- The key appears neither in the service definition nor in the process's command line. On Linux, `journalctl -u cbshare` shows the server's output.

## 4. Test run record and sign-off

This plan has **10 cases**: 6 P1 (NET-02, SET-05, USR-05, UI-01, PLAT-01, PLAT-02), 2 P2 (USR-14, UI-02) and 2 P3 (UI-05, PLAT-03).

### Run details

| Item | Value |
| --- | --- |
| cbshare version (`cbshare --version`) | |
| Release workflow run (link) | |
| Date(s) of the run | |
| Tester(s) | |
| Server A (Windows): OS version | |
| Server B (Linux): distribution and version | |
| Server C (arm64): board and OS | |
| Clients: OS and browser versions | |
| Phones and authenticator apps | |

### Results

Write **Pass**, **Fail**, **Blocked** or **N/A**, and the issue number or a short note for anything but Pass.

| ID | Test case | Priority | Result | Notes |
| --- | --- | --- | --- | --- |
| NET-02 | Access from every client | P1 | | |
| SET-05 | The setup QR code is the admin account's own entry | P1 | | |
| USR-05 | An unused QR code expires after 24 hours | P1 | | |
| USR-14 | A database in the old shared-code format is refused with a clear message | P2 | | |
| UI-01 | Supported browsers | P1 | | |
| UI-02 | Phone layout | P2 | | |
| UI-05 | Screen reader basics | P3 | | |
| PLAT-01 | Windows server | P1 | | |
| PLAT-02 | Linux server | P1 | | |
| PLAT-03 | Running as a service | P3 | | |

### Release criteria

A release can go ahead when:

- the release workflow passed: Go tests on Linux and Windows, and the external tests (every automated case in Chromium, and the browser cases in Firefox and WebKit);
- every **P1** case of this plan passes;
- every failed **P2** case has an issue and a decision (fix before release, or accept and document);
- failed **P3** cases have issues;
- no case is left **Blocked**.

### Sign-off

| Role | Name | Decision | Date |
| --- | --- | --- | --- |
| Tester | | | |
| Product owner | | Release / Do not release | |

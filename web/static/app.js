"use strict";

// cpsync front end. No framework, no build step: this file is embedded in
// the server executable and served as is.
//
// The page (index.html) contains every screen; exactly one is visible:
//
//   setup-pin -> setup-qr      first-time setup only
//   login -> chat              every visit (username + one-time code)
//
// route() asks the server for GET /api/state and shows the right screen.
// Every other screen change goes back through route(), so the server is
// always the source of truth.
//
// Sections of this file:
//   API            fetch wrapper and error type
//   Screens        screen switching and routing
//   Setup, Sign in the forms of the sign-in screens
//   Chat           loading messages
//   Live updates   Server-Sent Events, or polling
//   Rendering      turning messages into DOM, copy/edit/delete
//   Composer       sending text and files, drag and drop
//   Admin          admin unlock and replacing the admin's own code
//   Users          the admin's user management: add, new QR code, revoke, delete
//   Toast          short notifications
//
// Security notes: message text is only ever inserted with textContent,
// never as HTML, and the Content Security Policy forbids inline scripts.
// The session cookie is HttpOnly; the anti-CSRF token (app.csrf) is the
// only secret this script holds, and it is sent back on every request.

// $ is a querySelector shortcut.
const $ = (sel, root = document) => root.querySelector(sel);

// Global page state.
const app = {
  state: null,   // last /api/state response
  csrf: "",
};

// ---------- API ----------

// ApiError carries the server's error response: message is the sentence to
// show, code the stable identifier used to react (see apiError in Go).
class ApiError extends Error {
  constructor(status, body) {
    super((body && body.error) || `Request failed (HTTP ${status}).`);
    this.status = status;
    this.code = body && body.code;
    this.body = body || {};
  }
}

// api calls the JSON API. body may be an object (sent as JSON) or a
// FormData (file uploads). It returns the parsed JSON response, or throws
// an ApiError. A 401 while signed in means the session ended: the page
// goes back to the sign-in screen, wherever the request came from.
async function api(method, path, body, { raw = false } = {}) {
  const headers = {};
  if (app.csrf) headers["X-CSRF-Token"] = app.csrf;
  let payload;
  if (body instanceof FormData) {
    payload = body;
  } else if (body !== undefined) {
    headers["Content-Type"] = "application/json";
    payload = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(path, { method, headers, body: payload, credentials: "same-origin", cache: "no-store" });
  } catch (e) {
    throw new ApiError(0, { error: "The server cannot be reached.", code: "network" });
  }
  if (raw && res.ok) return res;
  let data = null;
  if ((res.headers.get("Content-Type") || "").startsWith("application/json")) {
    data = await res.json().catch(() => null);
  }
  if (!res.ok) {
    const err = new ApiError(res.status, data);
    if (err.code === "unauthenticated" && app.state && app.state.authenticated) {
      signedOut("Your session has ended. Sign in again.");
    }
    throw err;
  }
  return data;
}

// ---------- Screens ----------

// Screen ids, matching the <section id="screen-..."> elements.
const screens = ["setup-pin", "setup-qr", "login", "chat"];

// show displays one screen, hides the others and focuses its first field.
function show(name) {
  for (const s of screens) $(`#screen-${s}`).hidden = s !== name;
  // On the sign-in screen, a remembered username skips to the code field.
  let focusable = $(`#screen-${name} input:not([type=file]), #screen-${name} textarea`);
  if (name === "login" && $("#login-username").value) focusable = $("#login-code");
  if (focusable) setTimeout(() => focusable.focus(), 0);
  document.title = name === "chat" ? "cpsync" : "cpsync – sign in";
}

// setError writes msg into the .error element of scope ("" clears it).
function setError(scope, msg) {
  const el = $(".error", scope);
  if (el) el.textContent = msg || "";
}

// busy disables a form's buttons while its request runs (no double submit).
function busy(form, on) {
  for (const b of form.querySelectorAll("button")) b.disabled = on;
}

// refreshState reloads /api/state, which also returns the CSRF token after
// a page reload.
async function refreshState() {
  app.state = await api("GET", "/api/state");
  app.csrf = app.state.csrf || "";
  return app.state;
}

// route shows the screen that matches the server state. If the server is
// unreachable it retries every 3 seconds.
async function route() {
  let s;
  try {
    s = await refreshState();
  } catch (e) {
    toast(e.message);
    setTimeout(route, 3000);
    return;
  }
  if (s.setupRequired) return show("setup-pin");
  if (!s.authenticated) {
    if (!$("#login-username").value) $("#login-username").value = rememberedUsername();
    return show("login");
  }
  enterChat();
}

// signedOut tears down the chat and goes back to the sign-in screen,
// optionally explaining why.
function signedOut(message) {
  leaveChat();
  if (app.state) app.state.authenticated = false;
  app.csrf = "";
  route().then(() => {
    if (message) setError($("#form-login"), message);
  });
}

// ---------- Setup ----------

$("#form-setup-pin").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const form = ev.currentTarget;
  setError(form, "");
  busy(form, true);
  try {
    const enr = await api("POST", "/api/setup/pin", { pin: $("#setup-pin").value.trim() });
    $("#setup-qr-img").src = enr.qr;
    $("#setup-secret").textContent = enr.secret;
    show("setup-qr");
  } catch (e) {
    if (e.code === "setup_done") return route();
    setError(form, attemptsMessage(e));
    $("#setup-pin").select();
  } finally {
    busy(form, false);
  }
});

$("#btn-setup-confirm").addEventListener("click", async (ev) => {
  const scope = $("#screen-setup-qr");
  const btn = ev.currentTarget;
  setError(scope, "");
  btn.disabled = true;
  try {
    const r = await api("POST", "/api/setup/confirm");
    $("#setup-qr-img").removeAttribute("src");
    $("#setup-secret").textContent = "";
    $("#login-username").value = r.username || "admin";
    toast("Setup complete. Sign in as admin with the code from your authenticator app.");
    route();
  } catch (e) {
    if (e.code === "setup_expired") {
      show("setup-pin");
      setError($("#form-setup-pin"), e.message);
    } else if (e.code === "setup_done") {
      route();
    } else {
      setError(scope, e.message);
    }
  } finally {
    btn.disabled = false;
  }
});

// ---------- Sign in ----------

$("#form-login").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const form = ev.currentTarget;
  setError(form, "");
  busy(form, true);
  try {
    const r = await api("POST", "/api/login", {
      username: $("#login-username").value,
      code: $("#login-code").value.replace(/\s/g, ""),
    });
    app.csrf = r.csrf;
    $("#login-code").value = "";
    rememberUsername(r.username);
    route();
  } catch (e) {
    if (e.code === "setup_required") return route();
    setError(form, attemptsMessage(e));
    $("#login-code").select();
  } finally {
    busy(form, false);
  }
});

// The last username is remembered in this browser, so that people only
// type the code next time. Storage can be unavailable (private windows,
// blocked site data): the page then simply asks for the name again.
const usernameKey = "cpsync.username";
function rememberedUsername() {
  try { return localStorage.getItem(usernameKey) || ""; } catch (e) { return ""; }
}
function rememberUsername(name) {
  try { if (name) localStorage.setItem(usernameKey, name); } catch (e) { /* not kept */ }
}

// attemptsMessage adds "N attempts left before a lockout" to a failed
// sign-in message, so people know a lockout is coming.
function attemptsMessage(e) {
  if (e.code === "invalid" && typeof e.body.remaining === "number") {
    const n = e.body.remaining;
    return `${e.message} ${n} ${n === 1 ? "attempt" : "attempts"} left before a lockout.`;
  }
  return e.message;
}

$("#btn-logout").addEventListener("click", async () => {
  try { await api("POST", "/api/logout"); } catch (e) { /* signed out anyway */ }
  signedOut();
});

// ---------- Chat ----------

// Chat state. The server sends the whole list each time (it is at most
// max_messages long); render() then updates the DOM in place, keeping the
// elements of unchanged messages so that scrolling, selection and edits in
// progress are not disturbed.
const chat = {
  active: false,
  rev: null,
  messages: [],
  nodes: new Map(),   // message id -> element
  editing: new Set(), // ids being edited (not replaced on refresh)
  loaded: false,
  unread: 0,
};

// enterChat shows the chat and starts loading messages and live updates.
function enterChat() {
  $("#me-name").textContent = app.state.username;
  updateAdminUI();
  $("#hint").textContent = `Ctrl+Enter to send. Drop a file anywhere to share it (up to ${formatSize(app.state.maxFileSize)}). The latest ${app.state.maxMessages} messages are kept.`;
  show("chat");
  if (!chat.active) {
    chat.active = true;
    chat.loaded = false;
    refreshSoon();
    startUpdates();
  }
}

// leaveChat stops live updates and forgets every message shown, so that
// nothing remains on screen after signing out.
function leaveChat() {
  chat.active = false;
  stopUpdates();
  chat.rev = null;
  chat.messages = [];
  chat.nodes.clear();
  chat.editing.clear();
  $("#feed-inner").replaceChildren();
}

let loading = null;
let reloadQueued = false;

// loadMessages fetches the chat unless it is unchanged; calls made while a
// request is in flight are merged into one follow-up request.
async function loadMessages() {
  if (!chat.active) return;
  if (loading) {
    reloadQueued = true;
    return loading;
  }
  loading = (async () => {
    try {
      do {
        reloadQueued = false;
        const q = chat.rev !== null ? `?since=${chat.rev}` : "";
        const r = await api("GET", `/api/messages${q}`);
        if (!chat.active) return;
        chat.rev = r.rev;
        if (!r.unchanged) render(r.messages || []);
      } while (reloadQueued && chat.active);
    } finally {
      loading = null;
    }
  })();
  return loading;
}

// refreshSoon reloads messages in the background, reporting errors.
function refreshSoon() {
  loadMessages().catch((e) => {
    if (e.code !== "unauthenticated") toast(e.message);
  });
}

// ---------- Live updates: Server-Sent Events, or polling ----------
//
// Either way, the server only says "the chat is now at revision N"; when N
// differs from chat.rev, loadMessages() fetches the list. The mode comes
// from the server's configuration (app.state.mode).

const updates = { source: null, timer: null, retry: null };

// setStatus updates the indicator next to the logo ("live" is green,
// "down" red).
function setStatus(kind, text) {
  const s = $("#status");
  s.className = `status ${kind}`;
  $("#status-text").textContent = text;
  s.title = text;
}

// startUpdates starts the mode the server uses.
function startUpdates() {
  stopUpdates();
  if (app.state.mode === "polling") startPolling();
  else startStream();
}

// stopUpdates closes the event stream and cancels pending timers.
function stopUpdates() {
  if (updates.source) updates.source.close();
  clearTimeout(updates.timer);
  clearTimeout(updates.retry);
  updates.source = updates.timer = updates.retry = null;
}

// startStream opens the Server-Sent Events stream. EventSource reconnects
// by itself after network errors; when the server refuses the stream (for
// example because the session ended), it gives up, so we check the
// session and either sign out or open a new stream.
function startStream() {
  setStatus("", "Connecting");
  const es = new EventSource("/api/events");
  updates.source = es;
  es.addEventListener("open", () => setStatus("live", "Live"));
  es.addEventListener("rev", (ev) => {
    setStatus("live", "Live");
    if (String(chat.rev) !== ev.data) refreshSoon();
  });
  es.addEventListener("logout", () => {
    es.close();
    signedOut("You were signed out. If the admin gave you a new QR code, add it to your authenticator app before signing in.");
  });
  es.addEventListener("error", () => {
    if (es.readyState !== EventSource.CLOSED) {
      setStatus("down", "Reconnecting");
      return;
    }
    // The server refused the stream: find out whether the session ended.
    setStatus("down", "Reconnecting");
    es.close();
    updates.retry = setTimeout(async () => {
      try {
        const s = await refreshState();
        if (!s.authenticated) return signedOut("Your session has ended. Sign in again.");
        if (chat.active) startStream();
      } catch (e) {
        if (chat.active) startStream();
      }
    }, 3000);
  });
}

// startPolling asks for news every poll interval. A setTimeout chain is
// used rather than setInterval so that slow responses never pile up.
function startPolling() {
  const every = app.state.pollIntervalMs;
  const label = `Updates every ${Math.round(every / 100) / 10} s`;
  setStatus("live", label);
  const tick = async () => {
    if (!chat.active) return;
    try {
      await loadMessages();
      setStatus("live", label);
    } catch (e) {
      if (e.code === "unauthenticated") return; // signedOut() already ran
      setStatus("down", "Offline, retrying");
    }
    if (chat.active) updates.timer = setTimeout(tick, every);
  };
  updates.timer = setTimeout(tick, every);
}

// ---------- Rendering ----------

// cableFor gives every author a colour, computed from the name (FNV-1a
// hash) so that it is the same for everyone and across sessions. The eight
// colours are the --cable-0..7 variables of style.css.
function cableFor(name) {
  let h = 2166136261;
  for (const c of name) h = Math.imul(h ^ c.codePointAt(0), 16777619);
  return `var(--cable-${(h >>> 0) % 8})`;
}

// isOwn tells whether the signed-in user posted m. Ownership follows the
// account id, not the name: a deleted user's name can be given to someone
// new, who does not own the old messages.
function isOwn(m) {
  return m.kind !== "system" && !!m.userId && m.userId === app.state.userId;
}

// canChange mirrors the server's rule (loadOwnMessage in chat.go): authors
// and the admin may change a message; system messages can only be deleted,
// by the admin. The server enforces it; this only hides useless buttons.
function canChange(m, deleting) {
  if (m.kind === "system") return deleting && app.state.admin;
  return isOwn(m) || app.state.admin;
}

// messageVersion is what an element's look depends on: an element is
// rebuilt when it changes (an edit, admin rights, its author deleted).
function messageVersion(m) {
  return `${m.id}:${m.editedAt || ""}:${app.state.admin}:${!!m.authorDeleted}`;
}

// render brings the feed in line with messages. Each element carries a
// version (see messageVersion): an element is rebuilt only when its
// version changed and the user is not editing it. The feed scrolls to the
// bottom on first load, or when the user was already near the bottom.
function render(messages) {
  const feed = $("#feed");
  const inner = $("#feed-inner");
  const nearBottom = feed.scrollHeight - feed.scrollTop - feed.clientHeight < 80;
  const firstLoad = !chat.loaded;
  const known = new Set(chat.messages.map((m) => m.id));
  chat.messages = messages;

  const ids = new Set(messages.map((m) => m.id));
  for (const [id, node] of chat.nodes) {
    if (!ids.has(id)) {
      node.remove();
      chat.nodes.delete(id);
      chat.editing.delete(id);
    }
  }

  let prev = null;
  for (const m of messages) {
    const version = messageVersion(m);
    let node = chat.nodes.get(m.id);
    if (!node || (node.dataset.version !== version && !chat.editing.has(m.id))) {
      const fresh = messageNode(m, version);
      if (chat.loaded && !known.has(m.id)) fresh.classList.add("fresh");
      if (node) node.replaceWith(fresh);
      node = fresh;
      chat.nodes.set(m.id, node);
    }
    const expected = prev ? prev.nextElementSibling : inner.firstElementChild;
    if (expected !== node) {
      if (prev) prev.after(node);
      else inner.prepend(node);
    }
    prev = node;
  }

  const empty = $(".empty", inner);
  if (messages.length === 0 && !empty) inner.append(emptyNode());
  if (messages.length > 0 && empty) empty.remove();

  if (chat.loaded && document.hidden) {
    const incoming = messages.filter((m) => !known.has(m.id) && !isOwn(m)).length;
    if (incoming) {
      chat.unread += incoming;
      document.title = `(${chat.unread}) cpsync`;
    }
  }
  chat.loaded = true;
  if (firstLoad || nearBottom) feed.scrollTop = feed.scrollHeight;
}

// Unread count in the tab title while the tab is in the background.
document.addEventListener("visibilitychange", () => {
  if (!document.hidden && chat.unread) {
    chat.unread = 0;
    if (chat.active) document.title = "cpsync";
  }
});

// emptyNode is shown when the chat has no messages.
function emptyNode() {
  const div = el("div", "empty");
  div.append(el("h2", "", "Nothing shared yet"),
    el("p", "", "Paste a command, a config snippet or a password, or drop a file. Everyone signed in sees it straight away."));
  return div;
}

// el creates an element. Text is always set with textContent, so message
// content can never be interpreted as HTML.
function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

// messageNode builds the <article> for one message: header (author, time,
// tools), then the file, then the text in a <pre> to keep its formatting.
function messageNode(m, version) {
  const own = isOwn(m);
  const art = el("article", `msg${own ? " own" : ""}${m.kind === "system" ? " system" : ""}${m.authorDeleted ? " deleted-author" : ""}`);
  art.dataset.version = version;
  art.dataset.id = m.id;
  if (m.kind !== "system") art.style.setProperty("--cable", cableFor(m.author));

  const head = el("header", "msg-head");
  const author = el("span", "msg-author", m.kind === "system" ? "cpsync" : m.author);
  if (m.authorDeleted) author.append(el("span", "gone", "(deleted user)"));
  head.append(author);
  const time = el("time", "msg-time", formatTime(m.createdAt) + (m.editedAt ? " (edited)" : ""));
  time.dateTime = m.createdAt;
  time.title = new Date(m.createdAt).toLocaleString();
  head.append(time);

  const tools = el("span", "msg-tools");
  const text = m.body || "";
  if (m.kind !== "system" && text) tools.append(toolButton("Copy", (b) => copyText(text, b)));
  if (canChange(m, false)) tools.append(toolButton("Edit", () => startEdit(m)));
  if (canChange(m, true)) tools.append(toolButton("Delete", () => confirmDelete(m)));
  head.append(tools);
  art.append(head);

  if (m.kind === "file" && m.file) art.append(fileNode(m.file));
  if (text) art.append(el("pre", "msg-body", text));
  return art;
}

// toolButton creates a small action button (Copy, Edit, Delete).
function toolButton(label, onClick) {
  const b = el("button", "tool", label);
  b.type = "button";
  b.addEventListener("click", () => onClick(b));
  return b;
}

// fileNode shows a file's name, size and Download link.
function fileNode(f) {
  const box = el("div", "file");
  const info = el("div");
  info.append(el("div", "file-name", f.name), el("div", "file-size", formatSize(f.size)));
  const a = el("a", "btn quiet", "Download");
  a.href = `/api/files/${f.id}`;
  a.download = f.name;
  box.append(info, a);
  return box;
}

// formatTime shows the time for today's messages, date and time otherwise.
function formatTime(iso) {
  const d = new Date(iso);
  const now = new Date();
  const t = d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  if (d.toDateString() === now.toDateString()) return t;
  return `${d.toLocaleDateString([], { day: "numeric", month: "short" })}, ${t}`;
}

// formatSize formats a byte count: "57 bytes", "12 KB", "1.5 MB".
function formatSize(n) {
  if (n >= 1 << 20) return `${Math.round((n / (1 << 20)) * 10) / 10} MB`;
  if (n >= 1 << 10) return `${Math.round(n / (1 << 10))} KB`;
  return `${n} bytes`;
}

// copyText copies text to the clipboard, with a fallback for browsers that
// refuse the Clipboard API, and confirms on the button for 1.5 s.
async function copyText(text, button) {
  try {
    await navigator.clipboard.writeText(text);
  } catch (e) {
    const ta = el("textarea");
    ta.value = text;
    document.body.append(ta);
    ta.select();
    document.execCommand("copy");
    ta.remove();
  }
  button.textContent = "Copied";
  button.classList.add("copied");
  setTimeout(() => {
    button.textContent = "Copy";
    button.classList.remove("copied");
  }, 1500);
}

// startEdit replaces a message's text with an editor in place. While it is
// open, render() leaves this message alone.
function startEdit(m) {
  const node = chat.nodes.get(m.id);
  if (!node || chat.editing.has(m.id)) return;
  chat.editing.add(m.id);
  const body = $(".msg-body", node);
  const box = el("div", "edit-box");
  const ta = el("textarea");
  ta.value = m.body || "";
  ta.spellcheck = false;
  ta.setAttribute("aria-label", m.kind === "file" ? "File caption" : "Message");
  const actions = el("div", "edit-actions");
  const cancel = el("button", "btn quiet", "Cancel");
  cancel.type = "button";
  const save = el("button", "btn", "Save");
  save.type = "button";
  actions.append(cancel, save);
  box.append(ta, actions);
  if (body) body.replaceWith(box);
  else node.append(box);
  ta.focus();

  const finish = () => {
    chat.editing.delete(m.id);
    const latest = chat.messages.find((x) => x.id === m.id);
    if (latest) {
      const fresh = messageNode(latest, messageVersion(latest));
      node.replaceWith(fresh);
      chat.nodes.set(m.id, fresh);
    }
  };
  cancel.addEventListener("click", finish);
  ta.addEventListener("keydown", (ev) => {
    if (ev.key === "Escape") finish();
    if (ev.key === "Enter" && (ev.ctrlKey || ev.metaKey)) save.click();
  });
  save.addEventListener("click", async () => {
    save.disabled = true;
    try {
      const updated = await api("PUT", `/api/messages/${m.id}`, { text: ta.value });
      const i = chat.messages.findIndex((x) => x.id === m.id);
      if (i >= 0) chat.messages[i] = updated;
      finish();
      toast("Message updated.");
      refreshSoon();
    } catch (e) {
      save.disabled = false;
      toast(e.message);
      if (e.status === 404) finish();
    }
  });
}

// confirmDelete asks for confirmation in the #dlg-delete dialog, whose
// buttons set dialog.returnValue to "delete" or "cancel".
function confirmDelete(m) {
  const dlg = $("#dlg-delete");
  dlg.returnValue = "";
  dlg.showModal();
  dlg.addEventListener("close", async () => {
    if (dlg.returnValue !== "delete") return;
    try {
      await api("DELETE", `/api/messages/${m.id}`);
      toast("Message deleted.");
    } catch (e) {
      toast(e.message);
    }
    refreshSoon();
  }, { once: true });
}

// ---------- Composer ----------

// The message box. Enter adds a line (scripts are multi-line); Ctrl+Enter
// or Cmd+Enter sends.
const compose = $("#compose");

// autosize grows the message box with its content, up to 40% of the window.
function autosize() {
  compose.style.height = "auto";
  compose.style.height = `${Math.min(compose.scrollHeight + 2, window.innerHeight * 0.4)}px`;
}
compose.addEventListener("input", autosize);
compose.addEventListener("keydown", (ev) => {
  if (ev.key === "Enter" && (ev.ctrlKey || ev.metaKey)) {
    ev.preventDefault();
    $("#form-send").requestSubmit();
  }
});

$("#form-send").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const text = compose.value;
  if (!text.trim()) return;
  const btn = $("#btn-send");
  btn.disabled = true;
  try {
    await api("POST", "/api/messages", { text });
    compose.value = "";
    autosize();
    refreshSoon();
  } catch (e) {
    toast(e.message);
  } finally {
    btn.disabled = false;
    compose.focus();
  }
});

$("#btn-attach").addEventListener("click", () => $("#file-input").click());
$("#file-input").addEventListener("change", (ev) => {
  const f = ev.target.files[0];
  ev.target.value = "";
  if (f) uploadFile(f);
});

// uploadFile sends a file with the message box text as its caption. The
// size is checked here first to give immediate feedback; the server checks
// it again.
async function uploadFile(file) {
  if (file.size > app.state.maxFileSize) {
    toast(`${file.name} is larger than the ${formatSize(app.state.maxFileSize)} limit.`);
    return;
  }
  const fd = new FormData();
  const caption = compose.value;
  if (caption.trim()) fd.append("caption", caption);
  fd.append("file", file, file.name);
  toast(`Uploading ${file.name}…`);
  try {
    await api("POST", "/api/files", fd);
    if (caption.trim()) {
      compose.value = "";
      autosize();
    }
    toast(`Shared ${file.name}.`);
    refreshSoon();
  } catch (e) {
    toast(e.message);
  }
}

// Drag and drop anywhere on the chat. dragenter/dragleave fire for every
// child element crossed, so a counter tells when the file really left.
let dragDepth = 0;
const hasFiles = (ev) => ev.dataTransfer && [...ev.dataTransfer.types].includes("Files");
window.addEventListener("dragenter", (ev) => {
  if (!chat.active || !hasFiles(ev)) return;
  ev.preventDefault();
  dragDepth++;
  $("#drop").hidden = false;
});
window.addEventListener("dragover", (ev) => {
  if (chat.active && hasFiles(ev)) ev.preventDefault();
});
window.addEventListener("dragleave", () => {
  if (--dragDepth <= 0) {
    dragDepth = 0;
    $("#drop").hidden = true;
  }
});
window.addEventListener("drop", (ev) => {
  if (!chat.active || !hasFiles(ev)) return;
  ev.preventDefault();
  dragDepth = 0;
  $("#drop").hidden = true;
  const f = ev.dataTransfer.files[0];
  if (f) uploadFile(f);
});

// ---------- Admin ----------

// updateAdminUI shows the Admin button to the admin account only, and the
// Admin badge while admin rights are unlocked.
function updateAdminUI() {
  const admin = !!app.state.admin;
  $("#btn-admin").hidden = !app.state.isAdminUser;
  $("#admin-badge").hidden = !admin;
  $("#btn-admin").textContent = admin ? "Admin tools" : "Admin";
}

// adminChanged reloads the state after admin rights changed and redraws
// the messages, since their Edit/Delete buttons depend on those rights.
async function adminChanged() {
  await refreshState();
  updateAdminUI();
  render(chat.messages);
}

// Buttons marked data-close close their dialog.
for (const b of document.querySelectorAll("dialog [data-close]")) {
  b.addEventListener("click", () => b.closest("dialog").close());
}

$("#btn-admin").addEventListener("click", () => {
  if (app.state.admin) {
    resetRotation();
    $("#dlg-admin-tools").showModal();
  } else {
    const form = $("#form-admin");
    form.reset();
    setError(form, "");
    $("#dlg-admin").showModal();
  }
});

$("#form-admin").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const form = ev.currentTarget;
  setError(form, "");
  busy(form, true);
  try {
    await api("POST", "/api/admin/login", { password: $("#admin-password").value });
    $("#dlg-admin").close();
    form.reset();
    await adminChanged();
    toast("Admin unlocked. You can edit and delete any message.");
  } catch (e) {
    setError(form, attemptsMessage(e));
    $("#admin-password").select();
  } finally {
    busy(form, false);
  }
});

$("#btn-admin-lock").addEventListener("click", async () => {
  try {
    await api("POST", "/api/admin/logout");
    $("#dlg-admin-tools").close();
    await adminChanged();
    toast("Admin locked.");
  } catch (e) {
    toast(e.message);
  }
});

// resetRotation returns the admin dialog to its first step. Replacing the
// admin's own code is a two-step flow: "Replace my QR code" only shows a
// new code; nothing changes until "Switch and sign me out".
function resetRotation() {
  $("#rotate-step-1").hidden = false;
  $("#rotate-step-2").hidden = true;
  $("#rotate-qr-img").removeAttribute("src");
  $("#rotate-secret").textContent = "";
  setError($("#dlg-admin-tools"), "");
}

$("#btn-rotate").addEventListener("click", async (ev) => {
  const btn = ev.currentTarget;
  btn.disabled = true;
  try {
    const enr = await api("POST", "/api/admin/rotate");
    $("#rotate-qr-img").src = enr.qr;
    $("#rotate-secret").textContent = enr.secret;
    $("#rotate-step-1").hidden = true;
    $("#rotate-step-2").hidden = false;
  } catch (e) {
    toast(e.message);
  } finally {
    btn.disabled = false;
  }
});

$("#btn-rotate-cancel").addEventListener("click", async () => {
  try { await api("POST", "/api/admin/rotate/cancel"); } catch (e) { /* nothing changed */ }
  $("#dlg-admin-tools").close();
  toast("Your current code is still valid.");
});

$("#btn-rotate-confirm").addEventListener("click", async (ev) => {
  const btn = ev.currentTarget;
  btn.disabled = true;
  try {
    await api("POST", "/api/admin/rotate/confirm");
    resetRotation();
    $("#dlg-admin-tools").close();
    signedOut("Your admin code was replaced. Sign in with the new code from your authenticator app.");
  } catch (e) {
    setError($("#dlg-admin-tools"), e.message);
  } finally {
    btn.disabled = false;
  }
});

$("#dlg-admin-tools").addEventListener("close", () => {
  // Closing with Escape while a new code is shown keeps the current one.
  if (!$("#rotate-step-2").hidden && app.state.authenticated) {
    api("POST", "/api/admin/rotate/cancel").catch(() => {});
  }
  resetRotation();
});

// ---------- Users ----------
//
// The admin's user list (Admin tools → Users). A new user, or a new QR
// code for an existing one, comes back with its QR code exactly once:
// #dlg-user-qr shows it, and closing the dialog removes it from the page.

const users = { list: [], count: 0, max: 0 };

$("#btn-users").addEventListener("click", async () => {
  $("#dlg-admin-tools").close();
  const form = $("#form-add-user");
  form.reset();
  setError(form, "");
  $("#dlg-users").showModal();
  await loadUsers();
});

// loadUsers fetches the list and redraws it.
async function loadUsers() {
  try {
    const r = await api("GET", "/api/admin/users");
    users.list = r.users;
    users.count = r.count;
    users.max = r.max;
    renderUsers();
  } catch (e) {
    if (e.code === "admin_required") $("#dlg-users").close();
    toast(e.message);
  }
}

// statusText describes a user's status, with the deadline of a pending
// QR code.
function statusText(u) {
  switch (u.status) {
    case "active": return "Active";
    case "pending": return `Waiting for first sign-in, until ${formatDeadline(u.enrolExpiresAt)}`;
    case "expired": return "QR code expired";
    case "revoked": return "Revoked";
  }
  return u.status;
}

// formatDeadline shows a date and time such as "Fri 6 Nov, 14:05".
function formatDeadline(iso) {
  const d = new Date(iso);
  return `${d.toLocaleDateString([], { weekday: "short", day: "numeric", month: "short" })}, ${d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}`;
}

function renderUsers() {
  $("#users-count").textContent = `${users.count} of ${users.max} users. Each person signs in with their own QR code.`;
  const full = users.count >= users.max;
  $("#new-username").disabled = full;
  $("#btn-add-user").disabled = full;
  const limit = $("#users-limit");
  limit.hidden = !full;
  limit.textContent = full ? `The limit of ${users.max} users is reached. Delete a user to add someone else, or raise max_users in the server configuration.` : "";

  const items = users.list.map((u) => {
    const li = el("li", "user");
    li.dataset.name = u.name;
    const main = el("div", "user-main");
    const name = el("div", "user-name", u.name);
    if (u.admin) name.append(el("span", "badge", "Admin"));
    main.append(name);
    const meta = el("div", "user-meta");
    meta.append(el("span", `user-status ${u.status}`, statusText(u)));
    const extra = [];
    if (u.lastLoginAt) extra.push(`last sign-in ${formatTime(u.lastLoginAt)}`);
    if (u.sessions) extra.push(`${u.sessions} ${u.sessions === 1 ? "session" : "sessions"} open`);
    if (extra.length) meta.append(document.createTextNode(` · ${extra.join(" · ")}`));
    main.append(meta);
    li.append(main);

    if (!u.admin) {
      const actions = el("div", "user-actions");
      actions.append(userButton("New QR code", "btn quiet", () => newUserQR(u)));
      if (u.status === "active" || u.status === "pending") {
        actions.append(userButton("Revoke", "btn quiet danger-text", () => revokeUser(u)));
      }
      actions.append(userButton("Delete", "btn quiet danger-text", () => deleteUser(u)));
      li.append(actions);
    }
    return li;
  });
  $("#user-list").replaceChildren(...items);
}

function userButton(label, cls, onClick) {
  const b = el("button", cls, label);
  b.type = "button";
  b.addEventListener("click", onClick);
  return b;
}

$("#form-add-user").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const form = ev.currentTarget;
  setError(form, "");
  busy(form, true);
  try {
    const r = await api("POST", "/api/admin/users", { name: $("#new-username").value });
    form.reset();
    await loadUsers();
    showUserQR(r.user, r.enrollment);
  } catch (e) {
    setError(form, e.message);
  } finally {
    busy(form, false);
    renderUsers(); // restores the limit's disabled state
  }
});

// confirmUser asks for confirmation in #dlg-user-confirm and resolves to
// true when the admin confirms.
function confirmUser(title, text, action) {
  const dlg = $("#dlg-user-confirm");
  $("#user-confirm-title").textContent = title;
  $("#user-confirm-text").textContent = text;
  $("#user-confirm-btn").textContent = action;
  dlg.returnValue = "";
  dlg.showModal();
  return new Promise((resolve) => {
    dlg.addEventListener("close", () => resolve(dlg.returnValue === "confirm"), { once: true });
  });
}

async function newUserQR(u) {
  const signedIn = u.status === "active" || u.status === "pending";
  const ok = await confirmUser(`New QR code for ${u.name}?`,
    signedIn
      ? `${u.name}'s current code stops working at once and ${u.name} is signed out. The new code must be used within 24 hours.`
      : `The new code must be used within 24 hours.`,
    "Create new QR code");
  if (!ok) return;
  try {
    const r = await api("POST", `/api/admin/users/${u.id}/qr`);
    await loadUsers();
    showUserQR(r.user, r.enrollment);
  } catch (e) {
    toast(e.message);
    loadUsers();
  }
}

async function revokeUser(u) {
  const ok = await confirmUser(`Revoke ${u.name}'s access?`,
    `${u.name} is signed out and their code stops working. Their messages stay. You can give them a new QR code later.`,
    "Revoke");
  if (!ok) return;
  try {
    await api("POST", `/api/admin/users/${u.id}/revoke`);
    toast(`${u.name} can no longer sign in.`);
  } catch (e) {
    toast(e.message);
  }
  loadUsers();
}

async function deleteUser(u) {
  const ok = await confirmUser(`Delete ${u.name}?`,
    `${u.name} is signed out and removed. Their messages stay in the chat, marked as from a deleted user. The name can be used again.`,
    "Delete");
  if (!ok) return;
  try {
    await api("DELETE", `/api/admin/users/${u.id}`);
    toast(`${u.name} was removed.`);
  } catch (e) {
    toast(e.message);
  }
  loadUsers();
}

// showUserQR shows a user's new QR code. It is the only time it is shown.
function showUserQR(user, enr) {
  $("#user-qr-title").textContent = `QR code for ${user.name}`;
  $("#user-qr-img").src = enr.qr;
  $("#user-qr-img").alt = `QR code for ${user.name}'s cpsync account`;
  $("#user-qr-secret").textContent = enr.secret;
  $("#user-qr-deadline").textContent = `${user.name} signs in with the username "${user.name}" and must do so before ${formatDeadline(user.enrolExpiresAt)}, or the code expires.`;
  $("#dlg-user-qr").showModal();
}

// Closing the dialog, however it is closed, removes the code from the page.
$("#dlg-user-qr").addEventListener("close", () => {
  $("#user-qr-img").removeAttribute("src");
  $("#user-qr-secret").textContent = "";
  $(".secret", $("#dlg-user-qr")).open = false;
});

// ---------- Toast ----------

// toast shows a short message at the bottom of the page for 4 seconds.
let toastTimer;
function toast(msg) {
  const el = $("#toast");
  el.textContent = msg;
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.hidden = true; }, 4000);
}

// Start: show the screen that matches the server state.
route();

// Theme switch: System / Light / Dark, saved in localStorage under "theme".
// The inline script in layout.html applies a saved choice before the page paints.
(function () {
  const root = document.documentElement;

  function saved() {
    try {
      const t = localStorage.getItem("theme");
      return t === "light" || t === "dark" ? t : "system";
    } catch (e) {
      return "system";
    }
  }

  function sync() {
    const current = saved();
    document.querySelectorAll("[data-theme-choice]").forEach((b) => {
      b.setAttribute("aria-pressed", String(b.dataset.themeChoice === current));
    });
  }

  const themeSurfaces = { light: "#FAF9FC", dark: "#14111C" };

  function syncThemeColor() {
    const explicit = root.getAttribute("data-theme");
    const metas = Array.from(document.querySelectorAll('meta[name="theme-color"]'));
    if (explicit === "light" || explicit === "dark") {
      const color = themeSurfaces[explicit];
      const meta = metas[0] || document.head.appendChild(document.createElement("meta"));
      meta.setAttribute("name", "theme-color");
      meta.setAttribute("content", color);
      meta.removeAttribute("media");
      metas.slice(1).forEach((extra) => extra.remove());
      return;
    }
    const light = metas[0] || document.head.appendChild(document.createElement("meta"));
    light.setAttribute("name", "theme-color");
    light.setAttribute("content", themeSurfaces.light);
    light.setAttribute("media", "(prefers-color-scheme: light)");
    let dark = metas[1];
    if (!dark) {
      dark = document.createElement("meta");
      light.after(dark);
    }
    dark.setAttribute("name", "theme-color");
    dark.setAttribute("content", themeSurfaces.dark);
    dark.setAttribute("media", "(prefers-color-scheme: dark)");
    metas.slice(2).forEach((extra) => extra.remove());
  }

  function choose(choice) {
    try {
      if (choice === "system") localStorage.removeItem("theme");
      else localStorage.setItem("theme", choice);
    } catch (e) {}
    if (choice === "light" || choice === "dark") root.setAttribute("data-theme", choice);
    else root.removeAttribute("data-theme");
    sync();
    syncThemeColor();
  }

  document.addEventListener("click", (e) => {
    const b = e.target.closest("[data-theme-choice]");
    if (b) choose(b.dataset.themeChoice);
  });

  function reveal() {
    document.querySelectorAll(".theme-switch").forEach((el) => (el.hidden = false));
    sync();
    syncThemeColor();
  }

  document.addEventListener("DOMContentLoaded", reveal);
  document.addEventListener("htmx:afterSettle", reveal);
  document.addEventListener("htmx:historyRestore", reveal);
  window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => {
    if (!root.getAttribute("data-theme")) syncThemeColor();
  });
})();

// The magic-link page posts itself. GET does not use the token, so a mail
// scanner that only opens the link cannot sign in. The button stays for
// browsers without JavaScript. A body swap can bring the form in later.
function autosubmitForms(root) {
  (root || document).querySelectorAll("form[data-autosubmit]:not([data-autosubmit-started])").forEach((form) => {
    form.dataset.autosubmitStarted = "1";
    const button = form.querySelector("[type=submit]");
    if (button) button.setAttribute("aria-busy", "true");
    form.requestSubmit();
  });
}

// A closed password field must not be submitted with the email-link form.
// Listeners stay on document so a swapped sign-in form still toggles.
function syncPasswordToggle(toggle) {
  const form = toggle.form;
  const input = form?.querySelector("[data-auth-password]");
  if (!form || !input) return;
  const linkButton = form.querySelector(".auth-link-submit");
  const passwordButton = form.querySelector(".auth-password-submit");
  const passwordMode = toggle.checked;
  input.disabled = !passwordMode;
  // Enter and the mobile keyboard submit the first submit button.
  // Password mode has to be that button, or the press sends an email link.
  if (linkButton && passwordButton) {
    form.action = passwordMode ? "/signin" : "/signin/link";
    if (passwordMode) linkButton.before(passwordButton);
    else passwordButton.before(linkButton);
  }
}

function syncPasswordToggles(root) {
  (root || document).querySelectorAll(".auth-password-toggle").forEach(syncPasswordToggle);
}

document.addEventListener("change", (e) => {
  const toggle = e.target.closest?.(".auth-password-toggle");
  if (!toggle) return;
  if (!toggle.checked) {
    const input = toggle.form?.querySelector("[data-auth-password]");
    if (input) input.value = "";
  }
  syncPasswordToggle(toggle);
});

// A plain form post of a large file gives no feedback until it finishes.
document.addEventListener("submit", (e) => {
  const label = e.target.dataset?.busy;
  const button = e.target.querySelector?.("[type=submit]");
  if (!label || !button) return;
  button.setAttribute("aria-busy", "true");
  button.textContent = label;
});

document.addEventListener("click", (e) => {
  const opener = e.target.closest("[data-open-dialog]");
  if (opener) document.getElementById(opener.dataset.openDialog)?.showModal();
  const closer = e.target.closest("[data-close-dialog]");
  if (closer) closer.closest("dialog")?.close();
});

document.addEventListener("click", async (e) => {
  const copy = e.target.closest("[data-copy]");
  if (copy) {
    const label = copy.querySelector("span") || copy;
    const source = document.getElementById(copy.dataset.copy);
    try {
      await navigator.clipboard.writeText(source.textContent.trim());
      label.textContent = "Copied!";
    } catch (err) {
      // Some browsers refuse the async clipboard; a selection still copies.
      const range = document.createRange();
      range.selectNodeContents(source);
      getSelection().removeAllRanges();
      getSelection().addRange(range);
      label.textContent = document.execCommand("copy") ? "Copied!" : "Link selected";
    }
    setTimeout(() => (label.textContent = "Copy link"), 2000);
  }
  if (e.target.closest("[data-print]")) window.print();
});

// Times are rendered in UTC; show them in the viewer's time zone.
const timeFormats = {
  datetime: { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" },
  time: { hour: "numeric", minute: "2-digit" },
  date: { month: "short", day: "numeric", year: "numeric" },
};

function localizeTimes(root) {
  root.querySelectorAll("time[data-local]").forEach((el) => {
    const d = new Date(el.dateTime);
    if (!isNaN(d)) el.textContent = d.toLocaleString([], timeFormats[el.dataset.local] || timeFormats.datetime);
  });
  // A datetime-local field shows the device's time zone. Its hidden partner
  // (data-utc-field) carries the same moment in UTC to the server.
  root.querySelectorAll("input[data-utc]:not([data-localized])").forEach((input) => {
    input.dataset.localized = "1";
    const d = new Date(input.dataset.utc);
    if (input.dataset.utc && !isNaN(d)) {
      input.value = new Date(d.getTime() - d.getTimezoneOffset() * 60000).toISOString().slice(0, 16);
    }
  });
}

document.addEventListener("submit", (e) => {
  e.target.querySelectorAll?.("input[data-utc-field]").forEach((input) => {
    const hidden = e.target.elements[input.dataset.utcField];
    const d = new Date(input.value);
    if (hidden) hidden.value = input.value && !isNaN(d) ? d.toISOString() : "";
  });
});

document.addEventListener("DOMContentLoaded", () => localizeTimes(document));

// The host page polls its queue. A refresh would close an open dialog or
// snap a dragged row back, so it waits.
window.djmShouldPoll = () =>
  !document.hidden && !document.documentElement.dataset.reordering && !document.querySelector("dialog[open]");

// Polling replaces <details> elements; keep the ones the host opened open.
let openDetails = [];
document.addEventListener("htmx:beforeSwap", () => {
  openDetails = [...document.querySelectorAll("details[id][open]")].map((d) => d.id);
});
document.addEventListener("htmx:afterSwap", () => {
  openDetails.forEach((id) => document.getElementById(id)?.setAttribute("open", ""));
});

// Turnstile tokens are single-use. A failed submit that stays on the form, including
// an HTMX swap of the same form, needs a new widget before the next attempt.
// The API script is deferred, so it calls onTurnstileLoad instead of turnstile.ready.
function renderTurnstile(root) {
  if (!window.turnstile || !root || !root.querySelectorAll) return;
  root.querySelectorAll(".cf-turnstile").forEach((el) => {
    if (el.dataset.turnstileMounted === "1") return;
    const sitekey = el.getAttribute("data-sitekey");
    if (!sitekey) return;
    el.dataset.turnstileMounted = "1";
    window.turnstile.render(el, { sitekey: sitekey, theme: "auto" });
  });
}

window.onTurnstileLoad = function () {
  renderTurnstile(document);
};

// The Turnstile API script lives in the sign-in page's head. A boosted visit
// swaps the body and does not load that script, so add it when a widget arrives.
function ensureTurnstile(root) {
  const scope = root && root.querySelector ? root : document;
  if (!scope.querySelector(".cf-turnstile")) return;
  if (window.turnstile) {
    renderTurnstile(scope);
    return;
  }
  if (document.querySelector("script[data-turnstile-api], script[src*='challenges.cloudflare.com/turnstile']")) return;
  const script = document.createElement("script");
  script.src = "https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit&onload=onTurnstileLoad";
  script.defer = true;
  script.dataset.turnstileApi = "1";
  document.head.appendChild(script);
}

// Boost keeps the current head and only copies the title. The body carries
// the new page's tags so a swap or a history restore can put them back.
function applyPageMeta() {
  const tpl = document.getElementById("page-meta");
  if (!tpl || !tpl.content) return;
  const title = tpl.getAttribute("data-title");
  if (title) document.title = title;
  const selector = 'meta[name="description"], meta[name="robots"], link[rel="canonical"], meta[property^="og:"], meta[name^="twitter:"]';
  const incoming = tpl.content.querySelectorAll(selector);
  if (!incoming.length) return;
  document.head.querySelectorAll(selector).forEach((el) => el.remove());
  incoming.forEach((el) => document.head.appendChild(el.cloneNode(true)));
}

function settlePage(root) {
  const scope = root && root.querySelectorAll ? root : document;
  applyPageMeta();
  ensureTurnstile(scope);
  localizeTimes(document);
  syncPasswordToggles(scope);
  autosubmitForms(scope);
}

document.addEventListener("htmx:afterSettle", (event) => {
  settlePage(event.detail?.target || document);
});
document.addEventListener("htmx:historyRestore", () => {
  settlePage(document);
});

autosubmitForms(document);
syncPasswordToggles(document);

// Header links collapse into this button below 768px. Desktop never shows it.
// Look the nodes up on each event: a body swap replaces the header.
(function () {
  const toggleEl = () => document.querySelector("nav.site-nav .nav-toggle");
  const menuEl = () => document.getElementById("site-menu");
  const navEl = () => toggleEl()?.closest("nav");

  const setOpen = (open) => {
    const toggle = toggleEl();
    const menu = menuEl();
    if (!toggle || !menu) return;
    toggle.setAttribute("aria-expanded", open ? "true" : "false");
    menu.classList.toggle("is-open", open);
  };

  document.addEventListener("click", (e) => {
    const toggle = toggleEl();
    const menu = menuEl();
    const nav = navEl();
    if (!toggle || !menu || !nav) return;
    if (toggle.contains(e.target)) {
      setOpen(toggle.getAttribute("aria-expanded") !== "true");
      return;
    }
    // Hide after the click, not during it. Hiding the link in the click handler
    // cancels navigation, including the log-out submit.
    if (menu.contains(e.target) && e.target.closest("a, button")) {
      setTimeout(() => setOpen(false), 0);
      return;
    }
    if (toggle.getAttribute("aria-expanded") === "true" && !nav.contains(e.target)) setOpen(false);
  });

  document.addEventListener("keydown", (e) => {
    const toggle = toggleEl();
    if (!toggle || e.key !== "Escape" || toggle.getAttribute("aria-expanded") !== "true") return;
    setOpen(false);
    toggle.focus();
  });

  document.addEventListener("focusin", (e) => {
    const toggle = toggleEl();
    const nav = navEl();
    if (!toggle || !nav || toggle.getAttribute("aria-expanded") !== "true") return;
    if (!nav.contains(e.target)) setOpen(false);
  });

  window.matchMedia("(min-width: 768px)").addEventListener("change", (e) => {
    if (e.matches) setOpen(false);
  });
})();

// CSRF for HTMX and fetch. The token lives in a meta tag inside <body>, so a
// boosted navigation that swaps the body picks up a rotated token. The listener
// stays on document and is not registered per control.
(function () {
  const SAFE = { GET: 1, HEAD: 1, OPTIONS: 1, TRACE: 1 };

  function token() {
    const tags = document.querySelectorAll('meta[name="csrf-token"]');
    const meta = tags[tags.length - 1];
    return meta ? meta.getAttribute("content") || "" : "";
  }

  function sameOrigin(url) {
    try {
      return new URL(url, window.location.href).origin === window.location.origin;
    } catch (e) {
      return false;
    }
  }

  function show(message) {
    const text = String(message || "").replace(/\s+/g, " ").trim().slice(0, 300)
      || "Reload the page and try again.";
    let banner = document.getElementById("csrf-error");
    if (!banner) {
      banner = document.createElement("p");
      banner.id = "csrf-error";
      banner.setAttribute("role", "alert");
      banner.className = "error";
      const main = document.getElementById("content") || document.body;
      main.prepend(banner);
    }
    banner.textContent = text;
  }

  window.djmCSRF = {
    token: token,
    sameOrigin: sameOrigin,
    // headers for a same-origin state-changing fetch. Cross-origin URLs are left alone.
    headers: function (url, headers) {
      const next = Object.assign({}, headers);
      if (sameOrigin(url)) {
        const value = token();
        if (value) next["X-CSRF-Token"] = value;
      }
      return next;
    },
    show: show,
  };

  document.addEventListener("htmx:configRequest", (event) => {
    const detail = event.detail || {};
    const verb = String(detail.verb || "GET").toUpperCase();
    if (SAFE[verb] || !sameOrigin(detail.path || "")) return;
    const value = token();
    if (!value) return;
    detail.headers["X-CSRF-Token"] = value;
  });

  document.addEventListener("htmx:beforeSwap", (event) => {
    const xhr = event.detail && event.detail.xhr;
    if (!xhr || xhr.getResponseHeader("X-CSRF-Rejected") !== "1") return;
    event.detail.shouldSwap = false;
    show("Invalid or missing CSRF token. Reload the page and try again.");
  });

  document.addEventListener("htmx:responseError", (event) => {
    const xhr = event.detail && event.detail.xhr;
    if (!xhr || xhr.status !== 403 || xhr.getResponseHeader("X-CSRF-Rejected") === "1") return;
    show(xhr.responseText);
  });
})();

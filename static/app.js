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

  function choose(choice) {
    try {
      if (choice === "system") localStorage.removeItem("theme");
      else localStorage.setItem("theme", choice);
    } catch (e) {}
    if (choice === "light" || choice === "dark") root.setAttribute("data-theme", choice);
    else root.removeAttribute("data-theme");
    sync();
  }

  document.addEventListener("click", (e) => {
    const b = e.target.closest("[data-theme-choice]");
    if (b) choose(b.dataset.themeChoice);
  });

  document.addEventListener("DOMContentLoaded", () => {
    document.querySelectorAll(".theme-switch").forEach((el) => (el.hidden = false));
    sync();
  });
})();

// The magic-link page posts itself. GET does not use the token, so a mail
// scanner that only opens the link cannot sign in. The button stays for
// browsers without JavaScript.
document.querySelectorAll("form[data-autosubmit]").forEach((form) => {
  const button = form.querySelector("[type=submit]");
  if (button) button.setAttribute("aria-busy", "true");
  form.requestSubmit();
});

// A closed password field must not be submitted with the email-link form.
document.querySelectorAll(".auth-password-toggle").forEach((toggle) => {
  const form = toggle.form;
  const input = form?.querySelector("[data-auth-password]");
  if (!form || !input) return;
  const linkButton = form.querySelector(".auth-link-submit");
  const passwordButton = form.querySelector(".auth-password-submit");
  const sync = () => {
    const passwordMode = toggle.checked;
    input.disabled = !passwordMode;
    // Enter and the mobile keyboard submit the first submit button.
    // Password mode has to be that button, or the press sends an email link.
    if (linkButton && passwordButton) {
      form.action = passwordMode ? "/signin" : "/signin/link";
      if (passwordMode) linkButton.before(passwordButton);
      else passwordButton.before(linkButton);
    }
  };
  toggle.addEventListener("change", () => {
    if (!toggle.checked) input.value = "";
    sync();
  });
  sync();
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

document.addEventListener("htmx:afterSettle", (event) => {
  renderTurnstile(event.detail?.target || document);
  localizeTimes(document);
});

// Header links collapse into this button below 768px. Desktop never shows it.
(function () {
  const toggle = document.querySelector("nav.site-nav .nav-toggle");
  const menu = document.getElementById("site-menu");
  if (!toggle || !menu) return;
  const nav = toggle.closest("nav");

  const setOpen = (open) => {
    toggle.setAttribute("aria-expanded", open ? "true" : "false");
    menu.classList.toggle("is-open", open);
  };

  toggle.addEventListener("click", () => {
    setOpen(toggle.getAttribute("aria-expanded") !== "true");
  });

  // Hide after the click, not during it. Hiding the link in the click handler
  // cancels navigation, including the log-out submit.
  menu.addEventListener("click", (e) => {
    if (!e.target.closest("a, button")) return;
    setTimeout(() => setOpen(false), 0);
  });

  document.addEventListener("click", (e) => {
    if (toggle.getAttribute("aria-expanded") !== "true") return;
    if (!nav.contains(e.target)) setOpen(false);
  });

  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && toggle.getAttribute("aria-expanded") === "true") {
      setOpen(false);
      toggle.focus();
    }
  });

  document.addEventListener("focusin", (e) => {
    if (toggle.getAttribute("aria-expanded") !== "true") return;
    if (!nav.contains(e.target)) setOpen(false);
  });

  window.matchMedia("(min-width: 768px)").addEventListener("change", (e) => {
    if (e.matches) setOpen(false);
  });
})();

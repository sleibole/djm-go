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
});

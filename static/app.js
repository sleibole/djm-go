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

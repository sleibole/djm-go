// Install help for the shared footer. The head script stashes
// beforeinstallprompt before this file runs; state stays here so a body
// swap does not lose it or register the browser listeners again.
// Nothing is written to storage: an installed flag would survive uninstall.
(function (root, factory) {
  const api = factory();
  if (typeof module === "object" && module.exports) module.exports = api;
  if (root && root.document) {
    root.djmInstall = api.createInstall({ window: root, document: root.document });
    root.djmInstall.start();
  }
})(typeof window !== "undefined" ? window : globalThis, function () {
  const installedDisplayModes = ["standalone", "fullscreen", "minimal-ui", "window-controls-overlay", "tabbed"];

  function isInstalledDisplay(win) {
    const nav = (win && win.navigator) || {};
    if (nav.standalone === true) return true;
    if (!win || typeof win.matchMedia !== "function") return false;
    for (let i = 0; i < installedDisplayModes.length; i++) {
      try {
        if (win.matchMedia("(display-mode: " + installedDisplayModes[i] + ")").matches) return true;
      } catch (e) {}
    }
    return false;
  }

  // iOS is checked before Chrome/Android. Chrome on iPhone includes CriOS,
  // and iPadOS can report a Macintosh user agent with a touch screen.
  function detectPlatform(nav) {
    nav = nav || {};
    const ua = String(nav.userAgent || "");
    const platform = String(nav.platform || "");
    const touch = Number(nav.maxTouchPoints) || 0;
    const uaPlatform = nav.userAgentData ? String(nav.userAgentData.platform || "") : "";
    const iosDevice = /iPad|iPhone|iPod/i.test(ua) || platform === "iPhone" || platform === "iPad" || platform === "iPod";
    const desktopUAData = uaPlatform === "macOS" || uaPlatform === "Windows" || uaPlatform === "Linux" || uaPlatform === "Chrome OS";
    const iPadDesktop = touch > 1 && (platform === "MacIntel" || /Macintosh/i.test(ua)) && !desktopUAData && uaPlatform !== "Android";
    if (iosDevice || iPadDesktop || uaPlatform === "iOS") return "ios";
    if (uaPlatform === "Android" || /Android/i.test(ua)) return "android";
    if (/Windows|Macintosh|Linux|CrOS|X11/i.test(ua) || desktopUAData) return "desktop";
    return "unknown";
  }

  function inAppBrowser(nav) {
    const ua = String((nav && nav.userAgent) || "");
    return /Instagram|FBAN|FBAV|FB_IAB|FB4A|Musical_ly|\bTikTok\b|BytedanceWebview|Twitter for|LinkedInApp|\bSnapchat\b|Pinterest\/|Line\/|MicroMessenger|GSA\/|; wv\)/i.test(ua);
  }

  function sensitiveParam(name) {
    const n = String(name || "").toLowerCase().replace(/[^a-z0-9]/g, "");
    if (!n) return false;
    if ({
      token: 1, secret: 1, password: 1, passwd: 1, session: 1, magic: 1, auth: 1,
      credential: 1, credentials: 1, jwt: 1, otp: 1, code: 1, key: 1, ticket: 1,
      state: 1, apikey: 1, accesstoken: 1, refreshtoken: 1, idtoken: 1,
    }[n]) return true;
    return /token|secret|password|session|magic|credential|jwt|otp/.test(n);
  }

  function sensitivePath(pathname) {
    return /\/auth(?:\/|$)/i.test(String(pathname || ""));
  }

  // Home is the safe canonical URL. The current page is copied only when its
  // path and query cannot carry a sign-in token.
  function safePageURL(href) {
    let url;
    try {
      url = new URL(href);
    } catch (e) {
      return "";
    }
    const home = url.origin + "/";
    if (sensitivePath(url.pathname)) return home;
    for (const [name, value] of url.searchParams) {
      let decoded = value;
      try {
        decoded = decodeURIComponent(value);
      } catch (e) {}
      if (sensitiveParam(name) || sensitivePath(decoded)) return home;
    }
    return url.origin + url.pathname + url.search;
  }

  function focusEl(el) {
    if (!el || typeof el.focus !== "function") return;
    try {
      el.focus({ preventScroll: true });
    } catch (e) {
      el.focus();
    }
  }

  function createInstall(env) {
    const win = env.window;
    const doc = env.document;
    let promptEvent = null;
    let queuedEvent = null;
    let prompting = false;
    let sessionInstalled = false;
    let platformChoice = null;
    let statusText = "";
    let opener = null;
    let savedScroll = null;
    let started = false;

    function shouldHide() {
      return sessionInstalled || isInstalledDisplay(win);
    }

    function usablePrompt() {
      if (shouldHide()) return false;
      return !!(promptEvent && typeof promptEvent.prompt === "function" && !prompting);
    }

    function selectedPlatform() {
      if (platformChoice === "ios" || platformChoice === "android" || platformChoice === "desktop" || platformChoice === "unknown") {
        return platformChoice;
      }
      return detectPlatform(win.navigator);
    }

    function setStatus(text) {
      statusText = text || "";
      const status = doc.getElementById && doc.getElementById("install-app-status");
      if (status) status.textContent = statusText;
    }

    function promoteQueued() {
      if (!queuedEvent) return;
      promptEvent = queuedEvent;
      queuedEvent = null;
    }

    function dialog() {
      return doc.getElementById ? doc.getElementById("install-app-dialog") : null;
    }

    function placeManual(node, opening) {
      const manual = node.querySelector("[data-install-manual]");
      const direct = node.querySelector("[data-install-direct]");
      const details = node.querySelector("[data-install-other]");
      const mount = node.querySelector("[data-install-manual-mount]");
      if (!manual || !direct || !details || !mount) return;
      if (usablePrompt()) {
        const moved = manual.parentElement !== mount;
        if (moved) mount.appendChild(manual);
        details.hidden = false;
        if (opening) details.open = false;
        else if (moved && node.open) details.open = true;
        return;
      }
      if (manual.parentElement !== direct) direct.appendChild(manual);
      details.hidden = true;
    }

    function sync(opts) {
      const root = doc.documentElement;
      if (root && root.classList) root.classList.toggle("djm-install-hide", shouldHide());
      const node = dialog();
      if (!node) return;
      const native = node.querySelector("[data-install-native]");
      const promptButton = node.querySelector("[data-install-prompt]");
      const showNative = usablePrompt();
      const focused = promptButton && doc.activeElement === promptButton;
      if (native) native.hidden = !showNative;
      if (!showNative && focused) focusEl(node.querySelector("[data-close-dialog]"));
      placeManual(node, !!(opts && opts.opening));
      const platform = selectedPlatform();
      node.querySelectorAll("[data-install-panel]").forEach((panel) => {
        panel.hidden = panel.getAttribute("data-install-panel") !== platform;
      });
      node.querySelectorAll("[data-install-platform]").forEach((button) => {
        button.setAttribute("aria-pressed", button.getAttribute("data-install-platform") === platform ? "true" : "false");
      });
      const inapp = node.querySelector("[data-install-inapp]");
      if (inapp) inapp.hidden = !inAppBrowser(win.navigator);
      const status = doc.getElementById("install-app-status");
      if (status && status.textContent !== statusText) status.textContent = statusText;
    }

    function releaseScrollLock() {
      const root = doc.documentElement;
      if (root && root.style) root.style.paddingRight = "";
      if (savedScroll != null) {
        const y = savedScroll;
        savedScroll = null;
        if (win.scrollTo) win.scrollTo(0, y);
      }
    }

    // Chrome fires toggle on a later task and may not fire close at all.
    // Callers that already know the dialog is going away clean up immediately.
    function restorePage() {
      releaseScrollLock();
      const trigger = opener;
      opener = null;
      if (trigger && (!doc.contains || doc.contains(trigger))) focusEl(trigger);
    }

    function bindDialog() {
      const node = dialog();
      if (!node || node.dataset.installBound === "1") return;
      node.dataset.installBound = "1";
      const onClosed = () => {
        if (!node.open) restorePage();
      };
      node.addEventListener("toggle", onClosed);
      node.addEventListener("close", onClosed);
    }

    function closeInstallDialog() {
      const node = dialog();
      if (!node || !node.open) return;
      releaseScrollLock();
      opener = null;
      node.close();
    }

    function currentHref() {
      const loc = win.location;
      if (!loc) return "";
      return typeof loc === "string" ? loc : loc.href || "";
    }

    function copyWithSelection(url) {
      if (!doc.createElement || !doc.body || !doc.execCommand) return false;
      const area = doc.createElement("textarea");
      area.value = url;
      area.setAttribute("readonly", "");
      area.style.position = "fixed";
      area.style.top = "0";
      area.style.left = "0";
      area.style.width = "1px";
      area.style.height = "1px";
      area.style.opacity = "0";
      doc.body.appendChild(area);
      if (area.focus) focusEl(area);
      if (area.select) area.select();
      let ok = false;
      try {
        ok = !!doc.execCommand("copy");
      } catch (e) {
        ok = false;
      }
      if (area.remove) area.remove();
      return ok;
    }

    function copyLink() {
      const url = safePageURL(currentHref());
      if (!url) {
        setStatus("Couldn’t copy the link.");
        return Promise.resolve();
      }
      // Selection copy has to happen in this turn. The clipboard promise can
      // reject after the browser has forgotten the click.
      const legacy = copyWithSelection(url);
      const clipboard = win.navigator && win.navigator.clipboard;
      const failed = "Couldn’t copy the link.";
      if (clipboard && typeof clipboard.writeText === "function") {
        let pending;
        try {
          pending = clipboard.writeText(url);
        } catch (e) {
          setStatus(legacy ? "Link copied." : failed);
          return Promise.resolve();
        }
        return Promise.resolve(pending)
          .then(() => {
            setStatus("Link copied.");
          })
          .catch(() => {
            setStatus(legacy || copyWithSelection(url) ? "Link copied." : failed);
          });
      }
      setStatus(legacy ? "Link copied." : failed);
      return Promise.resolve();
    }

    function requestPrompt() {
      if (prompting || !promptEvent || typeof promptEvent.prompt !== "function" || shouldHide()) {
        return Promise.resolve({ ok: false, reason: "unavailable" });
      }
      const current = promptEvent;
      promptEvent = null;
      prompting = true;
      let pending;
      try {
        pending = current.prompt();
      } catch (e) {
        prompting = false;
        promoteQueued();
        setStatus("Couldn’t start installation. You can use the steps below.");
        sync();
        return Promise.resolve({ ok: false, reason: "error" });
      }
      sync();
      return Promise.resolve(pending)
        .then(() => (current.userChoice ? current.userChoice : null))
        .then((choice) => {
          const outcome = (choice && choice.outcome) || "";
          if (outcome === "dismissed") {
            setStatus("Installation was dismissed. You can still add it using the steps below.");
          } else if (outcome === "accepted") {
            setStatus("The browser is handling installation. You can keep using this tab either way.");
          }
          return { ok: true, outcome: outcome };
        })
        .catch(() => {
          setStatus("Couldn’t start installation. You can use the steps below.");
          return { ok: false, reason: "error" };
        })
        .finally(() => {
          prompting = false;
          promoteQueued();
          sync();
        });
    }

    function handleBrowserEvent(detail) {
      if (!detail) return;
      if (detail.type === "appinstalled") {
        promptEvent = null;
        queuedEvent = null;
        sessionInstalled = true;
        setStatus("This browser installed DJMagic.io. You can keep using this tab.");
        sync();
        return;
      }
      if (detail.type !== "beforeinstallprompt" || sessionInstalled) return;
      const event = detail.event;
      if (!event || typeof event.prompt !== "function") return;
      if (prompting) queuedEvent = event;
      else promptEvent = event;
      sync();
    }

    function onClickCapture(e) {
      const trigger = e.target && e.target.closest && e.target.closest("[data-open-dialog='install-app-dialog']");
      if (!trigger) return;
      opener = trigger;
      savedScroll = win.scrollY || 0;
      const root = doc.documentElement;
      const width = win.innerWidth || 0;
      const client = (root && root.clientWidth) || width;
      if (root && root.style && width - client > 0) root.style.paddingRight = width - client + "px";
      setStatus("");
      bindDialog();
      sync({ opening: true });
    }

    function onClick(e) {
      const target = e.target;
      if (!target || !target.closest) return;
      if (target.closest("[data-install-prompt]")) {
        e.preventDefault();
        requestPrompt().catch(() => setStatus("Couldn’t start installation. You can use the steps below."));
        return;
      }
      const platformBtn = target.closest("[data-install-platform]");
      if (platformBtn) {
        e.preventDefault();
        const name = platformBtn.getAttribute("data-install-platform");
        if (name === "ios" || name === "android" || name === "desktop") {
          platformChoice = name;
          sync();
        }
        return;
      }
      if (target.closest("[data-install-copy]")) {
        e.preventDefault();
        copyLink().catch(() => setStatus("Couldn’t copy the link."));
        return;
      }
      if (target.closest("#install-app-dialog [data-close-dialog]")) {
        restorePage();
        return;
      }
      if (target.closest("[data-open-dialog='install-app-dialog']")) {
        if (savedScroll != null && win.scrollY !== savedScroll && win.scrollTo) win.scrollTo(0, savedScroll);
        const node = dialog();
        if (node && node.open && usablePrompt()) focusEl(node.querySelector("[data-install-prompt]"));
      }
    }

    function onKeyDown(e) {
      if (e.key !== "Escape") return;
      const node = dialog();
      if (!node || !node.open) return;
      releaseScrollLock();
    }

    function swapRemovesDialog(event) {
      const node = dialog();
      if (!node || !node.open) return false;
      const target = event && event.detail && (event.detail.target || event.detail.elt);
      if (!target) return true;
      if (target === doc.body) return true;
      return !!(target.contains && target.contains(node));
    }

    function onBeforeSwap(event) {
      if (swapRemovesDialog(event)) closeInstallDialog();
    }

    function onCleanup(event) {
      const elt = event && event.target;
      if (elt && elt.id === "install-app-dialog" && elt.open && elt.close) elt.close();
    }

    function start() {
      if (started) return;
      started = true;
      const early = win.djmInstallEarly;
      if (early && typeof early.setHandler === "function") {
        const pending = typeof early.take === "function" ? early.take() : [];
        early.setHandler(handleBrowserEvent);
        pending.forEach(handleBrowserEvent);
      } else {
        win.addEventListener("beforeinstallprompt", (event) => {
          const off = doc.documentElement && doc.documentElement.getAttribute && doc.documentElement.getAttribute("data-install-ui") === "off";
          if (off) return;
          if (event && event.preventDefault) event.preventDefault();
          handleBrowserEvent({ type: "beforeinstallprompt", event: event });
        });
        win.addEventListener("appinstalled", () => handleBrowserEvent({ type: "appinstalled" }));
      }
      if (typeof win.matchMedia === "function") {
        installedDisplayModes.forEach((mode) => {
          try {
            const mq = win.matchMedia("(display-mode: " + mode + ")");
            if (mq && mq.addEventListener) {
              mq.addEventListener("change", () => {
                if (isInstalledDisplay(win)) closeInstallDialog();
                sync();
              });
            }
          } catch (e) {}
        });
      }
      doc.addEventListener("click", onClickCapture, true);
      doc.addEventListener("click", onClick);
      doc.addEventListener("keydown", onKeyDown, true);
      doc.addEventListener("htmx:beforeHistorySave", closeInstallDialog);
      doc.addEventListener("htmx:beforeSwap", onBeforeSwap);
      doc.addEventListener("htmx:beforeCleanupElement", onCleanup);
      doc.addEventListener("htmx:afterSettle", () => {
        bindDialog();
        sync();
      });
      doc.addEventListener("htmx:historyRestore", () => {
        closeInstallDialog();
        bindDialog();
        sync();
      });
      if (doc.readyState === "loading") {
        doc.addEventListener("DOMContentLoaded", () => {
          bindDialog();
          sync();
        });
      }
      bindDialog();
      sync();
    }

    return {
      start: start,
      sync: sync,
      handleBrowserEvent: handleBrowserEvent,
      requestPrompt: requestPrompt,
      choosePlatform: function (name) {
        platformChoice = name;
        sync();
      },
      copyLink: copyLink,
      usablePrompt: usablePrompt,
      shouldHide: shouldHide,
      selectedPlatform: selectedPlatform,
      status: function () { return statusText; },
      sessionInstalled: function () { return sessionInstalled; },
      prompting: function () { return prompting; },
      retainedPrompt: function () { return promptEvent; },
    };
  }

  return {
    installedDisplayModes: installedDisplayModes,
    isInstalledDisplay: isInstalledDisplay,
    detectPlatform: detectPlatform,
    inAppBrowser: inAppBrowser,
    safePageURL: safePageURL,
    createInstall: createInstall,
  };
});

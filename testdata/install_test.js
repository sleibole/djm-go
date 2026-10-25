const test = require("node:test");
const assert = require("node:assert/strict");
const install = require("../static/install.js");

const iphoneChrome = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/126.0.6478.54 Mobile/15E148 Safari/604.1";
const iphoneSafari = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1";
const ipadSafari = "Mozilla/5.0 (iPad; CPU OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1";
const ipadDesktop = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Safari/605.1.15";
const androidChrome = "Mozilla/5.0 (Linux; Android 14; Pixel 7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36";
const desktopChrome = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36";
const desktopWindows = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36";

test("platform detection prefers iOS over a Chrome token", () => {
  assert.equal(install.detectPlatform({ userAgent: iphoneChrome, maxTouchPoints: 5 }), "ios");
  assert.equal(install.detectPlatform({ userAgent: iphoneSafari, platform: "iPhone" }), "ios");
  assert.equal(install.detectPlatform({ userAgent: ipadSafari, platform: "iPad" }), "ios");
  assert.equal(install.detectPlatform({ userAgent: ipadDesktop, platform: "MacIntel", maxTouchPoints: 5 }), "ios");
  assert.equal(install.detectPlatform({
    userAgent: desktopChrome,
    platform: "MacIntel",
    maxTouchPoints: 2,
    userAgentData: { platform: "macOS" },
  }), "desktop");
  assert.equal(install.detectPlatform({ userAgent: androidChrome, userAgentData: { platform: "Android" } }), "android");
  assert.equal(install.detectPlatform({ userAgent: "Mozilla/5.0 Chrome/126", userAgentData: { platform: "Android" } }), "android");
  assert.equal(install.detectPlatform({ userAgent: desktopChrome, maxTouchPoints: 0 }), "desktop");
  assert.equal(install.detectPlatform({ userAgent: desktopWindows }), "desktop");
  assert.equal(install.detectPlatform({ userAgent: "" }), "unknown");
});

test("in-app browsers are a hint, not a platform", () => {
  assert.equal(install.inAppBrowser({ userAgent: iphoneSafari + " Instagram 320.0.0.0.0" }), true);
  assert.equal(install.inAppBrowser({ userAgent: androidChrome }), false);
  assert.equal(install.detectPlatform({ userAgent: iphoneSafari + " Instagram 1" }), "ios");
});

test("safe copy URL drops auth tokens and keeps an ordinary page", () => {
  assert.equal(install.safePageURL("https://djmagic.io/shows/friday?q=hello"), "https://djmagic.io/shows/friday?q=hello");
  assert.equal(install.safePageURL("https://djmagic.io/auth/secret-token"), "https://djmagic.io/");
  assert.equal(install.safePageURL("https://djmagic.io/shows/friday?token=abc#queue"), "https://djmagic.io/");
  assert.equal(install.safePageURL("https://djmagic.io/signin?next=%2Fauth%2Fsecret"), "https://djmagic.io/");
  assert.equal(install.safePageURL("https://user:pass@djmagic.io/account"), "https://djmagic.io/account");
  assert.equal(install.safePageURL("not a url"), "");
});

test("installed display modes are this window only", () => {
  const none = () => ({ matches: false });
  assert.equal(install.isInstalledDisplay({ navigator: { standalone: true }, matchMedia: none }), true);
  assert.equal(install.isInstalledDisplay({
    navigator: {},
    matchMedia: (q) => ({ matches: q === "(display-mode: standalone)" }),
  }), true);
  assert.equal(install.isInstalledDisplay({
    navigator: {},
    matchMedia: (q) => ({ matches: q === "(display-mode: fullscreen)" }),
  }), true);
  assert.equal(install.isInstalledDisplay({
    navigator: {},
    matchMedia: (q) => ({ matches: q === "(display-mode: browser)" }),
  }), false);
  assert.equal(install.isInstalledDisplay({ navigator: {}, matchMedia: none }), false);
});

function el(attrs = {}) {
  const node = {
    id: attrs.id || "",
    attrs: { ...attrs },
    hidden: !!attrs.hidden,
    open: false,
    dataset: {},
    textContent: "",
    children: [],
    parentElement: null,
    style: {},
    className: "",
    getAttribute(name) {
      return Object.prototype.hasOwnProperty.call(this.attrs, name) ? this.attrs[name] : null;
    },
    setAttribute(name, value) {
      this.attrs[name] = String(value);
    },
    appendChild(child) {
      if (child.parentElement) {
        child.parentElement.children = child.parentElement.children.filter((item) => item !== child);
      }
      child.parentElement = this;
      this.children.push(child);
      return child;
    },
    querySelector(sel) {
      return this.querySelectorAll(sel)[0] || null;
    },
    querySelectorAll(sel) {
      const out = [];
      const walk = (n) => {
        n.children.forEach((child) => {
          if (matches(child, sel)) out.push(child);
          walk(child);
        });
      };
      walk(this);
      return out;
    },
    closest(sel) {
      let n = this;
      while (n) {
        if (matches(n, sel)) return n;
        n = n.parentElement;
      }
      return null;
    },
    contains(other) {
      if (this === other) return true;
      return this.children.some((child) => child.contains(other));
    },
    focus() {
      this.focused = true;
    },
    addEventListener(type, fn) {
      this.listeners = this.listeners || {};
      (this.listeners[type] = this.listeners[type] || []).push(fn);
    },
    close() {
      this.open = false;
      (this.listeners && this.listeners.close || []).forEach((fn) => fn());
    },
  };
  return node;
}

function matches(node, sel) {
  const eq = sel.match(/^\[([^=\]]+)="([^"]+)"\]$/);
  if (eq) return node.attrs[eq[1]] === eq[2];
  const bare = sel.match(/^\[([^\]]+)\]$/);
  if (bare) return Object.prototype.hasOwnProperty.call(node.attrs, bare[1]);
  return false;
}

function mountPage() {
  const byId = {};
  function track(node) {
    if (node.id) byId[node.id] = node;
    node.children.forEach(track);
  }
  const opener = el({ "data-open-dialog": "install-app-dialog" });
  const native = el({ "data-install-native": "", hidden: true });
  const prompt = el({ "data-install-prompt": "" });
  native.appendChild(prompt);
  const status = el({ id: "install-app-status" });
  const direct = el({ "data-install-direct": "" });
  const manual = el({ "data-install-manual": "" });
  const panels = {
    ios: el({ "data-install-panel": "ios", hidden: true }),
    android: el({ "data-install-panel": "android", hidden: true }),
    desktop: el({ "data-install-panel": "desktop", hidden: true }),
    unknown: el({ "data-install-panel": "unknown" }),
  };
  const inapp = el({ "data-install-inapp": "", hidden: true });
  const platforms = ["ios", "android", "desktop"].map((name) => el({ "data-install-platform": name }));
  manual.appendChild(inapp);
  Object.values(panels).forEach((panel) => manual.appendChild(panel));
  platforms.forEach((button) => manual.appendChild(button));
  direct.appendChild(manual);
  const details = el({ "data-install-other": "", hidden: true });
  const mount = el({ "data-install-manual-mount": "" });
  details.appendChild(mount);
  const close = el({ "data-close-dialog": "" });
  const dialog = el({ id: "install-app-dialog" });
  [close, native, status, direct, details].forEach((child) => dialog.appendChild(child));
  const root = el();
  root.classList = {
    classes: new Set(),
    toggle(name, on) {
      if (on) this.classes.add(name);
      else this.classes.delete(name);
    },
    contains(name) {
      return this.classes.has(name);
    },
  };
  root.style = {};
  const body = el();
  body.appendChild(opener);
  body.appendChild(dialog);
  track(body);
  const doc = {
    documentElement: root,
    body,
    activeElement: null,
    readyState: "complete",
    getElementById(id) {
      return byId[id] || null;
    },
    contains(node) {
      return body.contains(node);
    },
    addEventListener() {},
    createElement() {
      return el();
    },
  };
  const listeners = {};
  const win = {
    document: doc,
    navigator: { userAgent: desktopChrome, maxTouchPoints: 0, platform: "MacIntel" },
    location: { href: "https://djmagic.io/shows/friday?q=hello" },
    scrollY: 240,
    innerWidth: 800,
    scrollTo(x, y) {
      this.scrollY = y;
    },
    matchMedia() {
      return { matches: false, addEventListener() {} };
    },
    addEventListener(type, fn) {
      (listeners[type] = listeners[type] || []).push(fn);
    },
    listeners,
  };
  root.clientWidth = 800;
  const api = install.createInstall({ window: win, document: doc });
  api.start();
  return { api, win, doc, dialog, native, prompt, panels, inapp, details, manual, direct, mount, opener, status, listeners };
}

test("native install button follows a usable prompt and a consumed event", async () => {
  const page = mountPage();
  assert.equal(page.native.hidden, true);
  assert.equal(page.panels.desktop.hidden, false);
  assert.equal(page.panels.ios.hidden, true);

  const event = {
    calls: 0,
    prompt() {
      this.calls += 1;
      return Promise.resolve();
    },
    userChoice: Promise.resolve({ outcome: "dismissed" }),
  };
  page.api.handleBrowserEvent({ type: "beforeinstallprompt", event });
  assert.equal(page.native.hidden, false);
  assert.equal(page.details.hidden, false);
  assert.equal(page.manual.parentElement, page.mount);
  assert.equal(page.details.open, false);

  const first = await page.api.requestPrompt();
  assert.equal(first.outcome, "dismissed");
  assert.equal(event.calls, 1);
  assert.equal(page.api.retainedPrompt(), null);
  assert.equal(page.native.hidden, true);
  assert.equal(page.manual.parentElement, page.direct);
  assert.match(page.api.status(), /dismissed/i);
  assert.equal(page.api.sessionInstalled(), false);

  const second = await page.api.requestPrompt();
  assert.equal(second.reason, "unavailable");
  assert.equal(event.calls, 1);
});

test("a prompt already in progress cannot start another", async () => {
  const page = mountPage();
  let release;
  const event = {
    calls: 0,
    prompt() {
      this.calls += 1;
      return new Promise((resolve) => {
        release = resolve;
      });
    },
    userChoice: Promise.resolve({ outcome: "accepted" }),
  };
  page.api.handleBrowserEvent({ type: "beforeinstallprompt", event });
  const pending = page.api.requestPrompt();
  const blocked = await page.api.requestPrompt();
  assert.equal(blocked.reason, "unavailable");
  assert.equal(event.calls, 1);
  release();
  const result = await pending;
  assert.equal(result.outcome, "accepted");
  assert.equal(page.api.sessionInstalled(), false);
  assert.match(page.api.status(), /handling installation/i);
  assert.doesNotMatch(page.api.status(), /installed/i);
});

test("prompt errors stay on the manual instructions", async () => {
  const page = mountPage();
  page.api.handleBrowserEvent({
    type: "beforeinstallprompt",
    event: {
      prompt() {
        return Promise.reject(new Error("blocked"));
      },
    },
  });
  const result = await page.api.requestPrompt();
  assert.equal(result.reason, "error");
  assert.match(page.api.status(), /Couldn’t start installation/);
  assert.equal(page.manual.parentElement, page.direct);

  const thrown = mountPage();
  thrown.api.handleBrowserEvent({
    type: "beforeinstallprompt",
    event: {
      prompt() {
        throw new Error("sync");
      },
    },
  });
  const syncResult = await thrown.api.requestPrompt();
  assert.equal(syncResult.reason, "error");
});

test("a later prompt replaces a used one, including one that arrives mid-prompt", async () => {
  const page = mountPage();
  let release;
  const first = {
    prompt() {
      return new Promise((resolve) => {
        release = resolve;
      });
    },
    userChoice: Promise.resolve({ outcome: "dismissed" }),
  };
  const next = {
    calls: 0,
    prompt() {
      this.calls += 1;
      return Promise.resolve();
    },
    userChoice: Promise.resolve({ outcome: "dismissed" }),
  };
  page.api.handleBrowserEvent({ type: "beforeinstallprompt", event: first });
  const pending = page.api.requestPrompt();
  page.api.handleBrowserEvent({ type: "beforeinstallprompt", event: next });
  assert.equal(page.native.hidden, true);
  release();
  await pending;
  assert.equal(page.api.retainedPrompt(), next);
  assert.equal(page.native.hidden, false);
  await page.api.requestPrompt();
  assert.equal(next.calls, 1);
});

test("appinstalled hides the invitation for this page only", () => {
  const page = mountPage();
  page.api.handleBrowserEvent({
    type: "beforeinstallprompt",
    event: { prompt() {} },
  });
  page.api.handleBrowserEvent({ type: "appinstalled" });
  assert.equal(page.api.sessionInstalled(), true);
  assert.equal(page.api.shouldHide(), true);
  assert.equal(page.doc.documentElement.classList.contains("djm-install-hide"), true);
  assert.equal(page.native.hidden, true);
  assert.match(page.api.status(), /This browser installed/);
  page.api.handleBrowserEvent({
    type: "beforeinstallprompt",
    event: { prompt() {} },
  });
  assert.equal(page.api.usablePrompt(), false);
});

test("browser display mode keeps install help", () => {
  const page = mountPage();
  assert.equal(page.api.shouldHide(), false);
  assert.equal(page.doc.documentElement.classList.contains("djm-install-hide"), false);
  page.win.matchMedia = (q) => ({ matches: q === "(display-mode: standalone)", addEventListener() {} });
  page.api.sync();
  assert.equal(page.api.shouldHide(), true);
  assert.equal(page.doc.documentElement.classList.contains("djm-install-hide"), true);
});

test("platform switch and in-app hint do not remove the other instructions", () => {
  const page = mountPage();
  page.win.navigator = { userAgent: iphoneSafari + " Instagram 1", maxTouchPoints: 5, platform: "iPhone" };
  page.api.sync();
  assert.equal(page.panels.ios.hidden, false);
  assert.equal(page.inapp.hidden, false);
  page.api.choosePlatform("android");
  assert.equal(page.panels.android.hidden, false);
  assert.equal(page.panels.ios.hidden, true);
  page.api.choosePlatform("desktop");
  assert.equal(page.panels.desktop.hidden, false);
});

test("start registers browser listeners once", () => {
  const page = mountPage();
  const before = page.listeners.beforeinstallprompt.length;
  page.api.start();
  assert.equal(page.listeners.beforeinstallprompt.length, before);
  assert.equal(before, 1);
});

test("early prompt queue is applied once", () => {
  const doc = {
    documentElement: { classList: { toggle() {} }, style: {}, getAttribute() { return null; } },
    readyState: "complete",
    getElementById() { return null; },
    addEventListener() {},
    contains() { return false; },
  };
  const queued = [];
  let handler = null;
  const win = {
    document: doc,
    navigator: { userAgent: desktopChrome },
    location: { href: "https://djmagic.io/" },
    matchMedia() { return { matches: false, addEventListener() {} }; },
    addEventListener() {
      throw new Error("early capture already owns the browser listeners");
    },
    djmInstallEarly: {
      setHandler(fn) { handler = fn; },
      take() {
        const items = queued.splice(0);
        return items;
      },
    },
  };
  const event = { prompt() { return Promise.resolve(); } };
  queued.push({ type: "beforeinstallprompt", event });
  const api = install.createInstall({ window: win, document: doc });
  api.start();
  assert.equal(api.retainedPrompt(), event);
  handler({ type: "appinstalled" });
  assert.equal(api.sessionInstalled(), true);
  assert.equal(api.retainedPrompt(), null);
});

test("copy link uses the clipboard and does not copy a token URL", async () => {
  const page = mountPage();
  let copied = "";
  page.win.navigator.clipboard = {
    writeText(value) {
      copied = value;
      return Promise.resolve();
    },
  };
  await page.api.copyLink();
  assert.equal(copied, "https://djmagic.io/shows/friday?q=hello");
  assert.equal(page.api.status(), "Link copied.");

  copied = "";
  page.win.location.href = "https://djmagic.io/auth/secret";
  page.win.navigator.clipboard.writeText = () => Promise.reject(new Error("denied"));
  let selected = "";
  page.doc.execCommand = () => true;
  page.doc.createElement = () => ({
    style: {},
    setAttribute() {},
    select() {},
    remove() {},
    set value(v) { selected = v; },
  });
  await page.api.copyLink();
  assert.equal(copied, "");
  assert.equal(selected, "https://djmagic.io/");
  assert.equal(page.api.status(), "Link copied.");
});

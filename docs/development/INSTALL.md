# Installing DJMagic.io

The footer **Install app** button opens the install dialog. Nothing opens on its own, and the venue display leaves the browser's install UI alone (`data-install-ui="off"`).

The dialog, the early prompt catcher in `templates/layout.html`, and `static/install.js` are the whole install path. There is no service worker and no offline cache.

## Where installation can be one click

Chromium browsers (Chrome and Edge, and Brave on Android or desktop) can fire `beforeinstallprompt`. The script in the document head calls `preventDefault()` and holds the event until `install.js` loads, including across an HTMX body swap. The dialog's **Install app** button calls `prompt()` from that click.

The browser may still refuse. A dismissed prompt stays dismissed until the browser offers another event. An accepted prompt means the browser took over, not that a home-screen icon exists yet. `appinstalled` hides the footer button for the rest of that page. Nothing is written to storage, because a stored "installed" flag would stick around after uninstall.

If the event never arrives, the dialog shows manual steps for the computer or the phone it detected. Android without the event gets the browser-menu instructions. A computer does not open on the iPhone or Android steps; those tabs are there when someone at a laptop is reading them for a phone.

Chromium also wants a 192px icon and a 512px icon in the manifest before it will fire the prompt. This app only has the 192 (`static/images/brand/dark_192.png`). A real 512, and a maskable icon, still need to be exported from the original artwork. Until then, Chrome and Edge may never show the one-click button, and the manual steps are the path that works.

## Why iPhone and iPad are manual

Safari does not let a page open the Add to Home Screen sheet. The dialog tells Safari visitors to use the Share menu, choose Add to Home Screen, leave Open as Web App on when it appears, and tap Add.

Brave, Chrome, Firefox, and Edge on iPhone and iPad cannot create that web app themselves. Their user agents (`Brave/`, `CriOS/`, `FxiOS/`, `EdgiOS/`) get a Safari handoff instead: copy the link, paste it into Safari, then follow the same Add to Home Screen steps. An iOS browser the script does not recognize keeps the generic steps, which already say to open Safari. Identification is only a few tokens next to `detectPlatform` in `static/install.js`. A desktop user who switches the dialog to iPhone & iPad sees the Safari steps, because that computer is not one of those browsers.

The copied link is the current page when its path and query are safe, and the site origin otherwise. Tokens, magic links, and `/auth/` paths are dropped. The Clipboard API is tried first. If that and the selection fallback both fail, the dialog shows the same URL in a readonly field.

## Already installed

The footer button is hidden when this window is already the installed app: `display-mode` of `standalone`, `fullscreen`, `minimal-ui`, `window-controls-overlay`, or `tabbed`, or Safari's `navigator.standalone`. A normal browser tab cannot tell that the app is installed on the home screen. The button stays in that tab.

## Tests

`go test -run 'TestInstallEntry|TestWebManifestInstallable|TestInstallScript'` runs the HTML checks and `node --test testdata/install_test.js`.

On a phone, check Safari (the four Add to Home Screen steps), Brave or Chrome (copy, then Safari), and Android Chrome (the Install button when the browser offers it, otherwise the menu steps). Launching from the home screen should hide the footer link. Copying from a page with a `token` query should copy the origin only.

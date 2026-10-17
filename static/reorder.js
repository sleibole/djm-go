// Queue reorder on the host show page, adapted from track-anything. A handle
// drags a queued row; the row moves on screen at once and the server is told
// which request it now follows. New requests can arrive mid-drag, so only the
// moved row and its new neighbor are sent, not the whole list. Without this
// script the handles stay hidden.

document.documentElement.dataset.reorder = "1";
if (!window.queueReorder) {
  window.queueReorder = true;

  const dragThreshold = 8;
  let drag = null;
  let saving = false;
  let suppressClick = false;

  document.addEventListener("pointerdown", onPointerDown, { passive: false });
  document.addEventListener("pointermove", onPointerMove, { passive: false });
  document.addEventListener("pointerup", (event) => finishDrag(event, true));
  document.addEventListener("pointercancel", (event) => finishDrag(event, false));
  document.addEventListener(
    "click",
    (event) => {
      if (!suppressClick) return;
      suppressClick = false;
      event.preventDefault();
      event.stopPropagation();
    },
    true,
  );
  document.addEventListener("contextmenu", (event) => {
    if (drag) event.preventDefault();
  });

  function onPointerDown(event) {
    if (saving || drag || !event.isPrimary || event.button !== 0) return;
    const handle = event.target.closest("[data-drag-handle]");
    const item = handle && handle.closest("[data-reorder-item]");
    const list = item && item.parentElement && item.parentElement.closest("[data-reorder-list]");
    if (!handle || !item || !list || item.parentElement !== list) return;
    event.preventDefault();
    try {
      handle.setPointerCapture(event.pointerId);
    } catch {
      // The pointer can already be gone. The move and up handlers no-op without drag.
    }
    drag = {
      pointerId: event.pointerId,
      item,
      list,
      startY: event.clientY,
      y: event.clientY,
      moved: false,
      previous: itemIDs(list),
      frame: 0,
    };
    document.documentElement.dataset.reordering = "1";
  }

  function onPointerMove(event) {
    if (!drag || event.pointerId !== drag.pointerId) return;
    event.preventDefault();
    drag.y = event.clientY;
    if (!drag.moved) {
      if (Math.abs(event.clientY - drag.startY) < dragThreshold) return;
      drag.moved = true;
      drag.item.classList.add("is-dragging");
      drag.frame = requestAnimationFrame(tick);
    }
    // Place immediately. A fast drag can release before the next frame.
    placeItem(drag.list, drag.item, drag.y);
  }

  function tick() {
    if (!drag || !drag.moved) return;
    const y = drag.y;
    const edge = 72;
    if (y < edge) window.scrollBy(0, -Math.ceil((edge - y) / 8));
    else if (y > window.innerHeight - edge) window.scrollBy(0, Math.ceil((y - (window.innerHeight - edge)) / 8));
    placeItem(drag.list, drag.item, y);
    drag.frame = requestAnimationFrame(tick);
  }

  function finishDrag(event, commit) {
    if (!drag || event.pointerId !== drag.pointerId) return;
    const state = drag;
    drag = null;
    if (state.frame) cancelAnimationFrame(state.frame);
    state.item.classList.remove("is-dragging");
    if (!commit || !state.moved) {
      if (state.moved) restoreItems(state.list, state.previous);
      document.documentElement.removeAttribute("data-reordering");
      return;
    }
    // The click that follows a drop comes in the same task; don't let the flag
    // outlive it and eat the host's next real click.
    suppressClick = true;
    setTimeout(() => (suppressClick = false), 0);
    if (itemIDs(state.list).join(",") === state.previous.join(",")) {
      document.documentElement.removeAttribute("data-reordering");
      return;
    }
    const before = state.item.previousElementSibling;
    const after = before && before.matches("[data-reorder-item]") ? before.dataset.id : "";
    saveMove(state.list, state.previous, state.item.dataset.id, after);
  }

  // Swap with the neighbor the pointer has crossed, and keep going when one
  // move crosses several rows.
  function placeItem(list, item, clientY) {
    const limit = list.querySelectorAll(":scope > [data-reorder-item]").length;
    for (let i = 0; i < limit; i++) {
      const next = item.nextElementSibling;
      if (next && next.matches("[data-reorder-item]")) {
        const box = next.getBoundingClientRect();
        if (clientY > box.top + box.height / 2) {
          next.after(item);
          continue;
        }
      }
      const prev = item.previousElementSibling;
      if (prev && prev.matches("[data-reorder-item]")) {
        const box = prev.getBoundingClientRect();
        if (clientY < box.top + box.height / 2) {
          list.insertBefore(item, prev);
          continue;
        }
      }
      break;
    }
  }

  function itemIDs(list) {
    return [...list.querySelectorAll(":scope > [data-reorder-item]")].map((el) => el.dataset.id);
  }

  function restoreItems(list, ids) {
    const byID = new Map(itemIDs(list).map((id) => [id, list.querySelector(`:scope > [data-reorder-item][data-id="${CSS.escape(id)}"]`)]));
    for (const id of ids) {
      const el = byID.get(id);
      if (el) list.appendChild(el);
    }
  }

  // The queue is refreshed either way: on success to renumber the rows, on a
  // conflict to show what the queue is now.
  function saveMove(list, previous, id, after) {
    saving = true;
    const body = new URLSearchParams({ after });
    fetch(`/requests/${encodeURIComponent(id)}/move`, {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body,
    })
      .then((res) => {
        if (!res.ok) restoreItems(list, previous);
      })
      .catch(() => restoreItems(list, previous))
      .finally(() => {
        saving = false;
        document.documentElement.removeAttribute("data-reordering");
        const live = document.getElementById("show-live");
        if (live && window.htmx) htmx.trigger(live, "refresh");
      });
  }
}

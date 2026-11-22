// split.js: a draggable bar between a pane and the part below it. The bar's
// position is kept as a share of the window height, per browser.

const narrow = () => window.matchMedia("(max-width: 800px)").matches;

// makeSplit wires bar to size pane inside container. storageKey names the
// localStorage entry that remembers the share; initial is the default share.
// min is the px each side keeps at least. layout() re-applies the size, for
// when the window or the surroundings move the pane. Narrow screens stack the
// parts without a bar (the CSS ignores the height).
export function makeSplit({ bar, pane, container, storageKey, initial, min = 80 }) {
  let at = initial;
  try {
    const v = parseFloat(localStorage.getItem(storageKey));
    if (v > 0 && v < 1) at = v;
  } catch {}

  const save = () => {
    try {
      localStorage.setItem(storageKey, String(at));
    } catch {}
  };

  // layout sizes the pane so the bar sits at the saved share of the window
  // height, keeping min px for the pane and for the part below.
  function layout() {
    if (narrow() || !pane.getClientRects().length) return;
    const top = pane.getBoundingClientRect().top;
    const bottom = container.getBoundingClientRect().bottom;
    const max = bottom - top - bar.offsetHeight - min;
    const h = Math.round(Math.max(min, Math.min(at * window.innerHeight - top, max)));
    pane.style.height = h + "px";
    bar.setAttribute("aria-valuenow", String(Math.round(at * 100)));
    bar.setAttribute("aria-valuemin", "5");
    bar.setAttribute("aria-valuemax", "95");
  }

  // move puts the bar's top at y (px from the viewport top), clamped.
  function move(y) {
    at = Math.min(Math.max(y / window.innerHeight, 0.05), 0.95);
    layout();
  }

  bar.addEventListener("pointerdown", (e) => {
    if (e.button !== 0) return;
    e.preventDefault();
    bar.setPointerCapture(e.pointerId);
    bar.focus();
    const grab = e.clientY - bar.getBoundingClientRect().top; // keep the bar under the pointer
    document.body.classList.add("splitting");
    const onMove = (ev) => move(ev.clientY - grab);
    // lostpointercapture follows pointerup and pointercancel, and also fires
    // when the view closes mid-drag.
    const up = () => {
      bar.removeEventListener("pointermove", onMove);
      document.body.classList.remove("splitting");
      save();
    };
    bar.addEventListener("pointermove", onMove);
    bar.addEventListener("lostpointercapture", up, { once: true });
  });
  bar.addEventListener("keydown", (e) => {
    if (e.key !== "ArrowUp" && e.key !== "ArrowDown") return;
    e.preventDefault();
    const step = (e.shiftKey ? 80 : 20) * (e.key === "ArrowUp" ? -1 : 1);
    move(bar.getBoundingClientRect().top + step);
    save();
  });

  return { layout };
}

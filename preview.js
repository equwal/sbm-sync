// Live preview for the bookmark page of sbm Sync, as in the sbm add-on: the
// page of the highlighted bookmark shows in a frame beside the results. The
// arrow keys in the search box and the mouse move the highlight, and Enter
// opens the highlighted bookmark (search.js). search.js changes the results;
// this script follows the changes. The preview needs a wide window: in a
// narrow one, the pane stays hidden and loads no page.
"use strict";

(() => {
  const results = document.getElementById("results");
  const pane = document.getElementById("preview");
  const form = document.getElementById("search");
  if (!results || !pane || !form) return;
  const box = form.elements.q;
  const frame = pane.querySelector("iframe");
  const caption = pane.querySelector("p");
  const wide = matchMedia("(min-width: 64em)");
  // A page loads only after the highlight stays on it this long (ms), so
  // that fast typing does not load a page for each letter.
  const wait = 300;
  // A search from the address bar of the browser starts the preview at once.
  // Else it starts when the user types or points at a bookmark.
  let started = box.value.trim() !== "";
  let sel = 0; // the highlighted row
  let timer = null;
  let framed = null; // the address in the frame
  let pointer = ""; // where the mouse was last

  const rows = () => [...results.querySelectorAll("li")];

  // address gives the address that the frame loads for a row, or null. An
  // http page loads with https, because a secure page cannot show an
  // insecure frame.
  function address(li) {
    const a = li && li.querySelector("a.open");
    if (!a) return null;
    const u = new URL(a.href);
    if (u.protocol !== "http:" && u.protocol !== "https:") return null;
    u.protocol = "https:";
    return u.href;
  }

  function load(to) {
    if (to === framed) return;
    framed = to;
    frame.src = to ?? "about:blank";
  }

  // show shows the page of row li, when the highlight stays on it.
  function show(li) {
    clearTimeout(timer);
    if (!wide.matches) return;
    const to = address(li);
    caption.textContent = !li ? "No bookmark to preview." : to ?? "No preview: this bookmark is not a web page.";
    if (to) timer = setTimeout(() => load(to), wait);
    else load(null);
  }

  function highlight(i) {
    const all = rows();
    sel = Math.max(0, Math.min(all.length - 1, i));
    all.forEach((li, j) => li.classList.toggle("sel", j === sel));
    if (started) show(all[sel]);
  }

  // point: the user points at row i, with the mouse, the Tab key or the
  // arrow keys.
  function point(i) {
    started = true;
    highlight(i);
  }

  // New results: the highlight goes to the first row.
  new MutationObserver(() => highlight(0)).observe(results, { childList: true });
  box.addEventListener("input", () => {
    started = true;
  });
  box.addEventListener("keydown", (e) => {
    if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
    e.preventDefault();
    point(sel + (e.key === "ArrowDown" ? 1 : -1));
    const li = rows()[sel];
    if (li) li.scrollIntoView({ block: "nearest" });
  });
  results.addEventListener("mousemove", (e) => {
    // The browser also sends mousemove when the page moves under a mouse
    // that stays still. Only a move of the mouse moves the highlight.
    const li = e.target.closest("li");
    const at = e.screenX + "," + e.screenY;
    if (!li || at === pointer) return;
    pointer = at;
    point(rows().indexOf(li));
  });
  results.addEventListener("focusin", (e) => {
    const li = e.target.closest("li");
    if (li) point(rows().indexOf(li));
  });
  wide.addEventListener("change", () => (wide.matches ? highlight(sel) : load(null)));
  // A page in the frame can take the focus with a script, and then the
  // typing goes into that page. The preview is only to look at, so the
  // focus goes back to the search box at once. The mouse wheel and clicks
  // on links still work in the frame.
  window.addEventListener("blur", () => setTimeout(() => {
    if (document.activeElement === frame) box.focus({ preventScroll: true });
  }));
  pane.hidden = false;
  highlight(0);
})();

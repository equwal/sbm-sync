// Live search for the bookmark pages of sbm Sync: the results change while
// you type, and Enter opens the first result, as in the sbm add-on.
// Ctrl+Enter opens it in a new tab. The server does the search: this script
// gets the page of the search and shows its results. Without this script the
// form still works.
"use strict";

(() => {
  const form = document.getElementById("search");
  const results = document.getElementById("results");
  if (!form || !results) return;
  const box = form.elements.q;
  const clears = results.dataset.empty === "clear"; // no results for no query
  let timer = null; // the update that waits for a pause in the typing
  let running = Promise.resolve(); // the newest update
  let newest = 0; // the number of the newest update

  // update shows the results that the server gives for the form.
  async function update() {
    const n = ++newest;
    const query = new URLSearchParams(new FormData(form)).toString();
    let fresh = null;
    if (!clears || box.value.trim() !== "") {
      try {
        const resp = await fetch("/bookmarks?" + query);
        if (!resp.ok) return;
        const page = new DOMParser().parseFromString(await resp.text(), "text/html");
        fresh = page.getElementById("results");
      } catch {
        return;
      }
      if (fresh === null) return; // not signed in any more
    }
    if (n !== newest) return; // a newer update is on its way
    results.replaceChildren(...(fresh === null ? [] : fresh.childNodes));
    if (!clears) history.replaceState(null, "", "?" + query);
  }

  function later() {
    clearTimeout(timer);
    timer = setTimeout(() => {
      timer = null;
      running = update();
    }, 150);
  }

  box.addEventListener("input", later);
  form.addEventListener("change", (e) => {
    if (e.target !== box) later();
  });
  box.addEventListener("keydown", async (e) => {
    if (e.key !== "Enter" || e.isComposing) return;
    e.preventDefault();
    if (timer !== null) {
      clearTimeout(timer);
      timer = null;
      running = update();
    }
    await running;
    const first = results.querySelector("a.open");
    if (first === null) form.submit();
    else if (e.ctrlKey || e.metaKey) window.open(first.href, "_blank", "noopener");
    else location.assign(first.href);
  });
})();

// The viewer's progressive enhancements. Every page works without this
// script; it adds type-ahead search, relative times, a dark mode toggle,
// highlighted search snippets and an image lightbox.
import { debounce, relativeTime, snippetParts, wikiURL } from "./lib.js";

function theme() {
  const root = document.documentElement;
  const saved = localStorage.getItem("kb-theme");
  if (saved) root.dataset.theme = saved;
  document.querySelector("[data-theme-toggle]")?.addEventListener("click", () => {
    const dark = root.dataset.theme
      ? root.dataset.theme === "dark"
      : matchMedia("(prefers-color-scheme: dark)").matches;
    root.dataset.theme = dark ? "light" : "dark";
    localStorage.setItem("kb-theme", root.dataset.theme);
  });
}

function times() {
  for (const el of document.querySelectorAll("time[datetime]")) {
    const d = new Date(el.getAttribute("datetime"));
    const r = relativeTime(d);
    if (r) el.textContent = r;
    el.title = d.toLocaleString();
  }
}

function snippets() {
  for (const el of document.querySelectorAll("[data-snippet]")) {
    const frag = document.createDocumentFragment();
    for (const p of snippetParts(el.textContent)) {
      if (p.mark) {
        const m = document.createElement("mark");
        m.textContent = p.text;
        frag.append(m);
      } else frag.append(p.text);
    }
    el.replaceChildren(frag);
  }
}

function suggest() {
  const input = document.querySelector("input[data-suggest]");
  const list = input?.parentElement.querySelector(".suggest");
  if (!input || !list) return;
  let items = [];
  let active = -1;
  const close = () => {
    list.hidden = true;
    active = -1;
  };
  const show = (titles) => {
    items = titles;
    active = -1;
    list.replaceChildren(
      ...titles.map((t) => {
        const li = document.createElement("li");
        const a = document.createElement("a");
        a.href = wikiURL(t);
        a.textContent = t;
        li.append(a);
        return li;
      }),
    );
    list.hidden = titles.length === 0;
  };
  const fetchSuggest = debounce(async (q) => {
    if (!q.trim()) return close();
    try {
      const r = await fetch("/api/suggest?q=" + encodeURIComponent(q));
      if (r.ok && input.value === q) show(await r.json());
    } catch {
      close();
    }
  }, 120);
  input.addEventListener("input", () => fetchSuggest(input.value));
  input.addEventListener("keydown", (e) => {
    if (list.hidden) return;
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      active = (active + (e.key === "ArrowDown" ? 1 : -1) + items.length) % items.length;
      [...list.children].forEach((li, i) => li.classList.toggle("active", i === active));
    } else if (e.key === "Enter" && active >= 0) {
      e.preventDefault();
      location.href = wikiURL(items[active]);
    } else if (e.key === "Escape") close();
  });
  input.addEventListener("blur", () => setTimeout(close, 150));
  document.addEventListener("keydown", (e) => {
    if (e.key === "/" && document.activeElement !== input && !/input|textarea/i.test(document.activeElement?.tagName)) {
      e.preventDefault();
      input.focus();
      input.select();
    }
  });
}

function lightbox() {
  const imgs = document.querySelectorAll(".content img:not(.missing)");
  if (!imgs.length) return;
  const box = document.createElement("div");
  box.className = "lightbox";
  box.hidden = true;
  const big = document.createElement("img");
  const cap = document.createElement("p");
  box.append(big, cap);
  document.body.append(box);
  box.addEventListener("click", () => (box.hidden = true));
  document.addEventListener("keydown", (e) => e.key === "Escape" && (box.hidden = true));
  for (const img of imgs) {
    img.addEventListener("click", (e) => {
      e.preventDefault();
      big.src = img.src;
      big.alt = img.alt;
      cap.textContent = img.alt;
      box.hidden = false;
    });
  }
}

theme();
document.addEventListener("DOMContentLoaded", () => {
  times();
  snippets();
  suggest();
  lightbox();
});

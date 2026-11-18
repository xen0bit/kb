// Pure helpers, kept apart from the DOM code so `bun test` can cover them.

/** Formats a past time relative to now: "just now", "5 minutes ago", ... */
export function relativeTime(date, now = new Date()) {
  const s = Math.round((now.getTime() - date.getTime()) / 1000);
  if (Number.isNaN(s)) return "";
  if (s < 60) return "just now";
  const units = [
    ["year", 365 * 24 * 3600],
    ["month", 30 * 24 * 3600],
    ["day", 24 * 3600],
    ["hour", 3600],
    ["minute", 60],
  ];
  for (const [name, secs] of units) {
    const n = Math.floor(s / secs);
    if (n >= 1) return `${n} ${name}${n === 1 ? "" : "s"} ago`;
  }
  return "just now";
}

/** The /wiki/ URL for a title, matching the server's canonical form. */
export function wikiURL(title) {
  const t = title.trim().replace(/\s+/g, "_");
  return "/wiki/" + t.replace(/[()%?"'\\/]/g, (c) => "%" + c.charCodeAt(0).toString(16).toUpperCase().padStart(2, "0"));
}

/**
 * Splits a search snippet whose matches are wrapped in ** into text and
 * highlight segments, so it can be rendered without innerHTML.
 */
export function snippetParts(snippet) {
  const parts = [];
  const re = /\*\*(.+?)\*\*/g;
  let last = 0;
  for (let m; (m = re.exec(snippet)); ) {
    if (m.index > last) parts.push({ text: snippet.slice(last, m.index), mark: false });
    parts.push({ text: m[1], mark: true });
    last = m.index + m[0].length;
  }
  if (last < snippet.length) parts.push({ text: snippet.slice(last), mark: false });
  return parts;
}

/** Delays fn until calls stop for ms. */
export function debounce(fn, ms) {
  let t;
  return (...args) => {
    clearTimeout(t);
    t = setTimeout(() => fn(...args), ms);
  };
}

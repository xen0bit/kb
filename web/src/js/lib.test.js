import { describe, expect, test } from "bun:test";
import { relativeTime, snippetParts, wikiURL } from "./lib.js";

describe("relativeTime", () => {
  const now = new Date("2026-01-10T12:00:00Z");
  test("recent", () => expect(relativeTime(new Date("2026-01-10T11:59:30Z"), now)).toBe("just now"));
  test("minutes", () => expect(relativeTime(new Date("2026-01-10T11:55:00Z"), now)).toBe("5 minutes ago"));
  test("singular", () => expect(relativeTime(new Date("2026-01-09T12:00:00Z"), now)).toBe("1 day ago"));
  test("invalid", () => expect(relativeTime(new Date("nope"), now)).toBe(""));
});

describe("wikiURL", () => {
  test("spaces and parens", () => expect(wikiURL("Foo (band)")).toBe("/wiki/Foo_%28band%29"));
  test("unicode kept", () => expect(wikiURL("Éclair")).toBe("/wiki/Éclair"));
});

describe("snippetParts", () => {
  test("marks", () =>
    expect(snippetParts("a **b** c **d**")).toEqual([
      { text: "a ", mark: false },
      { text: "b", mark: true },
      { text: " c ", mark: false },
      { text: "d", mark: true },
    ]));
  test("plain", () => expect(snippetParts("plain")).toEqual([{ text: "plain", mark: false }]));
});

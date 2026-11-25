import { describe, expect, it } from "vitest"

import {
  primaryChecks,
  THEME_TOKENS,
  type ThemeName,
  tokenHex,
} from "./brand-contrast"

function firstFailure(theme: ThemeName, hex: string): string | undefined {
  return primaryChecks(theme, hex).find((check) => !check.passes)?.use
}

describe("primaryChecks", () => {
  // The same colours the gateway's check is tested with, so the page and the
  // gateway agree on what they accept.
  it.each([
    ["light", "#0073EE", "button text"],
    ["light", "#0050EE", "button hover text"],
    ["light", "#585858", "button hover text on a card"],
    ["dark", "#000000", "button text"],
    ["dark", "#0082EE", "button hover text"],
  ] as const)("rejects %s %s for %s", (theme, hex, use) => {
    expect(firstFailure(theme, hex)).toBe(use)
  })

  it.each([
    ["light", "#0A4A8F"],
    ["dark", "#7FB8F0"],
  ] as const)("accepts %s %s", (theme, hex) => {
    expect(firstFailure(theme, hex)).toBeUndefined()
  })
})

// Vitest does not load CSS, so the source is read from disk. The app's types
// leave out Node, so the module is named through a variable.
const fsModule = "node:fs"

// SAFETY: node:fs exports readFileSync with this signature.
const { readFileSync } = (await import(fsModule)) as {
  readFileSync: (path: string, encoding: "utf-8") => string
}

const styles = readFileSync("src/styles.css", "utf-8")

function block(selector: string): string {
  const start = styles.indexOf(`\n${selector} {`)
  expect(start).toBeGreaterThanOrEqual(0)

  return styles.slice(start, styles.indexOf("\n}", start))
}

describe("THEME_TOKENS", () => {
  it.each([
    ["light", ":root"],
    ["dark", ".dark"],
  ] as const)("match the %s declarations in styles.css", (theme, selector) => {
    const source = block(selector)

    for (const [name, token] of Object.entries(THEME_TOKENS[theme])) {
      const match = new RegExp(
        String.raw`\s${name}:\s*oklch\((?<l>[\d.]+)%\s+(?<c>[\d.]+)\s+(?<h>[\d.]+)\)`,
        "u",
      ).exec(source)

      expect(match?.groups).toBeDefined()
      expect(Number(match?.groups?.l) / 100).toBeCloseTo(token.l, 9)
      expect(Number(match?.groups?.c)).toBeCloseTo(token.c, 9)
      expect(Number(match?.groups?.h)).toBeCloseTo(token.h, 9)
    }
  })
})

describe("tokenHex", () => {
  // styles.css notes the hex beside each of these tokens.
  it.each([
    ["light", "--foreground", "#15191E"],
    ["light", "--card", "#FFFFFF"],
  ] as const)("converts %s %s to %s", (theme, token, hex) => {
    expect(tokenHex(theme, token)).toBe(hex)
  })
})

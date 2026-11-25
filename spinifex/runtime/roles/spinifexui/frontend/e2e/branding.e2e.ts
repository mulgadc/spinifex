import type { Locator, Page, Route } from "@playwright/test"

import { primaryChecks } from "../src/lib/brand-contrast"
import { expect, test, type Theme, useTheme } from "./console"

const LIGHT = "#0A4A8F"
const DARK = "#7FB8F0"

const LOGO =
  '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16"><rect width="16" height="16" fill="#0A4A8F"/></svg>'

interface Brand {
  stylesheet: string
  logos?: { logo?: string; logoDark?: string }
}

// Answers the public branding route as the gateway does for revision 3.
async function serveBrand(
  page: Page,
  { stylesheet, logos = { logo: "logo?rev=3" } }: Brand,
): Promise<void> {
  await page.route("**/branding/**", async (route) => {
    const name = new URL(route.request().url()).pathname.replace(
      "/branding/",
      "",
    )

    if (name === "branding.json") {
      await route.fulfill({
        json: {
          enabled: true,
          revision: 3,
          productName: "Acme Cloud",
          docsUrl: "",
          stylesheet: "branding.css?rev=3",
          ...logos,
        },
      })
    } else if (name === "branding.css") {
      await route.fulfill({ contentType: "text/css", body: stylesheet })
    } else if (name === "logo" || name === "logo-dark") {
      await route.fulfill({ contentType: "image/svg+xml", body: LOGO })
    } else {
      await route.fulfill({ status: 404 })
    }
  })
}

async function signOut(page: Page): Promise<void> {
  await page.addInitScript(() => {
    localStorage.removeItem("spinifex:v2:aws-session")
  })
}

async function appliedPrimary(page: Page): Promise<string> {
  const { primary } = await primaryContrast(page)

  return primary
}

// The theme tokens as the page resolves them, in sRGB through a canvas, with
// the WCAG ratio of each pair a primary colour is used in.
async function primaryContrast(page: Page) {
  return await page.evaluate(() => {
    const canvas = document.createElement("canvas")
    canvas.width = 1
    canvas.height = 1
    const ctx = canvas.getContext("2d", { willReadFrequently: true })
    const root = getComputedStyle(document.documentElement)

    function luminance(token: string): number {
      if (!ctx) {
        throw new Error("no 2d context")
      }

      ctx.fillStyle = root.getPropertyValue(token).trim()
      ctx.fillRect(0, 0, 1, 1)

      // A typed array would clamp the linear values, so they go in a plain one.
      const [r = 0, g = 0, b = 0] = Array.from(
        ctx.getImageData(0, 0, 1, 1).data.slice(0, 3),
        (v) =>
          v / 255 <= 0.04045
            ? v / 255 / 12.92
            : ((v / 255 + 0.055) / 1.055) ** 2.4,
      )

      return 0.2126 * r + 0.7152 * g + 0.0722 * b
    }

    function ratio(a: string, b: string): number {
      const x = luminance(a)
      const y = luminance(b)

      return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05)
    }

    return {
      primary: root.getPropertyValue("--primary").trim(),
      buttonText: ratio("--primary", "--primary-foreground"),
      link: ratio("--primary", "--background"),
      ring: ratio("--ring", "--background"),
    }
  })
}

// The WCAG ratio of a button's text on its fill as the page paints it: the
// fill, with any alpha, is drawn over the card the button sits on.
async function paintedContrast(button: Locator): Promise<number> {
  return await button.evaluate((el) => {
    const canvas = document.createElement("canvas")
    canvas.width = 1
    canvas.height = 1
    const ctx = canvas.getContext("2d", { willReadFrequently: true })

    if (!ctx) {
      throw new Error("no 2d context")
    }

    const card = el.closest("[data-slot=card]")

    if (!card) {
      throw new Error("the button is not on a card")
    }

    function luminance(...layers: string[]): number {
      if (!ctx) {
        throw new Error("no 2d context")
      }

      ctx.clearRect(0, 0, 1, 1)

      for (const fill of layers) {
        ctx.fillStyle = fill
        ctx.fillRect(0, 0, 1, 1)
      }

      const [r = 0, g = 0, b = 0] = Array.from(
        ctx.getImageData(0, 0, 1, 1).data.slice(0, 3),
        (v) =>
          v / 255 <= 0.04045
            ? v / 255 / 12.92
            : ((v / 255 + 0.055) / 1.055) ** 2.4,
      )

      return 0.2126 * r + 0.7152 * g + 0.0722 * b
    }

    const ground = getComputedStyle(card).backgroundColor
    const style = getComputedStyle(el)
    const fill = luminance(ground, style.backgroundColor)
    const text = luminance(style.color)

    return (Math.max(fill, text) + 0.05) / (Math.min(fill, text) + 0.05)
  })
}

const BOTH =
  `:root:not(.dark){--primary:${LIGHT};--ring:${LIGHT}}\n` +
  `:root.dark{--primary:${DARK};--ring:${DARK}}\n`

const THEMES: [Theme, string][] = [
  ["light", LIGHT],
  ["dark", DARK],
]

test.describe("a branded console", () => {
  for (const [theme, colour] of THEMES) {
    test(`shows the operator's brand in the ${theme} theme`, async ({
      page,
    }) => {
      await useTheme(page, theme)
      await serveBrand(page, { stylesheet: BOTH })
      await page.goto("/")

      const sidebar = page.locator("[data-slot=sidebar]")
      await expect(sidebar.getByText("Acme Cloud")).toBeVisible()
      await expect(
        sidebar.getByRole("img", { name: "Acme Cloud" }),
      ).toHaveAttribute("src", "/branding/logo?rev=3")
      await expect(sidebar.getByText("Docs", { exact: true })).toHaveCount(0)
      await expect(page).toHaveTitle(/\| Acme Cloud$/u)

      const contrast = await primaryContrast(page)
      expect(contrast.primary).toBe(colour)
      expect(contrast.buttonText).toBeGreaterThanOrEqual(4.5)
      expect(contrast.link).toBeGreaterThanOrEqual(4.5)
      expect(contrast.ring).toBeGreaterThanOrEqual(3)
    })

    test(`brands the login page in the ${theme} theme`, async ({ page }) => {
      await useTheme(page, theme)
      await signOut(page)
      await serveBrand(page, { stylesheet: BOTH })
      await page.goto("/login")

      await expect(page.getByText("Acme Cloud credentials")).toBeVisible()
      expect(await appliedPrimary(page)).toBe(colour)
    })
  }

  // The validator's figure for the hovered button on a card is the one the
  // page paints, so a colour it accepts is readable on the login page.
  for (const [theme, colour] of THEMES) {
    test(`keeps the hovered login button readable in the ${theme} theme`, async ({
      page,
    }) => {
      await useTheme(page, theme)
      await signOut(page)
      await serveBrand(page, { stylesheet: BOTH })
      await page.goto("/login")

      const signIn = page.getByRole("button", { name: "Sign In" })
      await expect(signIn).toBeEnabled()

      const predicted = primaryChecks(theme, colour).find(
        (check) => check.use === "button hover text on a card",
      )?.ratio

      expect(predicted).toBeGreaterThanOrEqual(4.5)

      await signIn.hover()
      await expect
        .poll(async () => await paintedContrast(signIn))
        .toBeCloseTo(predicted ?? 0, 1)
    })
  }

  // A dark-theme logo with no logo replaces the built-in mark only in the dark
  // theme; the light theme keeps the built-in mark.
  for (const [theme, src] of [
    ["light", "/mulga-logo.svg"],
    ["dark", "/branding/logo-dark?rev=3"],
  ] as const) {
    test(`shows a dark-only logo only in the dark theme, in ${theme}`, async ({
      page,
    }) => {
      await useTheme(page, theme)
      await serveBrand(page, {
        stylesheet: BOTH,
        logos: { logoDark: "logo-dark?rev=3" },
      })
      await page.goto("/")

      const logo = page
        .locator("[data-slot=sidebar]")
        .getByRole("img", { name: "Acme Cloud" })

      await expect(logo).toHaveCount(1)
      await expect(logo).toHaveAttribute("src", src)
    })
  }

  test("leaves the dark theme at its default under a light-only colour", async ({
    page,
  }) => {
    await useTheme(page, "dark")
    await serveBrand(page, {
      stylesheet: `:root:not(.dark){--primary:${LIGHT};--ring:${LIGHT}}\n`,
    })
    await page.goto("/")

    await expect(page.getByText("Acme Cloud").first()).toBeVisible()
    expect(await appliedPrimary(page)).toBe("oklch(68% 0.18 38)")
  })
})

test.describe("a stalled branding stylesheet", () => {
  test("leaves the login page usable in the default colours", async ({
    page,
  }) => {
    await useTheme(page, "light")
    await signOut(page)
    await serveBrand(page, { stylesheet: BOTH })

    // Held open for the whole test, as a gateway that has stopped answering.
    const held: Route[] = []
    await page.route("**/branding/branding.css*", (route) => {
      held.push(route)
    })

    await page.goto("/login", { waitUntil: "domcontentloaded" })
    const started = Date.now()

    const signIn = page.getByRole("button", { name: "Sign In" })
    await expect(signIn).toBeEnabled({ timeout: 3000 })
    expect(Date.now() - started).toBeLessThan(3000)
    expect(held).toHaveLength(1)
    await expect(page.getByText("Acme Cloud credentials")).toBeVisible()
    expect(await appliedPrimary(page)).toBe("oklch(55% 0.19 36.6)")
  })
})

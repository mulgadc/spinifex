import type { Page, Route } from "@playwright/test"

import { expect, test, type Theme, useTheme } from "./console"

const LIGHT = "#0A4A8F"
const DARK = "#7FB8F0"

const LOGO =
  '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16"><rect width="16" height="16" fill="#0A4A8F"/></svg>'

interface Brand {
  stylesheet: string
}

// Answers the public branding route as the gateway does for revision 3.
async function serveBrand(page: Page, { stylesheet }: Brand): Promise<void> {
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
          logo: "logo?rev=3",
        },
      })
    } else if (name === "branding.css") {
      await route.fulfill({ contentType: "text/css", body: stylesheet })
    } else if (name === "logo") {
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

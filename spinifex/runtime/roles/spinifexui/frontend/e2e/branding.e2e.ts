import type { Page, Route } from "@playwright/test"

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
  return await page.evaluate(() =>
    getComputedStyle(document.documentElement)
      .getPropertyValue("--primary")
      .trim(),
  )
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

      expect(await appliedPrimary(page)).toBe(colour)
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

import {
  expect,
  expectNoHorizontalOverflow,
  type Theme,
  test,
  useTheme,
} from "./console"

// The plan's four layout breakpoints: wide desktop, iPad landscape and
// portrait, and phone.
const VIEWPORTS = [
  { name: "1440", width: 1440, height: 900 },
  { name: "1024", width: 1024, height: 768 },
  { name: "834", width: 834, height: 1112 },
  { name: "390", width: 390, height: 844 },
]

const THEMES: Theme[] = ["dark", "light"]

test.describe("design catalogue", () => {
  for (const theme of THEMES) {
    for (const viewport of VIEWPORTS) {
      test(`fits ${viewport.name}px in ${theme}`, async ({ api, page }) => {
        await page.setViewportSize(viewport)
        await useTheme(page, theme)
        await page.goto("/design")
        await expect(
          page.getByRole("heading", { level: 1, name: "Design system" }),
        ).toBeVisible()

        await expectNoHorizontalOverflow(page)
        await expect(page.locator("html")).toHaveClass(theme)
        expect(api.unhandled).toStrictEqual([])
      })
    }

    test(`matches the ${theme} baseline`, async ({ api, page }) => {
      await page.setViewportSize({ width: 1440, height: 900 })
      await useTheme(page, theme)
      await page.goto("/design")
      await expect(
        page.getByRole("heading", { level: 1, name: "Design system" }),
      ).toBeVisible()
      await page.evaluate(async () => await document.fonts.ready)

      await expect(page).toHaveScreenshot(`design-${theme}.png`, {
        fullPage: true,
      })
      expect(api.unhandled).toStrictEqual([])
    })
  }
})

test.describe("theme bootstrap", () => {
  test("defaults to dark before the app renders", async ({ api, page }) => {
    await page.goto("/design", { waitUntil: "commit" })
    await page.waitForSelector("html.dark", { state: "attached" })

    // The class is on <html> before React has mounted anything into #app.
    expect(await page.locator("#app > *").count()).toBe(0)
    await expect(page.locator('meta[name="color-scheme"]')).toHaveAttribute(
      "content",
      "dark",
    )
    expect(api.unhandled).toStrictEqual([])
  })

  test("applies a stored light theme and toggles from the header", async ({
    api,
    page,
  }) => {
    await useTheme(page, "light")
    await page.goto("/design")
    await expect(page.locator("html")).toHaveClass("light")

    await page.getByRole("button", { name: /theme/iu }).click()
    await expect(page.locator("html")).toHaveClass("dark")
    expect(
      await page.evaluate(() => localStorage.getItem("spinifex-ui-theme")),
    ).toBe("dark")
    expect(api.unhandled).toStrictEqual([])
  })
})

test.describe("primitives", () => {
  test.beforeEach(async ({ page }) => {
    await page.goto("/design")
    await expect(
      page.getByRole("heading", { level: 1, name: "Design system" }),
    ).toBeVisible()
  })

  test("dialog traps focus, closes on Escape and returns focus", async ({
    page,
  }) => {
    const trigger = page.getByRole("button", { name: "Dialog", exact: true })
    await trigger.click()

    const dialog = page.getByRole("dialog", { name: "Stop instance?" })
    await expect(dialog).toBeVisible()
    await expect(dialog.locator(":focus")).toHaveCount(1)

    await page.keyboard.press("Tab")
    await page.keyboard.press("Tab")
    await page.keyboard.press("Tab")
    await expect(dialog.locator(":focus")).toHaveCount(1)

    await page.keyboard.press("Escape")
    await expect(dialog).toBeHidden()
    await expect(trigger).toBeFocused()
  })

  test("table specimen switches between its states", async ({ page }) => {
    const states = page.getByRole("group").filter({ hasText: "loading" })
    const table = page.getByRole("table")

    await expect(table.getByRole("row")).toHaveCount(6)

    await states.getByRole("button", { name: "empty" }).click()
    await expect(table).toContainText("No instances in this region.")
    await expect(states.getByRole("button", { name: "empty" })).toHaveAttribute(
      "aria-pressed",
      "true",
    )
    await expect(states.getByRole("button", { name: "rows" })).toHaveAttribute(
      "aria-pressed",
      "false",
    )

    await states.getByRole("button", { name: "error" }).click()
    await expect(table).toContainText("the gateway did not answer")

    await states.getByRole("button", { name: "many" }).click()
    await expect(table.getByRole("row")).toHaveCount(61)
  })

  test("a long name truncates instead of widening the table", async ({
    page,
  }) => {
    await page.setViewportSize({ width: 390, height: 844 })
    const states = page.getByRole("group").filter({ hasText: "loading" })
    await states.getByRole("button", { name: "long" }).click()

    await expectNoHorizontalOverflow(page)
  })

  test("command palette filters as you type", async ({ page }) => {
    const input = page.getByPlaceholder("Search resources and commands")
    await input.fill("vpc")

    await expect(page.getByRole("option", { name: /VPCs/u })).toBeVisible()
    await expect(page.getByRole("option", { name: "Create VPC" })).toBeVisible()
    await expect(page.getByRole("option", { name: /Instances/u })).toHaveCount(
      0,
    )

    await input.fill("zzz")
    await expect(page.getByText("No results.")).toBeVisible()
  })

  test("switch and checkbox respond to the keyboard", async ({ page }) => {
    const toggle = page.getByRole("switch").first()
    const before = await toggle.getAttribute("aria-checked")
    await toggle.focus()
    await page.keyboard.press("Space")
    await expect(toggle).not.toHaveAttribute("aria-checked", before ?? "")

    const checkbox = page.getByRole("checkbox").first()
    const checked = await checkbox.getAttribute("aria-checked")
    await checkbox.focus()
    await page.keyboard.press("Space")
    await expect(checkbox).not.toHaveAttribute("aria-checked", checked ?? "")
  })
})

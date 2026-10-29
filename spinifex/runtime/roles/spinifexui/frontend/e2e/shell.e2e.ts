import { expect, expectNoHorizontalOverflow, test } from "./console"

test.describe("command palette", () => {
  test("opens on the shortcut and navigates", async ({ page }) => {
    await page.goto("/")
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible()

    await page.keyboard.press("ControlOrMeta+k")
    const palette = page.getByRole("dialog", { name: "Command palette" })
    await expect(palette).toBeVisible()

    await palette.getByRole("combobox").fill("vpcs")
    await page.keyboard.press("Enter")

    await expect(palette).toBeHidden()
    await expect(page).toHaveURL(/\/ec2\/describe-vpcs$/u)
  })

  test("offers create flows", async ({ page }) => {
    await page.goto("/")
    await page.getByRole("button", { name: /search/iu }).click()
    await page.getByRole("combobox").fill("launch instance")
    await page.keyboard.press("Enter")

    await expect(page).toHaveURL(/\/ec2\/run-instances$/u)
  })
})

test.describe("breadcrumbs", () => {
  test("link a detail page back to its list", async ({ page }) => {
    await page.goto("/ec2/describe-instances/i-0a1b2c3d4e5f60718")

    const trail = page.getByRole("navigation", { name: "breadcrumb" })
    await expect(trail.getByText("EC2")).toBeVisible()
    await expect(trail.getByText("i-0a1b2c3d4e5f60718")).toHaveClass(
      /font-mono/u,
    )

    await trail.getByRole("link", { name: "Instances", exact: true }).click()
    await expect(page).toHaveURL(/\/ec2\/describe-instances$/u)
  })
})

test.describe("sidebar", () => {
  test("remembers a folded group across reloads", async ({ page }) => {
    await page.goto("/")
    const sidebar = page.locator("[data-slot=sidebar]")
    const s3 = sidebar.getByRole("button", { name: "S3", exact: true })

    await expect(
      sidebar.getByRole("link", { name: "Buckets", exact: true }),
    ).toBeVisible()
    await s3.click()
    await expect(
      sidebar.getByRole("link", { name: "Buckets", exact: true }),
    ).toBeHidden()

    await page.reload()
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible()
    await expect(
      sidebar.getByRole("link", { name: "Instances", exact: true }),
    ).toBeVisible()
    await expect(
      sidebar.getByRole("link", { name: "Buckets", exact: true }),
    ).toBeHidden()
  })

  test("keeps the active group open even when folded", async ({ page }) => {
    await page.goto("/ec2/describe-vpcs")
    const sidebar = page.locator("[data-slot=sidebar]")

    await sidebar.getByRole("button", { name: "EC2", exact: true }).click()
    await expect(
      sidebar.getByRole("link", { name: "VPCs", exact: true }),
    ).toHaveAttribute("data-active")
  })

  test("opens as a sheet on a phone and closes on navigation", async ({
    page,
  }) => {
    await page.setViewportSize({ width: 390, height: 844 })
    await page.goto("/")
    await expectNoHorizontalOverflow(page)

    await page.getByRole("button", { name: "Toggle Sidebar" }).click()
    const sheet = page.getByRole("dialog", { name: "Sidebar" })
    await expect(sheet).toBeVisible()

    await sheet.getByRole("link", { name: "VPCs", exact: true }).click()
    await expect(page).toHaveURL(/\/ec2\/describe-vpcs$/u)
    await expect(sheet).toBeHidden()
  })
})

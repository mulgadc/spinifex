import {
  expect,
  expectNoHorizontalOverflow,
  STS_NAMESPACE,
  test,
  xml,
} from "./console"

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

test.describe("sign in", () => {
  test("keeps the shell in view but out of reach", async ({ page }) => {
    await page.goto("/")
    const sidebar = page.locator("[data-slot=sidebar]")
    await sidebar.getByRole("button", { name: "Sign out" }).click()

    await expect(page).toHaveURL(/\/login/u)
    await expect(page.getByLabel("Access Key ID")).toBeVisible()
    await expect(page.getByRole("status")).toHaveText("Signed out.")
    await expect(sidebar).toBeVisible()
    await expect(page.locator("[inert] [data-slot=sidebar]")).toHaveCount(1)

    // Backwards from the first field would land in the header or sidebar.
    await page.getByLabel("Access Key ID").focus()
    await page.keyboard.press("Shift+Tab")
    await expect(page.locator("[inert] :focus")).toHaveCount(0)
  })
})

test.describe("signing in", () => {
  test("fades into the console without reloading the page", async ({
    api,
    page,
  }) => {
    api.handle("GetSessionToken", () => ({
      body: xml(
        "GetSessionToken",
        "<GetSessionTokenResult><Credentials><AccessKeyId>ASIAE2EFORM00000000</AccessKeyId><SecretAccessKey>e2e-form-secret</SecretAccessKey><SessionToken>e2e-form-token</SessionToken><Expiration>2099-01-01T00:00:00Z</Expiration></Credentials></GetSessionTokenResult>",
        STS_NAMESPACE,
      ),
    }))

    await page.goto("/")
    await page
      .locator("[data-slot=sidebar]")
      .getByRole("button", { name: "Sign out" })
      .click()
    await page.evaluate(() => {
      document.documentElement.dataset.e2eStayed = "1"
    })

    await page.getByLabel("Access Key ID").fill("AKIAE2EFORM000000000")
    await page.getByLabel("Secret Access Key").fill("e2e-form-secret")
    await page.keyboard.press("Enter")

    await expect(page).toHaveURL(/\/$/u)
    await expect(page.getByLabel("Access Key ID")).toBeHidden()
    await expect(page.locator("[inert]")).toHaveCount(0)
    await expect(page.locator("html")).toHaveAttribute("data-e2e-stayed", "1")
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

  test("folds the active group, and unfolds a group on entering it", async ({
    page,
  }) => {
    await page.goto("/ec2/describe-vpcs")
    const sidebar = page.locator("[data-slot=sidebar]")
    const vpcs = sidebar.getByRole("link", { name: "VPCs", exact: true })
    await expect(vpcs).toHaveAttribute("data-active")

    await sidebar
      .getByRole("button", { name: "Networking", exact: true })
      .click()
    await expect(vpcs).toBeHidden()

    await sidebar.getByRole("link", { name: "Dashboard", exact: true }).click()
    await expect(vpcs).toBeHidden()

    await page.goto("/ec2/describe-vpcs")
    await expect(vpcs).toBeVisible()
    await expect(vpcs).toHaveAttribute("data-active")
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

import { CONSOLE_CSP, ec2, expect, liveMetrics, test } from "./console"

// Vite dev applies no CSP, so a library that injects inline script or style
// works there and breaks in the shipped console. This runs the built dist with
// the console's own header and fails on any violation.
test("the built console renders under its CSP", async ({ api, page }) => {
  const violations: string[] = []
  page.on("console", (message) => {
    if (message.text().includes("Content Security Policy")) {
      const { url, lineNumber } = message.location()
      violations.push(`${url}:${lineNumber}: ${message.text().slice(0, 120)}`)
    }
  })

  await page.route(
    (url) =>
      !url.pathname.startsWith("/api/") && !url.pathname.startsWith("/proxy/"),
    async (route) => {
      if (route.request().resourceType() !== "document") {
        await route.fallback()

        return
      }

      const response = await route.fetch()
      await route.fulfill({
        response,
        headers: {
          ...response.headers(),
          "content-security-policy": CONSOLE_CSP,
        },
      })
    },
  )

  await page.goto("/")
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible()

  // A release has no design catalogue; the palette is its overlay in use.
  await page.getByRole("button", { name: /theme/iu }).click()
  await page.keyboard.press("ControlOrMeta+k")
  await expect(
    page.getByRole("dialog", { name: "Command palette" }),
  ).toBeVisible()
  await page.keyboard.press("Escape")

  await page.goto("/design")
  await expect(page.getByText("Page not found")).toBeVisible()

  // The charts draw through uPlot, the one library here that styles the DOM.
  api.handle(
    "DescribeInstances",
    ec2(
      "DescribeInstances",
      `<reservationSet><item><reservationId>r-csp</reservationId><instancesSet><item>
<instanceId>i-0c5p000000000001</instanceId><instanceState><code>16</code><name>running</name></instanceState>
<instanceType>t3.micro</instanceType><launchTime>2026-10-20T09:00:00Z</launchTime>
</item></instancesSet></item></reservationSet>`,
    ),
  )
  api.handle("GetInstanceMetrics", liveMetrics())
  await page.goto(
    "/ec2/describe-instances?selected=i-0c5p000000000001&tab=monitoring",
  )
  await expect(
    page.getByRole("figure", { name: "CPU utilization (%)" }).locator("canvas"),
  ).toBeVisible()

  expect(api.unhandled).toStrictEqual([])
  expect(violations).toStrictEqual([])
})

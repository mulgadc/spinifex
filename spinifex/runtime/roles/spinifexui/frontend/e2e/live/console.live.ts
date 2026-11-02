import type { Page } from "@playwright/test"

import { expect, liveConfig, test } from "./live"

const RUNNING_TIMEOUT = 5 * 60_000
const TERMINATED_TIMEOUT = 5 * 60_000
const GRAPH_TIMEOUT = 60_000
const APPEAR_TIMEOUT = 60_000
const MINIMUM_GRAPHS = 3

// The banner the console shows when the capacity listing comes back empty.
const NO_COMPUTE =
  "No compute available. No new instances can be created until compute is available."

// The launch form opens in the instance list's side panel, so the table and
// the form are on one page and a launch can be read from the list directly.
const LAUNCH_PANEL = "/ec2/describe-instances?create=true"

async function openLaunchForm(page: Page): Promise<void> {
  await page.goto(LAUNCH_PANEL)
  await expect(
    page.getByRole("button", { name: "Create Instance" }),
  ).toBeVisible({ timeout: APPEAR_TIMEOUT })
}

async function instanceIds(page: Page): Promise<string[]> {
  const cells = await page
    .getByRole("table", { name: "Instances" })
    .getByRole("cell")
    .filter({ hasText: /^i-[0-9a-f]+$/ })
    .allInnerTexts()

  return cells.map((text) => text.trim())
}

async function selectOption(
  page: Page,
  trigger: string,
  match?: RegExp,
): Promise<void> {
  await page.locator(trigger).click()
  const options = page.getByRole("option")
  const wanted = match ? options.filter({ hasText: match }) : options
  await wanted.first().click()
}

async function terminate(page: Page, instanceId: string): Promise<void> {
  await page.goto("/ec2/describe-instances")
  await page.getByRole("checkbox", { name: `Select ${instanceId}` }).check()
  await page.getByRole("button", { name: "Terminate", exact: true }).click()

  // The dialog holds its confirm disabled until the phrase is typed, as AWS
  // does before an irreversible delete.
  await page
    .getByRole("textbox", { name: /type .terminate./ })
    .fill("terminate")
  await page.getByRole("button", { name: /^Terminate \d+$/ }).click()
}

// Signing in with an access key is the only way into the console, so every
// other check depends on this one.
test("an operator signs in with an access key and reaches the dashboard", async ({
  signedIn,
}) => {
  await signedIn.goto("/")
  await expect(signedIn.getByRole("link", { name: "Dashboard" })).toBeVisible({
    timeout: APPEAR_TIMEOUT,
  })

  // The nodes page is where the cluster's own capacity is reported, from the
  // daemon's node status rather than from the EC2 listing.
  await signedIn.goto("/nodes")
  await expect(
    signedIn.getByRole("table", { name: "Resource usage by node" }),
  ).toBeVisible({ timeout: APPEAR_TIMEOUT })
  await expect(
    signedIn.getByRole("table", { name: "Instance type capacity" }),
  ).toBeVisible()
})

// The regression guard for the defect this suite was written for: a cluster
// with free capacity must not report that it has none. The mocked suite
// cannot see this, because the fault was in what the control plane sent.
test("a cluster with free capacity offers instance types rather than reporting none", async ({
  signedIn,
}) => {
  await openLaunchForm(signedIn)

  await expect(signedIn.getByText(NO_COMPUTE)).toHaveCount(0)
  await expect(
    signedIn.getByRole("button", { name: "Create Instance" }),
  ).toBeEnabled()

  await signedIn.locator("#instanceType").click()
  await expect(signedIn.getByRole("option").first()).toBeVisible()
})

// The console reports the cluster's true free capacity, so a figure capped to
// keep a response small would fail here rather than mislead a customer.
test("the launch form reports a capacity figure for the selected type", async ({
  signedIn,
}) => {
  const config = liveConfig()
  await openLaunchForm(signedIn)
  await selectOption(signedIn, "#instanceType", new RegExp(config.instanceType))

  const capacity = signedIn.getByText(
    new RegExp(`Available capacity for ${config.instanceType}: \\d+`),
  )

  await expect(capacity).toBeVisible()

  const text = (await capacity.textContent()) ?? ""
  const match = /: (?<slots>\d+)/.exec(text)
  expect(Number(match?.groups?.slots ?? 0)).toBeGreaterThan(0)
})

// The whole customer path in one test, because the parts are only worth
// anything together: launch, reach running, read live metrics, terminate.
test("an instance launches, serves metrics and terminates", async ({
  signedIn,
}) => {
  const config = liveConfig()
  await openLaunchForm(signedIn)
  const before = new Set(await instanceIds(signedIn))
  let launched = ""

  try {
    // The OS tiles put a visually-hidden radio inside its label, so the click
    // goes to the label and the browser forwards it. The first tile picks a
    // distribution at its newest version for the architecture.
    await signedIn.locator('label:has(input[type="radio"])').first().click()
    await selectOption(
      signedIn,
      "#instanceType",
      new RegExp(config.instanceType),
    )
    await selectOption(signedIn, "#keyName")
    await signedIn.getByRole("button", { name: "Create Instance" }).click()

    await expect
      .poll(
        async () => {
          const now = await instanceIds(signedIn)
          launched = now.find((id) => !before.has(id)) ?? ""

          return launched
        },
        { timeout: APPEAR_TIMEOUT },
      )
      .not.toBe("")

    const row = signedIn.getByRole("row").filter({ hasText: launched })
    await expect(row.getByText("running")).toBeVisible({
      timeout: RUNNING_TIMEOUT,
    })

    // Monitoring is the only page that reads the metrics stream, so a graph
    // rendering is the evidence the guest is being sampled.
    await signedIn.goto(`/ec2/describe-instances/${launched}?tab=monitoring`)
    await expect(
      signedIn.getByRole("region", { name: "Instance details" }),
    ).toBeVisible()
    await expect
      .poll(async () => await signedIn.getByRole("figure").count(), {
        timeout: GRAPH_TIMEOUT,
      })
      .toBeGreaterThanOrEqual(MINIMUM_GRAPHS)
    await expect(
      signedIn.getByRole("figure", { name: /CPU utilization/i }),
    ).toBeVisible()
  } finally {
    if (launched) {
      await terminate(signedIn, launched)
      await expect(
        signedIn
          .getByRole("row")
          .filter({ hasText: launched })
          .getByText(/terminated|shutting-down/),
      ).toBeVisible({ timeout: TERMINATED_TIMEOUT })
    }
  }
})

// The pages a customer reaches from the sidebar, each loading its own listing
// against the live control plane. A page that throws renders no table.
const LISTINGS = [
  ["volumes", "/ec2/describe-volumes", "Volumes"],
  ["VPCs", "/ec2/describe-vpcs", "VPCs"],
  ["subnets", "/ec2/describe-subnets", "Subnets"],
  ["security groups", "/ec2/describe-security-groups", "Security groups"],
  ["key pairs", "/ec2/describe-key-pairs", "Key pairs"],
  ["images", "/ec2/describe-images", "Images"],
  ["elastic IPs", "/ec2/describe-addresses", "Elastic IPs"],
  ["snapshots", "/ec2/describe-snapshots", "Snapshots"],
  ["load balancers", "/ec2/describe-load-balancers", "Load balancers"],
] as const

for (const [name, path, table] of LISTINGS) {
  test(`the ${name} listing loads against the cluster`, async ({
    signedIn,
  }) => {
    await signedIn.goto(path)
    await expect(signedIn.getByRole("table", { name: table })).toBeVisible({
      timeout: APPEAR_TIMEOUT,
    })
  })
}

import {
  ec2,
  expect,
  expectNoHorizontalOverflow,
  instanceTypeCapacity,
  liveMetrics,
  test,
  type Theme,
  useTheme,
} from "./console"

interface FakeInstance {
  id: string
  name?: string
  state: FakeState
  type: string
  privateIp: string
  publicIp?: string
  launched: string
  tags?: Record<string, string>
}

const INSTANCES: FakeInstance[] = [
  {
    id: "i-0a1b2c3d4e5f60001",
    name: "web-01",
    state: "running",
    type: "t3.micro",
    privateIp: "10.200.0.4",
    publicIp: "72.52.77.240",
    launched: "2026-10-20T09:00:00Z",
  },
  {
    id: "i-0a1b2c3d4e5f60002",
    name: "web-02",
    state: "running",
    type: "t3.micro",
    privateIp: "10.200.0.10",
    tags: {
      "spinifex:eks-cluster": "test-cluster",
      "spinifex:eks-nodegroup": "node-gap",
    },
    launched: "2026-10-21T09:00:00Z",
  },
  {
    id: "i-0a1b2c3d4e5f60003",
    name: "db-primary",
    state: "stopped",
    type: "t3.xlarge",
    privateIp: "10.200.0.9",
    launched: "2026-10-01T09:00:00Z",
  },
  {
    id: "i-0a1b2c3d4e5f60004",
    state: "pending",
    type: "t3.small",
    privateIp: "10.200.0.12",
    launched: "2026-10-28T09:00:00Z",
  },
  {
    id: "i-0a1b2c3d4e5f60005",
    name: "a-very-long-instance-name-that-a-customer-chose-for-their-production-database-replica",
    state: "running",
    type: "t3.large",
    privateIp: "10.200.0.20",
    launched: "2026-10-22T09:00:00Z",
  },
]

type FakeState = "pending" | "running" | "stopped"

const STATE_CODES = {
  pending: 0,
  running: 16,
  stopped: 80,
} satisfies Record<FakeState, number>

function instanceXml(instance: FakeInstance): string {
  const pairs = [
    ...(instance.name ? [["Name", instance.name]] : []),
    ...Object.entries(instance.tags ?? {}),
  ]

  const tags =
    pairs.length > 0
      ? `<tagSet>${pairs.map(([key, value]) => `<item><key>${key}</key><value>${value}</value></item>`).join("")}</tagSet>`
      : ""

  return `<item><instanceId>${instance.id}</instanceId><imageId>ami-ubuntu-26.04-x86_64</imageId>
<instanceState><code>${STATE_CODES[instance.state]}</code><name>${instance.state}</name></instanceState>
<instanceType>${instance.type}</instanceType><launchTime>${instance.launched}</launchTime>
<placement><availabilityZone>ap-southeast-2a</availabilityZone></placement>
<privateIpAddress>${instance.privateIp}</privateIpAddress>${instance.publicIp ? `<ipAddress>${instance.publicIp}</ipAddress>` : ""}${tags}</item>`
}

function describeInstances(instances: FakeInstance[]) {
  return ec2(
    "DescribeInstances",
    `<reservationSet><item><reservationId>r-e2e</reservationId><ownerId>000000000001</ownerId><instancesSet>${instances.map(instanceXml).join("")}</instancesSet></item></reservationSet>`,
  )
}

function idsIn(params: URLSearchParams): string[] {
  return [...params.entries()].flatMap(([key, value]) =>
    key.startsWith("InstanceId.") ? [value] : [],
  )
}

test.describe("instances list", () => {
  test.beforeEach(({ api }) => {
    api.handle("DescribeInstances", describeInstances(INSTANCES))
  })

  test("names each instance and opens it in the side panel", async ({
    page,
  }) => {
    await page.goto("/ec2/describe-instances")
    const table = page.getByRole("table", { name: "Instances" })

    await expect(table.getByRole("row")).toHaveCount(INSTANCES.length + 1)
    await expect(table.getByRole("columnheader", { name: "Zone" })).toHaveCount(
      0,
    )

    const web01 = table.getByRole("row").filter({ hasText: "web-01" })
    await expect(
      web01.getByRole("link", { name: "i-0a1b2c3d4e5f60001" }),
    ).toBeVisible()
    await expect(web01.getByRole("cell", { name: "web-01" })).toBeVisible()
    await expect(web01.getByText(/ ago$/u)).toBeVisible()

    // An instance with no Name tag is reached by its ID.
    await table.getByRole("link", { name: "i-0a1b2c3d4e5f60004" }).click()
    await expect(page).toHaveURL(/\?selected=i-0a1b2c3d4e5f60004$/u)

    const panel = page.getByRole("region", { name: "Instance details" })
    await expect(
      panel.getByRole("heading", { name: "i-0a1b2c3d4e5f60004" }),
    ).toBeVisible()
    await expect(
      table.getByRole("row").filter({ hasText: "i-0a1b2c3d4e5f60004" }),
    ).toHaveAttribute("data-active", "")

    // The panel's own page outlives the list it was opened from.
    await panel.getByRole("link", { name: "Open instance page" }).click()
    await expect(page).toHaveURL(
      /\/ec2\/describe-instances\/i-0a1b2c3d4e5f60004$/u,
    )
  })

  test("renders the panel's tabs as the instance's own page", async ({
    api,
    page,
  }) => {
    api.handle("GetInstanceMetrics", liveMetrics())
    await page.goto("/ec2/describe-instances/i-0a1b2c3d4e5f60001")
    const details = page.getByRole("region", { name: "Instance details" })

    await expect(details.getByRole("heading", { name: "web-01" })).toBeVisible()
    await expect(page.getByRole("table", { name: "Instances" })).toBeHidden()
    await expect(
      details.getByRole("link", { name: "Open instance page" }),
    ).toBeHidden()
    await expect(details.getByText(/^ssh -i /u)).toBeVisible()

    await details.getByRole("tab", { name: "Monitoring" }).click()
    await expect(page).toHaveURL(/\?tab=monitoring$/u)
    await expect(details.getByRole("figure")).toHaveCount(3)
    // A running instance streams; the newest sample is what each chart reads.
    await expect(details.getByText("Live", { exact: true })).toBeVisible()
    await expect(
      details.getByRole("figure", { name: "CPU utilization (%)" }),
    ).toContainText("42%")

    await details.getByRole("button", { name: "Back to instances" }).click()
    await expect(page).toHaveURL(/\/ec2\/describe-instances$/u)
  })

  test("closes the panel on a click in the list's background", async ({
    page,
  }) => {
    await page.goto("/ec2/describe-instances?selected=i-0a1b2c3d4e5f60001")
    const panel = page.getByRole("region", { name: "Instance details" })
    await expect(panel.getByText(/^ssh -i /u)).toBeVisible()

    // A row selects another instance rather than closing.
    await page
      .getByRole("row")
      .filter({ hasText: "db-primary" })
      .getByRole("cell", { name: "t3.xlarge" })
      .click()
    await expect(
      panel.getByRole("heading", { name: "db-primary" }),
    ).toBeVisible()

    await panel.getByRole("tab", { name: "Settings" }).click()
    await expect(panel).toBeVisible()

    await page.getByText("Virtual machines running on this cluster.").click()
    await expect(panel).toBeHidden()
    await expect(page).toHaveURL(/\/ec2\/describe-instances$/u)
  })

  test("opens a row's panel on click and keeps the tab in the URL", async ({
    page,
  }) => {
    await page.goto("/ec2/describe-instances")
    await page
      .getByRole("table", { name: "Instances" })
      .getByRole("row")
      .filter({ hasText: "db-primary" })
      .getByRole("cell", { name: "t3.xlarge" })
      .click()

    const panel = page.getByRole("region", { name: "Instance details" })
    await expect(
      panel.getByRole("heading", { name: "db-primary" }),
    ).toBeVisible()
    // AWS's launch-time form, in the viewer's zone: UTC under test.
    await expect(panel.getByText("2026/10/01 09:00 GMT")).toBeVisible()
    await expect(panel.getByRole("button", { name: "Start" })).toBeEnabled()
    await expect(panel.getByRole("button", { name: "Stop" })).toBeDisabled()

    await panel.getByRole("tab", { name: "Monitoring" }).click()
    await expect(page).toHaveURL(/tab=monitoring/u)
    await expect(panel.getByRole("figure")).toHaveCount(3)
    await expect(
      panel.getByRole("figure", { name: "CPU utilization (%)" }),
    ).toBeVisible()
    await expect(
      panel.getByRole("figure", { name: "Network packets" }),
    ).toBeVisible()
    await expect(
      panel.getByText("Live statistics need a running instance."),
    ).toBeVisible()

    await page.reload()
    await expect(
      page
        .getByRole("region", { name: "Instance details" })
        .getByRole("tab", { name: "Monitoring" }),
    ).toHaveAttribute("aria-selected", "true")

    // Opening the panel was a history entry and a tab change was not, so one
    // Back walks out of it.
    await page.goBack()
    await expect(
      page.getByRole("region", { name: "Instance details" }),
    ).toBeHidden()
    await expect(page).toHaveURL(/\/ec2\/describe-instances$/u)
  })

  test("keeps the tab and the console output when the panel expands", async ({
    api,
    page,
  }) => {
    const log = "[    1.310146] pci 0000:00:08.0: enabling Extended Tags"
    api.handle(
      "GetConsoleOutput",
      ec2(
        "GetConsoleOutput",
        `<instanceId>i-0a1b2c3d4e5f60001</instanceId><output>${btoa(log)}</output>`,
      ),
    )
    await page.goto("/ec2/describe-instances?selected=i-0a1b2c3d4e5f60001")
    const panel = page.getByRole("region", { name: "Instance details" })

    await panel.getByRole("button", { name: "Actions" }).click()
    await page.getByRole("menuitem", { name: "View console output" }).click()
    await expect(page).toHaveURL(/tab=settings/u)
    await expect(panel.getByText("enabling Extended Tags")).toBeVisible()
    await expect(panel.getByText("[    1.310146]")).toHaveAttribute(
      "data-tone",
      "stamp",
    )

    await panel.getByRole("link", { name: "Open instance page" }).click()
    await expect(page).toHaveURL(
      /\/ec2\/describe-instances\/i-0a1b2c3d4e5f60001\?tab=settings$/u,
    )
    await expect(page.getByText("enabling Extended Tags")).toBeVisible()
  })

  test("asks before stopping or rebooting", async ({ page }) => {
    await page.goto("/ec2/describe-instances?selected=i-0a1b2c3d4e5f60001")
    const panel = page.getByRole("region", { name: "Instance details" })

    await panel.getByRole("button", { name: "Stop", exact: true }).click()
    const stop = page.getByRole("alertdialog", { name: "Stop instance?" })
    await expect(stop).toBeVisible()
    await stop.getByRole("button", { name: "Cancel" }).click()
    await expect(stop).toBeHidden()

    await panel.getByRole("button", { name: "Reboot", exact: true }).click()
    await expect(
      page.getByRole("alertdialog", { name: "Reboot instance?" }),
    ).toBeVisible()
  })

  test("closes the panel when rows are selected", async ({ page }) => {
    await page.goto("/ec2/describe-instances?selected=i-0a1b2c3d4e5f60001")
    const panel = page.getByRole("region", { name: "Instance details" })
    await expect(panel).toBeVisible()

    await page
      .getByRole("row")
      .filter({ hasText: "db-primary" })
      .getByRole("checkbox")
      .click()
    await expect(panel).toBeHidden()
    await expect(
      page.getByRole("toolbar", { name: "Selection actions" }),
    ).toBeVisible()
  })

  test("closes the panel from its close button", async ({ page }) => {
    await page.goto("/ec2/describe-instances?selected=i-0a1b2c3d4e5f60001")
    await page.getByRole("button", { name: "Close instance details" }).click()
    await expect(
      page.getByRole("region", { name: "Instance details" }),
    ).toBeHidden()
    await expect(page).toHaveURL(/\/ec2\/describe-instances$/u)
  })

  test("launches from a panel beside the list", async ({ page }) => {
    await page.goto("/ec2/describe-instances")
    await page.getByRole("link", { name: "Launch instance" }).click()
    await expect(page).toHaveURL(/\?create=true$/u)

    const panel = page.getByRole("region", { name: "Launch instance" })
    await expect(panel).toBeVisible()
    await expect(page.getByRole("table", { name: "Instances" })).toBeVisible()

    await panel.getByRole("button", { name: "Close create form" }).click()
    await expect(panel).toBeHidden()
  })

  test("shows the panel as its own page on a phone", async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 })
    await page.goto("/ec2/describe-instances")
    const table = page.getByRole("table", { name: "Instances" })
    await expect(
      table.getByRole("columnheader", { name: "Name" }),
    ).toBeVisible()
    await expect(
      table.getByRole("columnheader", { name: "State" }),
    ).toBeVisible()
    await expect(table.getByRole("columnheader", { name: "Type" })).toBeHidden()
    await expectNoHorizontalOverflow(page)

    await page.getByRole("link", { name: "i-0a1b2c3d4e5f60002" }).click()

    const panel = page.getByRole("region", { name: "Instance details" })
    await expect(panel).toBeVisible()
    await expect(page.getByRole("table", { name: "Instances" })).toBeHidden()
    await expectNoHorizontalOverflow(page)

    await panel.getByRole("button", { name: "Close instance details" }).click()
    await expect(page.getByRole("table", { name: "Instances" })).toBeVisible()
  })

  test("filters by text and state, and keeps both in the URL", async ({
    page,
  }) => {
    await page.goto("/ec2/describe-instances")
    const rows = page.getByRole("table", { name: "Instances" }).getByRole("row")

    await page.getByRole("searchbox", { name: "Filter instances" }).fill("web")
    await expect(rows).toHaveCount(3)

    await page.getByRole("button", { name: /^running/iu }).click()
    await expect(page).toHaveURL(/q=web/u)
    await expect(page).toHaveURL(/state=running/u)

    await page.reload()
    await expect(
      page.getByRole("searchbox", { name: "Filter instances" }),
    ).toHaveValue("web")
    await expect(rows).toHaveCount(3)

    await page.getByRole("searchbox", { name: "Filter instances" }).fill("zzz")
    await expect(
      page.getByText("No instances match these filters."),
    ).toBeVisible()

    await page.getByRole("button", { name: "Clear filters" }).click()
    await expect(rows).toHaveCount(INSTANCES.length + 1)
  })

  test("shows tags other than Name as one pill and a count", async ({
    page,
  }) => {
    await page.goto("/ec2/describe-instances")

    const row = page
      .getByRole("table", { name: "Instances" })
      .getByRole("row")
      .filter({ hasText: "web-02" })

    await expect(row.getByText("spinifex:eks-cluster")).toBeVisible()
    await expect(row.getByText("+1")).toBeVisible()

    await row.getByText("spinifex:eks-cluster").hover()
    await expect(page.getByText("node-gap")).toBeVisible()
  })

  test("keeps a long name to its column, in full on hover", async ({
    page,
  }) => {
    await page.goto("/ec2/describe-instances")
    const long = INSTANCES[4]?.name ?? ""

    const cell = page
      .getByRole("table", { name: "Instances" })
      .getByRole("row")
      .filter({ hasText: "i-0a1b2c3d4e5f60005" })
      .getByText(long)

    const box = await cell.boundingBox()
    expect(box?.width).toBeLessThan(260)

    await cell.hover()
    await expect(page.locator("[data-slot=tooltip-content]")).toContainText(
      long,
    )
  })

  test("counts each state on its filter", async ({ page }) => {
    await page.goto("/ec2/describe-instances")
    const states = page.getByRole("group", { name: "Instance state" })

    await expect(states.getByRole("button", { name: "all 5" })).toBeVisible()
    await expect(
      states.getByRole("button", { name: "running 3" }),
    ).toBeVisible()
    await expect(
      states.getByRole("button", { name: "stopped 1" }),
    ).toBeVisible()
    await expect(states.getByRole("button", { name: "other 1" })).toBeVisible()
  })

  test("sorts by a column and orders addresses numerically", async ({
    page,
  }) => {
    await page.goto("/ec2/describe-instances?sort=private-ip")
    const header = page.getByRole("columnheader", { name: /private ip/iu })

    await expect(header).toHaveAttribute("aria-sort", "ascending")

    const addresses = page
      .getByRole("table", { name: "Instances" })
      .getByRole("cell")
      .filter({ hasText: /^10\.200\.0\.\d+$/u })

    await expect(addresses).toHaveText([
      "10.200.0.4",
      "10.200.0.9",
      "10.200.0.10",
      "10.200.0.12",
      "10.200.0.20",
    ])

    await header.getByRole("button").click()
    await expect(header).toHaveAttribute("aria-sort", "descending")
    await expect(page).toHaveURL(/sort=-private-ip/u)
  })

  test("stops only the selected instances that are running", async ({
    api,
    page,
  }) => {
    const stopped: string[][] = []
    api.handle("StopInstances", (params) => {
      stopped.push(idsIn(params))

      return ec2("StopInstances", "<instancesSet/>")(params)
    })

    await page.goto("/ec2/describe-instances")
    await page
      .getByRole("checkbox", { name: "Select i-0a1b2c3d4e5f60001" })
      .click()
    await page
      .getByRole("checkbox", { name: "Select i-0a1b2c3d4e5f60003" })
      .click()

    const toolbar = page.getByRole("toolbar", { name: "Selection actions" })
    await expect(toolbar.getByText("2 selected")).toBeVisible()
    await expect(
      toolbar.getByRole("button", { name: "Reboot 1" }),
    ).toBeEnabled()

    await toolbar.getByRole("button", { name: /^Stop/u }).click()
    await expect(toolbar).toBeHidden()
    expect(stopped).toStrictEqual([["i-0a1b2c3d4e5f60001"]])
  })

  test("asks before terminating and names every instance", async ({
    api,
    page,
  }) => {
    const terminated: string[][] = []
    api.handle("TerminateInstances", (params) => {
      terminated.push(idsIn(params))

      return ec2("TerminateInstances", "<instancesSet/>")(params)
    })

    await page.goto("/ec2/describe-instances")
    await page.getByRole("checkbox", { name: "Select all rows" }).click()
    await page.getByRole("button", { name: "Terminate", exact: true }).click()

    const dialog = page.getByRole("alertdialog")
    await expect(dialog).toContainText("Terminate 5 instances?")
    await expect(dialog).toContainText("i-0a1b2c3d4e5f60005")

    await dialog.getByRole("button", { name: "Cancel" }).click()
    expect(terminated).toStrictEqual([])

    await page.getByRole("button", { name: "Terminate", exact: true }).click()
    const terminate = dialog.getByRole("button", { name: "Terminate 5" })
    await expect(terminate).toBeDisabled()

    await dialog.getByLabel(/^To confirm, type/u).fill("terminate")
    await terminate.click()
    await expect(page.getByRole("alertdialog")).toBeHidden()
    expect(terminated).toHaveLength(1)
    expect(terminated[0]).toHaveLength(5)
  })

  test("fits a phone, dropping secondary columns", async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 })
    await page.goto("/ec2/describe-instances")

    await expect(
      page.getByRole("columnheader", { name: /state/iu }),
    ).toBeVisible()
    await expect(
      page.getByRole("columnheader", { name: /private ip/iu }),
    ).toBeHidden()
    await expectNoHorizontalOverflow(page)
  })

  for (const theme of ["dark", "light"] satisfies Theme[]) {
    test(`matches the ${theme} baseline`, async ({ page }) => {
      await useTheme(page, theme)
      await page.setViewportSize({ width: 1440, height: 900 })
      await page.goto("/ec2/describe-instances")
      await page
        .getByRole("checkbox", { name: "Select i-0a1b2c3d4e5f60002" })
        .click()
      await page.evaluate(async () => await document.fonts.ready)
      await expect(page).toHaveScreenshot(`instances-${theme}.png`)
    })

    test(`matches the ${theme} panel baseline`, async ({ page }) => {
      await useTheme(page, theme)
      await page.setViewportSize({ width: 1440, height: 900 })
      await page.goto("/ec2/describe-instances?selected=i-0a1b2c3d4e5f60001")
      await expect(
        page.getByRole("region", { name: "Instance details" }),
      ).toBeVisible()
      await page.evaluate(async () => await document.fonts.ready)
      await expect(page).toHaveScreenshot(`instances-panel-${theme}.png`)
    })
  }
})

test("an empty account is offered a first launch", async ({ page }) => {
  await page.goto("/ec2/describe-instances")

  await expect(page.getByText("No instances yet.")).toBeVisible()
  await page.getByRole("link", { name: "Launch your first instance" }).click()
  await expect(page).toHaveURL(/\/ec2\/describe-instances\?create=true$/u)
})

// The launch form decides whether the cluster has compute from the instance
// type listing alone, so it says "no compute" for an empty list whatever the
// reason the list is empty. An idle production cluster hit exactly that: the
// node replies were dropped and the empty result read as no capacity.
const NO_COMPUTE =
  "No compute available. No new instances can be created until compute is available."

function instanceTypeSet(types: readonly [string, number, number][]): string {
  const items = types.map(
    ([name, vcpu, memMiB]) =>
      `<item><instanceType>${name}</instanceType>` +
      `<vCpuInfo><defaultVCpus>${vcpu}</defaultVCpus></vCpuInfo>` +
      `<memoryInfo><sizeInMiB>${memMiB}</sizeInMiB></memoryInfo>` +
      `<processorInfo><supportedArchitectures><item>x86_64</item></supportedArchitectures></processorInfo>` +
      `</item>`,
  )

  return `<instanceTypeSet>${items.join("")}</instanceTypeSet>`
}

test("the launch form offers the types the capacity listing reports", async ({
  api,
  page,
}) => {
  api.handle(
    "DescribeInstanceTypes",
    ec2("DescribeInstanceTypes", instanceTypeSet([["t3.micro", 2, 1024]])),
  )
  api.handle("GetInstanceTypeCapacity", instanceTypeCapacity({ "t3.micro": 3 }))

  await page.goto("/ec2/describe-instances?create=true")

  await expect(page.getByText(NO_COMPUTE)).toHaveCount(0)
  await expect(
    page.getByRole("button", { name: "Create Instance" }),
  ).toBeEnabled()
})

test("the launch form reports the free slot count for the selected type", async ({
  api,
  page,
}) => {
  api.handle(
    "DescribeInstanceTypes",
    ec2("DescribeInstanceTypes", instanceTypeSet([["t3.micro", 2, 1024]])),
  )
  api.handle("GetInstanceTypeCapacity", instanceTypeCapacity({ "t3.micro": 2 }))

  await page.goto("/ec2/describe-instances?create=true")
  await page.locator("#instanceType").click()
  await page.getByRole("option").filter({ hasText: "t3.micro" }).first().click()

  await expect(
    page.getByText("Available capacity for t3.micro: 2"),
  ).toBeVisible()
})

// A session whose policy does not grant the capacity action reads no figure.
// Reporting a cap of 1 there would refuse a launch the cluster would take, so
// the form claims nothing and RunInstances decides.
test("a type with no capacity figure is offered without one", async ({
  api,
  page,
}) => {
  api.handle(
    "DescribeInstanceTypes",
    ec2("DescribeInstanceTypes", instanceTypeSet([["t3.micro", 2, 1024]])),
  )
  api.handle("GetInstanceTypeCapacity", () => ({
    status: 403,
    contentType: "text/xml",
    body: `<ErrorResponse><Error><Code>AccessDenied</Code><Message>no grant</Message></Error><RequestId>e2e</RequestId></ErrorResponse>`,
  }))

  await page.goto("/ec2/describe-instances?create=true")
  await page.locator("#instanceType").click()
  await page.getByRole("option").filter({ hasText: "t3.micro" }).first().click()

  await expect(page.getByText(NO_COMPUTE)).toHaveCount(0)
  await expect(page.getByText(/Available capacity for/u)).toHaveCount(0)
  await expect(
    page.getByRole("button", { name: "Create Instance" }),
  ).toBeEnabled()
})

test("an account with no capacity is told so, and cannot launch", async ({
  page,
}) => {
  // The default handler answers with an empty instanceTypeSet.
  await page.goto("/ec2/describe-instances?create=true")

  await expect(page.getByText(NO_COMPUTE)).toBeVisible()
  await expect(
    page.getByRole("button", { name: "Create Instance" }),
  ).toBeDisabled()
})

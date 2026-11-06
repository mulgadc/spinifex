import type { Locator, Page } from "@playwright/test"

import {
  type ApiHandler,
  ec2,
  expect,
  test,
  type Theme,
  useTheme,
  xml,
} from "./console"

const VPC_ID = "vpc-0e2e000000000001"
const PUBLIC = "subnet-0e2e00000000000a"
const PRIVATE = "subnet-0e2e00000000000b"
const IGW_ID = "igw-0e2e000000000001"
const NAT_ID = "nat-0e2e000000000001"
const MAP = `/ec2/describe-vpcs?selected=${VPC_ID}&tab=topology`
const PAGE = `/ec2/describe-vpcs/${VPC_ID}?tab=topology`

// A lifted line keeps its layer: no line is raised over the cards.
async function linesStayBeneathCards(page: Page) {
  const raised = await page.evaluate(
    () =>
      [...document.querySelectorAll(".react-flow__edge")].filter(
        (edge) =>
          Number(getComputedStyle(edge.closest("svg") ?? edge).zIndex || 0) > 0,
      ).length,
  )

  expect(raised).toBe(0)
}

// The box's border lies outside its card on every side.
async function boxEncloses(box: Locator, card: Locator) {
  const [outer, inner] = await Promise.all([
    box.boundingBox(),
    card.boundingBox(),
  ])

  expect(outer && inner).toBeTruthy()

  if (!outer || !inner) {
    return
  }

  expect(inner.x).toBeGreaterThan(outer.x)
  expect(inner.y).toBeGreaterThan(outer.y)
  expect(inner.x + inner.width).toBeLessThan(outer.x + outer.width)
  expect(inner.y + inner.height).toBeLessThan(outer.y + outer.height)
}

// No line's label is drawn under a card, where the card would hide it.
async function labelsClearOfCards(page: Page) {
  const hidden = await page.evaluate(() => {
    const cards = [...document.querySelectorAll('[data-testid^="card-"]')].map(
      (card) => card.getBoundingClientRect(),
    )

    const labels = [
      ...document.querySelectorAll<HTMLElement>('[data-testid^="line-label-"]'),
    ]

    return labels.flatMap((label) => {
      const l = label.getBoundingClientRect()

      const under = cards.some(
        (c) =>
          l.left < c.right &&
          c.left < l.right &&
          l.top < c.bottom &&
          c.top < l.bottom,
      )

      // Labels stacked at one line's end must not cover each other either.
      const covers = labels.some((other) => {
        const o = other.getBoundingClientRect()

        return (
          other !== label &&
          l.left < o.right &&
          o.left < l.right &&
          l.top < o.bottom &&
          o.top < l.bottom
        )
      })

      return under || covers ? [label.dataset.testid] : []
    })
  })

  expect(hidden).toStrictEqual([])
}

function named(name: string): string {
  return `<tagSet><item><key>Name</key><value>${name}</value></item></tagSet>`
}

const VPCS = `<vpcSet><item><vpcId>${VPC_ID}</vpcId><state>available</state>
<cidrBlock>10.0.0.0/16</cidrBlock><isDefault>false</isDefault>${named("shop")}</item></vpcSet>`

const SUBNETS = `<subnetSet>
<item><subnetId>${PUBLIC}</subnetId><vpcId>${VPC_ID}</vpcId><cidrBlock>10.0.1.0/24</cidrBlock>
<availabilityZone>ap-southeast-2a</availabilityZone><state>available</state>${named("public-a")}</item>
<item><subnetId>${PRIVATE}</subnetId><vpcId>${VPC_ID}</vpcId><cidrBlock>10.0.2.0/24</cidrBlock>
<availabilityZone>ap-southeast-2b</availabilityZone><state>available</state>${named("private-b")}</item>
</subnetSet>`

const ROUTE_TABLES = `<routeTableSet>
<item><routeTableId>rtb-0e2e00000000main</routeTableId><vpcId>${VPC_ID}</vpcId>
<associationSet><item><main>true</main></item></associationSet>
<routeSet><item><destinationCidrBlock>10.0.0.0/16</destinationCidrBlock><gatewayId>local</gatewayId></item>
<item><destinationCidrBlock>0.0.0.0/0</destinationCidrBlock><gatewayId>${IGW_ID}</gatewayId></item></routeSet></item>
<item><routeTableId>rtb-0e2e0000000priv</routeTableId><vpcId>${VPC_ID}</vpcId>
<associationSet><item><subnetId>${PRIVATE}</subnetId><main>false</main></item></associationSet>
<routeSet><item><destinationCidrBlock>10.0.0.0/16</destinationCidrBlock><gatewayId>local</gatewayId></item>
<item><destinationCidrBlock>0.0.0.0/0</destinationCidrBlock><natGatewayId>${NAT_ID}</natGatewayId></item></routeSet></item>
</routeTableSet>`

const GATEWAYS = `<internetGatewaySet><item><internetGatewayId>${IGW_ID}</internetGatewayId>
<attachmentSet><item><vpcId>${VPC_ID}</vpcId><state>available</state></item></attachmentSet></item>
</internetGatewaySet>`

const NATS = `<natGatewaySet><item><natGatewayId>${NAT_ID}</natGatewayId><vpcId>${VPC_ID}</vpcId>
<subnetId>${PUBLIC}</subnetId><state>available</state><connectivityType>public</connectivityType>
<natGatewayAddressSet><item><allocationId>eipalloc-0e2e0000000nat</allocationId>
<publicIp>203.0.113.10</publicIp></item></natGatewayAddressSet></item></natGatewaySet>`

const ADDRESSES = `<addressesSet>
<item><allocationId>eipalloc-0e2e0000000nat</allocationId><publicIp>203.0.113.10</publicIp><domain>vpc</domain></item>
<item><allocationId>eipalloc-0e2e0000000web</allocationId><publicIp>203.0.113.11</publicIp>
<instanceId>i-0e2e00000000000a</instanceId><domain>vpc</domain></item>
</addressesSet>`

function instance(id: string, subnet: string, ip: string, name: string) {
  return `<item><reservationId>r-${id}</reservationId><instancesSet><item>
<instanceId>${id}</instanceId><vpcId>${VPC_ID}</vpcId><subnetId>${subnet}</subnetId>
<privateIpAddress>${ip}</privateIpAddress><instanceState><code>16</code><name>running</name></instanceState>
<instanceType>t3.micro</instanceType>${named(name)}</item></instancesSet></item>`
}

const INSTANCES = `<reservationSet>${instance("i-0e2e00000000000a", PUBLIC, "10.0.1.10", "web")}${instance("i-0e2e00000000000b", PRIVATE, "10.0.2.10", "worker")}</reservationSet>`

function query(
  namespace: string,
): (action: string, inner: string) => ApiHandler {
  return (action, inner) => () => ({
    body: xml(action, `<${action}Result>${inner}</${action}Result>`, namespace),
  })
}

const elb = query("http://elasticloadbalancing.amazonaws.com/doc/2015-12-01/")
const rds = query("http://rds.amazonaws.com/doc/2014-10-31/")

const LOAD_BALANCERS = `<LoadBalancers><member>
<LoadBalancerArn>arn:aws:elasticloadbalancing:ap-southeast-2:000000000001:loadbalancer/app/shop/0e2e</LoadBalancerArn>
<LoadBalancerName>shop</LoadBalancerName><VpcId>${VPC_ID}</VpcId><Type>application</Type>
<Scheme>internet-facing</Scheme><State><Code>active</Code></State>
<AvailabilityZones><member><ZoneName>ap-southeast-2a</ZoneName><SubnetId>${PUBLIC}</SubnetId></member></AvailabilityZones>
</member></LoadBalancers>`

const DATABASES = `<DBInstances><DBInstance><DBInstanceIdentifier>orders</DBInstanceIdentifier>
<Engine>postgres</Engine><DBInstanceStatus>available</DBInstanceStatus>
<DBSubnetGroup><DBSubnetGroupName>shop-db</DBSubnetGroupName><VpcId>${VPC_ID}</VpcId>
<SubnetGroupStatus>Complete</SubnetGroupStatus></DBSubnetGroup></DBInstance></DBInstances>`

test.beforeEach(({ api }) => {
  api.handle("DescribeVpcs", ec2("DescribeVpcs", VPCS))
  api.handle("DescribeSubnets", ec2("DescribeSubnets", SUBNETS))
  api.handle("DescribeRouteTables", ec2("DescribeRouteTables", ROUTE_TABLES))
  api.handle(
    "DescribeInternetGateways",
    ec2("DescribeInternetGateways", GATEWAYS),
  )
  api.handle("DescribeNatGateways", ec2("DescribeNatGateways", NATS))
  api.handle("DescribeAddresses", ec2("DescribeAddresses", ADDRESSES))
  api.handle("DescribeInstances", ec2("DescribeInstances", INSTANCES))
  api.handle(
    "DescribeLoadBalancers",
    elb("DescribeLoadBalancers", LOAD_BALANCERS),
  )
  api.handle("DescribeDBInstances", rds("DescribeDBInstances", DATABASES))
})

test.describe("VPC topology", () => {
  test("maps the VPC's gateways, subnets and what runs in them", async ({
    page,
  }) => {
    await page.goto(MAP)

    const panel = page.getByRole("region", { name: "VPC details" })
    await expect(panel.getByTestId(`card-vpc:${VPC_ID}`)).toHaveAttribute(
      "data-highlighted",
      "true",
    )

    await Promise.all(
      [
        `igw:${IGW_ID}`,
        `nat:${NAT_ID}`,
        `subnet:${PUBLIC}`,
        `subnet:${PRIVATE}`,
        "instance:i-0e2e00000000000a",
        "rds:orders",
      ].map(async (card) => {
        await expect(panel.getByTestId(`card-${card}`)).toBeVisible()
      }),
    )

    await expect(
      panel.getByTestId(`line-label-nat:${NAT_ID}>subnet:${PRIVATE}:0.0.0.0/0`),
    ).toHaveText("0.0.0.0/0")
    await expect(panel.getByTestId(`card-rds:orders`)).toContainText(
      "subnet group shop-db",
    )
  })

  test("shows reachability, subnet type and the NAT gateway's way out", async ({
    page,
  }) => {
    await page.goto(MAP)

    const panel = page.getByRole("region", { name: "VPC details" })
    await expect(panel.getByTestId("card-internet")).toHaveAttribute(
      "data-reach",
      "reachable",
    )
    await expect(panel.getByTestId(`card-subnet:${PUBLIC}`)).toContainText(
      "Typepublic",
    )
    await expect(panel.getByTestId(`card-subnet:${PRIVATE}`)).toContainText(
      "Typeprivate",
    )
    await expect(
      panel.getByTestId(`line-label-igw:${IGW_ID}>nat:${NAT_ID}:outbound only`),
    ).toHaveText("outbound only")
    await expect(
      panel.getByTestId("card-instance:i-0e2e00000000000a"),
    ).toContainText("Private IP10.0.1.10")
    await expect(panel.getByTestId(`box:instance:${PUBLIC}`)).toContainText(
      "Instances",
    )
    await expect(panel.getByTestId(`box:lb:${PUBLIC}`)).toContainText(
      "Load balancers",
    )
    await boxEncloses(
      panel.getByTestId(`box:instance:${PUBLIC}`),
      panel.getByTestId("card-instance:i-0e2e00000000000a"),
    )
    await boxEncloses(
      panel.getByTestId(`box:lb:${PUBLIC}`),
      panel.locator('[data-testid^="card-lb:"]'),
    )
  })

  test("says the internet is unreachable without a gateway, and unknown when it cannot tell", async ({
    api,
    page,
  }) => {
    api.handle(
      "DescribeInternetGateways",
      ec2("DescribeInternetGateways", "<internetGatewaySet/>"),
    )
    await page.goto(MAP)
    await expect(page.getByTestId("card-internet")).toHaveAttribute(
      "data-reach",
      "unreachable",
    )
    await expect(page.getByTestId("card-internet")).toContainText(
      "No internet access",
    )

    api.handle("DescribeRouteTables", () => ({
      status: 403,
      contentType: "text/xml",
      body: `<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>denied</Message></Error></Errors><RequestID>e2e</RequestID></Response>`,
    }))
    await page.reload()
    await expect(page.getByTestId("card-internet")).toHaveAttribute(
      "data-reach",
      "unknown",
    )
    await expect(page.getByTestId(`card-subnet:${PUBLIC}`)).toContainText(
      "Typeunknown",
    )
  })

  test("keeps a hovered card's lines beneath the other cards", async ({
    page,
  }) => {
    await page.goto(MAP)
    await page.getByTestId(`card-subnet:${PRIVATE}`).hover()
    await linesStayBeneathCards(page)
    await page.getByTestId(`card-igw:${IGW_ID}`).hover()
    await linesStayBeneathCards(page)
  })

  for (const [where, url] of [
    ["panel", MAP],
    ["page", PAGE],
  ] as const) {
    test(`fits the map, its controls and its legend in the ${where}`, async ({
      page,
    }) => {
      await page.setViewportSize({ width: 1440, height: 900 })
      await page.goto(url)
      await expect(page.getByTestId(`card-vpc:${VPC_ID}`)).toBeVisible()
      await expect(page.getByText(/^Hover a card/u)).toBeInViewport({
        ratio: 1,
      })
      await expect(page.locator(".react-flow__controls")).toBeInViewport({
        ratio: 1,
      })
    })
  }

  test("lays the full page out left to right or top down", async ({ page }) => {
    await page.goto(PAGE)

    const internet = page.getByTestId("card-internet")
    const igw = page.getByTestId(`card-igw:${IGW_ID}`)
    await expect(igw).toBeVisible()

    const down = [await internet.boundingBox(), await igw.boundingBox()]
    expect(down[0]?.y ?? 0).toBeLessThan(down[1]?.y ?? 0)

    await page.getByRole("button", { name: "Left to right" }).click()
    await expect(page.getByTestId(`box:instance:${PUBLIC}`)).toBeVisible()

    const across = [await internet.boundingBox(), await igw.boundingBox()]
    expect(across[0]?.x ?? 0).toBeLessThan(across[1]?.x ?? 0)
  })

  test("keeps every line's label clear of the cards, either way round", async ({
    page,
  }) => {
    await page.goto(PAGE)
    await expect(
      page.getByTestId(`line-label-igw:${IGW_ID}>subnet:${PUBLIC}:0.0.0.0/0`),
    ).toBeVisible()
    await labelsClearOfCards(page)

    await page.getByRole("button", { name: "Left to right" }).click()
    await expect(page.getByTestId(`box:instance:${PUBLIC}`)).toBeVisible()
    await labelsClearOfCards(page)
  })

  test("stacks several routes' labels into one subnet apart, either way round", async ({
    api,
    page,
  }) => {
    const extra = ["198.51.100.0/24", "203.0.113.0/24"]
      .map(
        (cidr) =>
          `<item><destinationCidrBlock>${cidr}</destinationCidrBlock><gatewayId>${IGW_ID}</gatewayId></item>`,
      )
      .join("")

    api.handle(
      "DescribeRouteTables",
      ec2(
        "DescribeRouteTables",
        ROUTE_TABLES.replace(
          `<gatewayId>${IGW_ID}</gatewayId></item></routeSet>`,
          `<gatewayId>${IGW_ID}</gatewayId></item>${extra}</routeSet>`,
        ),
      ),
    )
    await page.goto(PAGE)
    await expect(
      page.getByTestId(
        `line-label-igw:${IGW_ID}>subnet:${PUBLIC}:203.0.113.0/24`,
      ),
    ).toBeVisible()
    await labelsClearOfCards(page)

    await page.getByRole("button", { name: "Left to right" }).click()
    await expect(page.getByTestId(`box:instance:${PUBLIC}`)).toBeVisible()
    await labelsClearOfCards(page)
  })

  test("a card opens the resource's own panel", async ({ page }) => {
    await page.goto(MAP)
    await page.getByTestId(`card-subnet:${PRIVATE}`).click()
    await expect(page).toHaveURL(
      new RegExp(`/ec2/describe-subnets\\?selected=${PRIVATE}`, "u"),
    )
    await expect(
      page
        .getByRole("region", { name: "Subnet details" })
        .getByText("10.0.2.0/24")
        .first(),
    ).toBeVisible()
  })

  test("has no control that edits the network", async ({ page }) => {
    await page.goto(MAP)

    const canvas = page.locator(".react-flow")
    await expect(canvas.getByTestId(`card-vpc:${VPC_ID}`)).toBeVisible()
    await expect(canvas.locator(".react-flow__handle.connectable")).toHaveCount(
      0,
    )
    // Only the zoom controls: zoom in, zoom out and fit.
    await expect(canvas.getByRole("button")).toHaveCount(3)
  })

  test("says discovery is incomplete when a marker remains", async ({
    api,
    page,
  }) => {
    api.handle(
      "DescribeInstances",
      ec2("DescribeInstances", `${INSTANCES}<nextToken>more</nextToken>`),
    )

    await page.goto(MAP)
    await expect(
      page.getByText(/Discovery incomplete for instances/u),
    ).toBeVisible()
  })

  for (const theme of ["dark", "light"] satisfies Theme[]) {
    test(`matches the ${theme} baseline`, async ({ page }) => {
      await useTheme(page, theme)
      await page.setViewportSize({ width: 1440, height: 900 })
      await page.goto(MAP)
      await expect(page.getByTestId(`card-vpc:${VPC_ID}`)).toBeVisible()
      await page.evaluate(async () => await document.fonts.ready)
      await expect(page).toHaveScreenshot(`vpc-topology-${theme}.png`)
    })

    test(`matches the ${theme} left-to-right baseline`, async ({ page }) => {
      await useTheme(page, theme)
      await page.setViewportSize({ width: 1440, height: 900 })
      await page.goto(PAGE)
      await page.getByRole("button", { name: "Left to right" }).click()
      await expect(page.getByTestId(`box:instance:${PUBLIC}`)).toBeVisible()
      await page.evaluate(async () => await document.fonts.ready)
      await expect(page).toHaveScreenshot(`vpc-topology-across-${theme}.png`)
    })
  }
})

const RESOURCE_MAPS = [
  {
    name: "subnet",
    url: `/ec2/describe-subnets?selected=${PUBLIC}&tab=topology`,
    region: "Subnet details",
    lit: [`subnet:${PUBLIC}`, "instance:i-0e2e00000000000a", `nat:${NAT_ID}`],
  },
  {
    name: "route table",
    url: "/ec2/describe-route-tables?selected=rtb-0e2e0000000priv&tab=topology",
    region: "Route table details",
    lit: [`subnet:${PRIVATE}`, `nat:${NAT_ID}`],
  },
  {
    name: "internet gateway",
    url: `/ec2/describe-internet-gateways?selected=${IGW_ID}&tab=topology`,
    region: "Internet gateway details",
    lit: [`igw:${IGW_ID}`, `subnet:${PUBLIC}`],
  },
  {
    name: "NAT gateway",
    url: `/ec2/describe-nat-gateways?selected=${NAT_ID}&tab=topology`,
    region: "NAT gateway details",
    lit: [
      `nat:${NAT_ID}`,
      `subnet:${PUBLIC}`,
      `subnet:${PRIVATE}`,
      "eip:eipalloc-0e2e0000000nat",
    ],
  },
  {
    name: "instance",
    url: "/ec2/describe-instances?selected=i-0e2e00000000000a&tab=topology",
    region: "Instance details",
    lit: [
      "instance:i-0e2e00000000000a",
      `subnet:${PUBLIC}`,
      "eip:eipalloc-0e2e0000000web",
    ],
  },
] as const

test.describe("A resource's topology tab", () => {
  for (const { name, url, region, lit } of RESOURCE_MAPS) {
    test(`draws the ${name}'s VPC with it highlighted and the rest dimmed`, async ({
      page,
    }) => {
      await page.goto(url)

      const panel = page.getByRole("region", { name: region })

      await Promise.all(
        lit.map(async (id) => {
          await expect(panel.getByTestId(`card-${id}`)).toHaveAttribute(
            "data-highlighted",
            "true",
          )
        }),
      )
      await expect(panel.getByTestId(`card-vpc:${VPC_ID}`)).toHaveAttribute(
        "data-dimmed",
        "true",
      )
    })
  }

  test("says a detached internet gateway has no map", async ({ api, page }) => {
    api.handle(
      "DescribeInternetGateways",
      ec2(
        "DescribeInternetGateways",
        `<internetGatewaySet><item><internetGatewayId>${IGW_ID}</internetGatewayId>
<attachmentSet/></item></internetGatewaySet>`,
      ),
    )

    await page.goto(
      `/ec2/describe-internet-gateways?selected=${IGW_ID}&tab=topology`,
    )
    await expect(
      page.getByText(
        "This internet gateway is attached to no VPC, so it has no map.",
      ),
    ).toBeVisible()
  })

  test("offers the direction toggle on the instance's own page", async ({
    page,
  }) => {
    await page.goto("/ec2/describe-instances/i-0e2e00000000000a?tab=topology")
    await expect(
      page.getByTestId("card-instance:i-0e2e00000000000a"),
    ).toHaveAttribute("data-highlighted", "true")
    await page.getByRole("button", { name: "Left to right" }).click()
    await expect(
      page.getByTestId("card-instance:i-0e2e00000000000a"),
    ).toBeVisible()
  })
})

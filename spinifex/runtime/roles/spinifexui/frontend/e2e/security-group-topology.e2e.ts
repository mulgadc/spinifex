import type { Page } from "@playwright/test"

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
const WEB = "sg-0e2e00000000000web"
const APP = "sg-0e2e00000000000app"
const PANEL = `/ec2/describe-security-groups?selected=${WEB}&tab=topology`
const PAGE = `/ec2/describe-security-groups/${WEB}?tab=topology`

function cidrs(...blocks: string[]) {
  return `<ipRanges>${blocks.map((b) => `<item><cidrIp>${b}</cidrIp></item>`).join("")}</ipRanges>`
}

function rule(protocol: string, from: number, to: number, sources: string) {
  return `<item><ipProtocol>${protocol}</ipProtocol><fromPort>${from}</fromPort><toPort>${to}</toPort>${sources}</item>`
}

const GROUPS = `<securityGroupInfo>
<item><ownerId>000000000000</ownerId><groupId>${WEB}</groupId><groupName>web</groupName>
<groupDescription>front end</groupDescription><vpcId>${VPC_ID}</vpcId>
<ipPermissions>
${rule("tcp", 443, 443, cidrs("0.0.0.0/0"))}
${rule("tcp", 80, 80, cidrs("0.0.0.0/0"))}
${rule("icmp", 8, -1, cidrs("10.0.0.0/16"))}
${rule("tcp", 8080, 8080, `<groups><item><userId>000000000000</userId><groupId>${APP}</groupId></item></groups>`)}
<item><ipProtocol>-1</ipProtocol><groups><item><userId>000000000000</userId><groupId>${WEB}</groupId></item></groups></item>
</ipPermissions>
<ipPermissionsEgress><item><ipProtocol>-1</ipProtocol>${cidrs("0.0.0.0/0")}</item></ipPermissionsEgress>
</item>
<item><ownerId>000000000000</ownerId><groupId>${APP}</groupId><groupName>app</groupName>
<groupDescription>app tier</groupDescription><vpcId>${VPC_ID}</vpcId>
<ipPermissions/><ipPermissionsEgress/></item>
</securityGroupInfo>`

const INSTANCES = `<reservationSet><item><reservationId>r-1</reservationId><instancesSet><item>
<instanceId>i-0e2e00000000000a</instanceId><vpcId>${VPC_ID}</vpcId>
<privateIpAddress>10.0.1.10</privateIpAddress><instanceState><code>16</code><name>running</name></instanceState>
<groupSet><item><groupId>${WEB}</groupId><groupName>web</groupName></item></groupSet>
<tagSet><item><key>Name</key><value>web-1</value></item></tagSet></item></instancesSet></item></reservationSet>`

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
<LoadBalancerArn>arn:aws:elasticloadbalancing:ap-southeast-2:000000000001:loadbalancer/app/front/0e2e</LoadBalancerArn>
<LoadBalancerName>front</LoadBalancerName><VpcId>${VPC_ID}</VpcId><Type>application</Type>
<Scheme>internet-facing</Scheme><State><Code>active</Code></State>
<SecurityGroups><member>${WEB}</member></SecurityGroups></member>
<member><LoadBalancerArn>arn:aws:elasticloadbalancing:ap-southeast-2:000000000001:loadbalancer/app/other/0e2e</LoadBalancerArn>
<LoadBalancerName>other</LoadBalancerName><VpcId>${VPC_ID}</VpcId><Type>application</Type>
<SecurityGroups><member>${APP}</member></SecurityGroups></member></LoadBalancers>`

const DATABASES = `<DBInstances><DBInstance><DBInstanceIdentifier>orders</DBInstanceIdentifier>
<Engine>postgres</Engine><DBInstanceStatus>available</DBInstanceStatus>
<VpcSecurityGroups><VpcSecurityGroupMembership><VpcSecurityGroupId>${WEB}</VpcSecurityGroupId>
<Status>active</Status></VpcSecurityGroupMembership></VpcSecurityGroups></DBInstance></DBInstances>`

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

      // Labels stacked on one line's end must not cover each other either.
      const covers = labels.some((other) => {
        if (other === label) {
          return false
        }

        const o = other.getBoundingClientRect()

        return (
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

test.beforeEach(({ api }) => {
  api.handle("DescribeSecurityGroups", ec2("DescribeSecurityGroups", GROUPS))
  api.handle("DescribeInstances", ec2("DescribeInstances", INSTANCES))
  api.handle(
    "DescribeLoadBalancers",
    elb("DescribeLoadBalancers", LOAD_BALANCERS),
  )
  api.handle("DescribeDBInstances", rds("DescribeDBInstances", DATABASES))
})

test.describe("Security group topology", () => {
  test("maps who the group admits, what it covers and where it lets traffic out", async ({
    page,
  }) => {
    await page.goto(PANEL)

    const panel = page.getByRole("region", { name: "Security group details" })

    await expect(panel.getByTestId(`card-group:${WEB}`)).toHaveAttribute(
      "data-highlighted",
      "true",
    )
    await expect(panel.getByTestId(`card-in:peer:${APP}`)).toContainText(
      "members in this VPC",
    )
    await expect(panel.getByTestId(`card-in:peer:${APP}`)).toContainText("app")
    await expect(panel.getByTestId("card-in:cidr:10.0.0.0/16")).toBeVisible()
    await expect(panel.getByTestId("card-out:cidr:0.0.0.0/0")).toBeVisible()
    await expect(
      panel.getByTestId("card-instance:i-0e2e00000000000a"),
    ).toContainText("web-1")
    await expect(panel.getByTestId("box:members")).toBeVisible()
    await expect(panel.getByTestId("card-rds:orders")).toBeVisible()
    await expect(panel.locator('[data-testid^="card-lb:"]')).toHaveCount(1)
    await expect(panel.locator('[data-testid^="card-lb:"]')).toContainText(
      "front",
    )

    await expect(
      panel.getByTestId(
        "line-label-in:cidr:10.0.0.0/16>inbound (icmp/echo-request):0",
      ),
    ).toBeVisible()
    await expect(
      panel.getByTestId("line-label-in:cidr:0.0.0.0/0>inbound (tcp/443):0"),
    ).toBeVisible()
    await expect(
      panel.getByTestId("line-label-in:cidr:0.0.0.0/0>inbound (tcp/80):1"),
    ).toBeVisible()
    await expect(
      panel.getByTestId(
        "line-label-out:cidr:0.0.0.0/0>outbound (all traffic):0",
      ),
    ).toBeVisible()
    await expect(
      panel.getByRole("list", { name: "Rule protocols" }),
    ).toContainText("ICMP")
  })

  test("names each row's direction and lists the rules among members in their box", async ({
    page,
  }) => {
    await page.goto(PAGE)
    await expect(page.getByTestId("caption:inbound")).toContainText("Inbound")
    await expect(page.getByTestId("caption:outbound")).toContainText("Outbound")
    await expect(page.getByTestId("among")).toContainText(
      "inbound (all traffic)",
    )
    await expect(page.locator('[data-testid^="card-in:self"]')).toHaveCount(0)
  })

  test("points each rule's line the way its traffic is allowed", async ({
    page,
  }) => {
    await page.goto(PAGE)
    await expect(page.getByTestId(`card-group:${WEB}`)).toBeVisible()

    const lines = page.locator(".react-flow__edge-path")

    await expect(lines).not.toHaveCount(0)
    await expect(lines.and(page.locator(":not([marker-mid])"))).toHaveCount(0)
  })

  test("keeps every rule's label clear of the cards and of each other, either way round", async ({
    page,
  }) => {
    await page.goto(PAGE)
    await expect(page.getByTestId(`card-group:${WEB}`)).toBeVisible()
    await labelsClearOfCards(page)

    await page.getByRole("button", { name: "Left to right" }).click()
    await expect(page.getByTestId("box:members")).toBeVisible()
    await labelsClearOfCards(page)
  })

  test("a group a rule names opens that group's panel", async ({ page }) => {
    await page.goto(PANEL)
    await page.getByTestId(`card-in:peer:${APP}`).click()
    await expect(page).toHaveURL(new RegExp(`selected=${APP}`))
  })

  test("has no control that edits the group", async ({ page }) => {
    await page.goto(PAGE)
    await expect(page.getByTestId(`card-group:${WEB}`)).toBeVisible()
    await expect(
      page
        .locator(".react-flow")
        .getByRole("button", { name: /add|delete|remove|edit/i }),
    ).toHaveCount(0)
  })

  for (const theme of ["dark", "light"] satisfies Theme[]) {
    test(`matches the ${theme} baseline`, async ({ page }) => {
      await useTheme(page, theme)
      await page.setViewportSize({ width: 1440, height: 900 })
      await page.goto(PAGE)
      await expect(page.getByTestId(`card-group:${WEB}`)).toBeVisible()
      await page.evaluate(async () => await document.fonts.ready)
      await expect(page).toHaveScreenshot(
        `security-group-topology-${theme}.png`,
      )
    })
  }
})

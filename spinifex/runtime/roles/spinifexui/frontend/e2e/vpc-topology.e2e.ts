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
  }
})

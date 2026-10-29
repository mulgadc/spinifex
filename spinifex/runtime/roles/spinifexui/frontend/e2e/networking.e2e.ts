import { ec2, expect, expectNoHorizontalOverflow, test } from "./console"

const VPC_ID = "vpc-0e2e000000000001"
const IGW_ID = "igw-0e2e000000000001"
const DETACHED_IGW_ID = "igw-0e2e000000000002"

const VPCS = `<vpcSet><item><vpcId>${VPC_ID}</vpcId><state>available</state>
<cidrBlock>172.31.0.0/16</cidrBlock><isDefault>true</isDefault>
<instanceTenancy>default</instanceTenancy><ownerId>000000000001</ownerId>
<dhcpOptionsId>dopt-default</dhcpOptionsId></item></vpcSet>`

const SUBNETS = `<subnetSet><item><subnetId>subnet-0e2e000000000001</subnetId>
<vpcId>${VPC_ID}</vpcId><cidrBlock>172.31.0.0/20</cidrBlock>
<availabilityZone>ap-southeast-2a</availabilityZone><state>available</state></item></subnetSet>`

const GATEWAYS = `<internetGatewaySet>
<item><internetGatewayId>${IGW_ID}</internetGatewayId><ownerId>000000000001</ownerId>
<attachmentSet><item><vpcId>${VPC_ID}</vpcId><state>available</state></item></attachmentSet></item>
<item><internetGatewayId>${DETACHED_IGW_ID}</internetGatewayId><ownerId>000000000001</ownerId>
<attachmentSet/><tagSet><item><key>Name</key><value>spare</value></item></tagSet></item>
</internetGatewaySet>`

test.beforeEach(({ api }) => {
  api.handle("DescribeVpcs", ec2("DescribeVpcs", VPCS))
  api.handle("DescribeSubnets", ec2("DescribeSubnets", SUBNETS))
  api.handle(
    "DescribeInternetGateways",
    ec2("DescribeInternetGateways", GATEWAYS),
  )
})

test.describe("VPCs", () => {
  test("opens a VPC in the side panel with its subnets", async ({ page }) => {
    await page.goto("/ec2/describe-vpcs")
    await page.getByRole("cell", { name: "172.31.0.0/16" }).click()
    await expect(page).toHaveURL(new RegExp(`selected=${VPC_ID}`, "u"))

    const panel = page.getByRole("region", { name: "VPC details" })
    await expect(panel.getByText("172.31.0.0/20")).toBeVisible()

    await panel.getByRole("tab", { name: "Settings" }).click()
    await expect(panel.getByText("No tags.")).toBeVisible()
    await expect(
      panel.getByRole("button", { name: "Delete VPC" }),
    ).toBeEnabled()
  })

  test("deletes from the panel and closes it", async ({ api, page }) => {
    const deleted: string[] = []
    api.handle("DeleteVpc", (params) => {
      deleted.push(params.get("VpcId") ?? "")

      return ec2("DeleteVpc", "<return>true</return>")(params)
    })

    await page.goto(`/ec2/describe-vpcs?selected=${VPC_ID}&tab=settings`)
    await page.getByRole("button", { name: "Delete VPC" }).click()
    await page
      .getByRole("alertdialog")
      .getByRole("button", { name: "Delete" })
      .click()

    await expect(page.getByRole("region", { name: "VPC details" })).toBeHidden()
    expect(deleted).toStrictEqual([VPC_ID])
  })

  test("creates from a panel beside the list", async ({ page }) => {
    await page.goto("/ec2/describe-vpcs")
    await page.getByRole("link", { name: "Create VPC" }).click()

    const panel = page.getByRole("region", { name: "Create VPC" })
    await expect(panel.getByLabel("IPv4 CIDR block")).toBeVisible()
    await expect(page.getByRole("table", { name: "VPCs" })).toBeVisible()
  })
})

test.describe("internet gateways", () => {
  test("refuses to delete an attached gateway", async ({ page }) => {
    await page.goto("/ec2/describe-internet-gateways")
    await page.getByRole("link", { name: IGW_ID }).click()

    const panel = page.getByRole("region", { name: "Internet gateway details" })
    await expect(panel.getByRole("link", { name: VPC_ID })).toBeVisible()

    await panel.getByRole("tab", { name: "Settings" }).click()
    await expect(
      panel.getByRole("button", { name: "Delete internet gateway" }),
    ).toBeDisabled()
  })

  test("names a gateway by its tag and allows deleting it detached", async ({
    page,
  }) => {
    await page.goto(
      `/ec2/describe-internet-gateways?selected=${DETACHED_IGW_ID}&tab=settings`,
    )
    const panel = page.getByRole("region", { name: "Internet gateway details" })

    await expect(panel.getByRole("heading", { name: "spare" })).toBeVisible()
    await expect(panel.getByText("detached").first()).toBeVisible()
    await expect(
      panel.getByRole("button", { name: "Delete internet gateway" }),
    ).toBeEnabled()
  })

  test("shows the panel as its own page on a phone", async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 })
    await page.goto(`/ec2/describe-internet-gateways?selected=${IGW_ID}`)

    await expect(
      page.getByRole("region", { name: "Internet gateway details" }),
    ).toBeVisible()
    await expect(
      page.getByRole("table", { name: "Internet gateways" }),
    ).toBeHidden()
    await expectNoHorizontalOverflow(page)
  })
})

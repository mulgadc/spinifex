import { type ApiHandler, ec2, expect, test, xml } from "./console"

const ELB_NAMESPACE =
  "http://elasticloadbalancing.amazonaws.com/doc/2015-12-01/"

const VPC_ID = "vpc-0e2e000000000001"

const TG_ARN =
  "arn:aws:elasticloadbalancing:ap-southeast-2:000000000001:targetgroup/web/0e2e"

const LB_ARN =
  "arn:aws:elasticloadbalancing:ap-southeast-2:000000000001:loadbalancer/app/web/0e2e"

function elb(action: string, inner: string): ApiHandler {
  return () => ({
    body: xml(
      action,
      `<${action}Result>${inner}</${action}Result>`,
      ELB_NAMESPACE,
    ),
  })
}

test.beforeEach(({ api }) => {
  api.handle(
    "DescribeVpcs",
    ec2(
      "DescribeVpcs",
      `<vpcSet><item><vpcId>${VPC_ID}</vpcId><state>available</state>
<cidrBlock>172.31.0.0/16</cidrBlock><isDefault>true</isDefault></item></vpcSet>`,
    ),
  )
  api.handle(
    "DescribeSubnets",
    ec2(
      "DescribeSubnets",
      `<subnetSet><item><subnetId>subnet-0e2e000000000001</subnetId>
<vpcId>${VPC_ID}</vpcId><cidrBlock>172.31.0.0/20</cidrBlock>
<availabilityZone>ap-southeast-2a</availabilityZone><state>available</state></item></subnetSet>`,
    ),
  )
  api.handle(
    "DescribeLoadBalancers",
    elb("DescribeLoadBalancers", "<LoadBalancers/>"),
  )
  api.handle(
    "DescribeTargetGroups",
    elb("DescribeTargetGroups", "<TargetGroups/>"),
  )
  api.handle(
    "DescribeSSLPolicies",
    elb("DescribeSSLPolicies", "<SslPolicies/>"),
  )
  api.handle("ListCertificates", () => ({
    body: JSON.stringify({ CertificateSummaryList: [] }),
    contentType: "application/x-amz-json-1.1",
  }))
})

// Unit tests run without the React compiler, which is what hid a form whose
// errors never rendered; these run the compiled components in a browser.
test.describe("create load balancer", () => {
  test("sends the target group, load balancer and listener", async ({
    api,
    page,
  }) => {
    const calls: string[] = []

    const record =
      (action: string, inner: string): ApiHandler =>
      (params) => {
        calls.push(action)

        return elb(action, inner)(params)
      }

    api.handle(
      "CreateTargetGroup",
      record(
        "CreateTargetGroup",
        `<TargetGroups><member><TargetGroupArn>${TG_ARN}</TargetGroupArn></member></TargetGroups>`,
      ),
    )
    api.handle(
      "CreateLoadBalancer",
      record(
        "CreateLoadBalancer",
        `<LoadBalancers><member><LoadBalancerArn>${LB_ARN}</LoadBalancerArn></member></LoadBalancers>`,
      ),
    )
    api.handle("CreateListener", record("CreateListener", "<Listeners/>"))

    await page.goto("/ec2/describe-load-balancers?create=true")
    const panel = page.getByRole("region", { name: "Create load balancer" })
    await panel.locator("#lb-name").fill("web")
    await panel.locator("#tg-name").fill("web")
    await panel.getByRole("switch").first().click()
    await panel.getByRole("button", { name: "Create Load Balancer" }).click()

    await expect
      .poll(() => calls)
      .toStrictEqual([
        "CreateTargetGroup",
        "CreateLoadBalancer",
        "CreateListener",
      ])
  })

  test("shows a missing target group name and says so beside the button", async ({
    api,
    page,
  }) => {
    await page.goto("/ec2/describe-load-balancers?create=true")
    const panel = page.getByRole("region", { name: "Create load balancer" })
    await panel.locator("#lb-name").fill("web")
    await panel.getByRole("switch").first().click()
    await panel.getByRole("button", { name: "Create Load Balancer" }).click()

    await expect(panel.getByText("Name is required")).toBeVisible()
    await expect(
      panel.getByText("Fix the highlighted fields above."),
    ).toBeVisible()
    expect(api.unhandled).toStrictEqual([])
  })

  test("shows both forms' errors on an empty submit", async ({ page }) => {
    await page.goto("/ec2/describe-load-balancers?create=true")
    const panel = page.getByRole("region", { name: "Create load balancer" })
    await panel.getByRole("button", { name: "Create Load Balancer" }).click()

    await expect(panel.getByText("Name is required")).toHaveCount(2)
    await expect(panel.getByText("At least 1 subnet is required")).toBeVisible()
  })
})

test("create target group shows a missing name", async ({ page }) => {
  await page.goto("/ec2/describe-target-groups?create=true")
  const panel = page.getByRole("region", { name: "Create target group" })
  await panel.getByRole("button", { name: "Create Target Group" }).click()

  await expect(panel.getByText("Name is required")).toBeVisible()
})

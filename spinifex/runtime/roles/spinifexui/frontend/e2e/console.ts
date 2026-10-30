import { test as base, expect, type Page } from "@playwright/test"

// Mirrors securityHeadersMiddleware in spinifexui.go; keep the two in step.
export const CONSOLE_CSP =
  "default-src 'self'; script-src 'self'; style-src 'self'; " +
  "img-src 'self'; font-src 'self' data:; connect-src 'self'; " +
  "object-src 'none'; base-uri 'self'; form-action 'self'; " +
  "frame-ancestors 'none'; upgrade-insecure-requests;"

export const ADMIN_ACCOUNT = "000000000001"

// Obviously fake: nothing verifies the signature, the API is intercepted.
const FAKE_SESSION = {
  accessKeyId: "E2EFAKEACCESSKEY0000",
  secretAccessKey: "e2e-fake-secret",
  sessionToken: "e2e-fake-token",
  expiration: "2099-01-01T00:00:00Z",
}

export type Theme = "dark" | "light"

// A handler answers one API action. Query-protocol services (EC2, STS, IAM)
// name it in the form body; JSON services name it in X-Amz-Target. S3 is
// "<METHOD> <path>", and its handlers get the URL's query string.
export type ApiHandler = (params: URLSearchParams) => {
  status?: number
  body: string
  contentType?: string
  headers?: Record<string, string>
}

export interface ConsoleApi {
  handle: (action: string, handler: ApiHandler) => void
  unhandled: string[]
}

function actionOf(
  body: string,
  target: string | undefined,
  fallback: string,
): string {
  if (target) {
    return target.split(".").at(-1) ?? target
  }

  return new URLSearchParams(body).get("Action") ?? fallback
}

export function xml(action: string, inner: string, namespace: string): string {
  return `<?xml version="1.0" encoding="UTF-8"?>
<${action}Response xmlns="${namespace}">${inner}<requestId>e2e</requestId></${action}Response>`
}

// A JSON response body: the spinifex admin actions and the JSON-protocol services.
export type JsonValue =
  | string
  | number
  | boolean
  | null
  | JsonValue[]
  | { [key: string]: JsonValue }

export function json(value: JsonValue): ReturnType<ApiHandler> {
  return { body: JSON.stringify(value), contentType: "application/json" }
}

export const EC2_NAMESPACE = "http://ec2.amazonaws.com/doc/2016-11-15/"

export function ec2(action: string, inner: string): ApiHandler {
  return () => ({ body: xml(action, inner, EC2_NAMESPACE) })
}

// What the dashboard, the shell and the launch form ask for on an empty account.
const EMPTY_EC2 = {
  DescribeAddresses: "<addressesSet/>",
  DescribeImages: "<imagesSet/>",
  DescribeInstanceTypes: "<instanceTypeSet/>",
  DescribeInstances: "<reservationSet/>",
  DescribeInternetGateways: "<internetGatewaySet/>",
  DescribeKeyPairs: "<keySet/>",
  DescribeLaunchTemplates: "<launchTemplates/>",
  DescribeNatGateways: "<natGatewaySet/>",
  DescribePlacementGroups: "<placementGroupSet/>",
  DescribeSecurityGroups: "<securityGroupInfo/>",
  DescribeSubnets: "<subnetSet/>",
  DescribeVolumes: "<volumeSet/>",
  DescribeVpcs: "<vpcSet/>",
}

export const VERSION = {
  version: "v1.23.0-e2e",
  commit: "e2e0000",
  os: "linux",
  arch: "amd64",
  license: "AGPL-3.0",
}

// One healthy node with nothing on it, as a fresh single-node install reports.
export const NODES = {
  cluster_mode: "single",
  nodes: [
    {
      node: "node1",
      status: "Ready",
      host: "10.0.0.1",
      region: "ap-southeast-2",
      az: "ap-southeast-2a",
      uptime: 86_400,
      services: ["daemon", "awsgw", "predastore", "viperblock", "nats"],
      vm_count: 0,
      total_vcpu: 32,
      total_mem_gb: 128,
      reserved_vcpu: 2,
      reserved_mem_gb: 4,
      alloc_vcpu: 0,
      alloc_mem_gb: 0,
      total_gpus: 0,
      alloc_gpus: 0,
      instance_types: [],
    },
  ],
}

export const STS_NAMESPACE = "https://sts.amazonaws.com/doc/2011-06-15/"

export function stsIdentity(account: string): ApiHandler {
  return () => ({
    body: `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
<GetCallerIdentityResult><Arn>arn:aws:iam::${account}:user/admin</Arn><UserId>AIDAE2E</UserId><Account>${account}</Account></GetCallerIdentityResult>
<ResponseMetadata><RequestId>e2e</RequestId></ResponseMetadata></GetCallerIdentityResponse>`,
  })
}

async function installConsole(page: Page): Promise<ConsoleApi> {
  const handlers = new Map<string, ApiHandler>()

  const api: ConsoleApi = {
    handle: (action, handler) => {
      handlers.set(action, handler)
    },
    unhandled: [],
  }

  api.handle("GetCallerIdentity", stsIdentity(ADMIN_ACCOUNT))
  api.handle("GetVersion", () => json(VERSION))
  api.handle("GetNodes", () => json(NODES))
  api.handle("GetVMs", () => json({ vms: [] }))

  api.handle("GET /proxy/s3/", () => ({
    body: `<ListAllMyBucketsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Owner><ID>e2e</ID></Owner><Buckets/></ListAllMyBucketsResult>`,
  }))

  for (const [action, inner] of Object.entries(EMPTY_EC2)) {
    api.handle(action, ec2(action, inner))
  }

  await page.addInitScript((session) => {
    localStorage.setItem("spinifex:v2:aws-session", JSON.stringify(session))
  }, FAKE_SESSION)

  await page.route("**/api/config", async (route) => {
    await route.fulfill({
      json: { region: "ap-southeast-2", ochreEnabled: false },
    })
  })

  await page.route("**/proxy/**", async (route) => {
    const request = route.request()
    const body = request.postData() ?? ""

    const action = actionOf(
      body,
      request.headers()["x-amz-target"],
      `${request.method()} ${new URL(request.url()).pathname}`,
    )

    const handler = handlers.get(action)

    // Answer in the protocol's own error shape: a plain-text body makes the
    // SDK's DOMParser build a <parsererror>, whose inline style trips the CSP.
    if (!handler) {
      api.unhandled.push(action)
      const message = `no e2e handler for ${action}`
      await route.fulfill(
        request.headers()["x-amz-target"]
          ? {
              status: 501,
              contentType: "application/x-amz-json-1.1",
              body: JSON.stringify({ __type: "NotImplemented", message }),
            }
          : {
              status: 501,
              contentType: "text/xml",
              body: `<ErrorResponse><Error><Code>NotImplemented</Code><Message>${message}</Message></Error><RequestId>e2e</RequestId></ErrorResponse>`,
            },
      )

      return
    }

    const response = handler(
      action.includes(" ")
        ? new URL(request.url()).searchParams
        : new URLSearchParams(body),
    )

    await route.fulfill({
      status: response.status ?? 200,
      contentType: response.contentType ?? "text/xml",
      headers: response.headers,
      body: response.body,
    })
  })

  return api
}

export async function useTheme(page: Page, theme: Theme): Promise<void> {
  await page.addInitScript((value) => {
    localStorage.setItem("spinifex-ui-theme", value)
  }, theme)
}

// The page must never scroll sideways: wide content scrolls in its own box.
export async function expectNoHorizontalOverflow(page: Page): Promise<void> {
  const widths = await page.evaluate(() => ({
    scroll: document.documentElement.scrollWidth,
    client: document.documentElement.clientWidth,
  }))

  expect(widths.scroll, "page scrolls horizontally").toBeLessThanOrEqual(
    widths.client,
  )
}

export const test = base.extend<{ api: ConsoleApi }>({
  // Auto, so a test that never names the api still gets the fake console.
  api: [
    async ({ page }, use) => {
      await use(await installConsole(page))
    },
    { auto: true },
  ],
})

export { expect } from "@playwright/test"

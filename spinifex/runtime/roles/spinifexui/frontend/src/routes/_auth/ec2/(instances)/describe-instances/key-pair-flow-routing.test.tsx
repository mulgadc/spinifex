// Drives the real routeTree, so Back goes through the router's own history.
import { QueryClientProvider } from "@tanstack/react-query"
import {
  createMemoryHistory,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router"
import { render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

const sdk = await vi.hoisted(async () => {
  const { createFakeSdk } = await import("@/test/fake-sdk")

  return createFakeSdk([
    ["DescribeInstancesCommand", () => ({ Reservations: [] })],
    [
      "DescribeImagesCommand",
      () => ({
        Images: [
          {
            ImageId: "ami-u24",
            Name: "ami-ubuntu-24.04-x86_64",
            Architecture: "x86_64",
            RootDeviceName: "/dev/sda1",
          },
        ],
      }),
    ],
    [
      "DescribeKeyPairsCommand",
      () => ({ KeyPairs: [{ KeyPairId: "kp-1", KeyName: "default" }] }),
    ],
    [
      "DescribeInstanceTypesCommand",
      () => ({
        InstanceTypes: [
          {
            InstanceType: "t3.nano",
            ProcessorInfo: { SupportedArchitectures: ["x86_64"] },
          },
        ],
      }),
    ],
    ["DescribeSubnetsCommand", () => ({ Subnets: [] })],
    ["DescribeRouteTablesCommand", () => ({ RouteTables: [] })],
    ["DescribePlacementGroupsCommand", () => ({ PlacementGroups: [] })],
    [
      "DescribeVpcsCommand",
      () => ({ Vpcs: [{ VpcId: "vpc-1", IsDefault: true }] }),
    ],
    ["DescribeSecurityGroupsCommand", () => ({ SecurityGroups: [] })],
    ["DescribeLaunchTemplatesCommand", () => ({ LaunchTemplates: [] })],
  ])
})

vi.mock("@/lib/aws-client", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(sdk.send)
})

// Console-only calls such as instance capacity are refused, as for a session
// without the grant, so nothing leaves the test.
vi.mock("@/lib/signed-fetch", () => ({
  signedFetch: vi.fn().mockRejectedValue(new Error("not authorized")),
  signedAdminFetch: vi.fn().mockRejectedValue(new Error("not authorized")),
}))

vi.mock("@/lib/sts", () => ({
  getStsClient: () => ({
    send: vi.fn().mockResolvedValue({
      Account: "000000000001",
      Arn: "arn:aws:iam::000000000001:user/test",
    }),
  }),
}))

vi.mock("@/lib/auth", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/auth")>()

  return {
    ...actual,
    getCredentials: () => ({
      accessKeyId: "test",
      secretAccessKey: "test",
      sessionToken: "test",
      expiration: "2099-01-01T00:00:00Z",
    }),
  }
})

import { ThemeProvider } from "@/components/theme-provider"
import { SidebarProvider } from "@/components/ui/sidebar"
import { createTestQueryClient } from "@/test/render"

import { routeTree } from "../../../../../routeTree.gen"

function renderAppAt(path: string) {
  const queryClient = createTestQueryClient()

  const router = createRouter({
    routeTree,
    context: { queryClient },
    history: createMemoryHistory({ initialEntries: [path] }),
  })

  render(
    <ThemeProvider defaultTheme="dark" storageKey="spinifex-ui-theme">
      <QueryClientProvider client={queryClient}>
        <SidebarProvider>
          <RouterProvider router={router} />
        </SidebarProvider>
      </QueryClientProvider>
    </ThemeProvider>,
  )

  return { router }
}

describe("launch form key pair flow routing", () => {
  it("returns to the launch form with its entries on Back", async () => {
    const user = userEvent.setup()
    const { router } = renderAppAt("/ec2/describe-instances?create=true")

    await user.type(await screen.findByLabelText("Name (optional)"), "web-1")
    await user.click(screen.getByRole("button", { name: "Create key pair" }))

    await expect(
      screen.findByRole("region", { name: "Create key pair" }),
    ).resolves.toBeInTheDocument()
    expect(router.state.location.search).toMatchObject({
      create: true,
      keyPair: "create",
    })

    router.history.back()

    await waitFor(() => {
      expect(
        screen.queryByRole("region", { name: "Create key pair" }),
      ).not.toBeInTheDocument()
    })
    expect({ ...router.state.location.search }).toStrictEqual({ create: true })
    expect(screen.getByLabelText("Name (optional)")).toHaveValue("web-1")
  })
})

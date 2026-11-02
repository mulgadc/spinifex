import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

const mockSend = vi.hoisted(() => vi.fn().mockResolvedValue({}))

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import {
  ec2ImagesQueryOptions,
  ec2SubnetsQueryOptions,
  ec2VpcsQueryOptions,
} from "@/queries/ec2"
import { iamRolesQueryOptions } from "@/queries/iam"

import { CreateClusterPage } from "./create-cluster-page"

const EKS_IMAGE = {
  ImageId: "ami-eks",
  Tags: [{ Key: "spinifex:managed-by", Value: "eks" }],
}

const onClose = vi.fn()

function renderPage({ withEksImage = true }: { withEksImage?: boolean } = {}) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })

  queryClient.setQueryData(ec2VpcsQueryOptions.queryKey, {
    $metadata: {},
    Vpcs: [{ VpcId: "vpc-1", CidrBlock: "10.0.0.0/16" }],
  })
  queryClient.setQueryData(ec2SubnetsQueryOptions.queryKey, {
    $metadata: {},
    Subnets: [
      {
        SubnetId: "subnet-a",
        VpcId: "vpc-1",
        CidrBlock: "10.0.1.0/24",
        AvailabilityZone: "ap-southeast-2a",
      },
    ],
  })
  queryClient.setQueryData(iamRolesQueryOptions.queryKey, {
    $metadata: {},
    Roles: [],
  })
  queryClient.setQueryData(ec2ImagesQueryOptions.queryKey, {
    $metadata: {},
    Images: withEksImage ? [EKS_IMAGE] : [],
  })

  return render(
    <QueryClientProvider client={queryClient}>
      <CreateClusterPage onClose={onClose} />
    </QueryClientProvider>,
  )
}

async function cliText(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole("button", { name: "AWS CLI" }))

  return screen.getByText(/AWS_PROFILE=spinifex/).closest("code")
}

describe("CreateClusterPage", () => {
  it("renders the cluster name field and submit button", () => {
    renderPage()
    expect(screen.getByLabelText("Name")).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: "Create cluster" }),
    ).toBeInTheDocument()
  })

  it("blocks submit and shows validation when required fields are empty", async () => {
    const user = userEvent.setup()
    renderPage()

    await user.click(screen.getByRole("button", { name: "Create cluster" }))

    await expect(
      screen.findByText("Name is required"),
    ).resolves.toBeInTheDocument()
    expect(mockSend).not.toHaveBeenCalled()
  })

  it("defaults endpoint access to public with an open CIDR", () => {
    renderPage()
    expect(
      screen.getByRole("switch", { name: "Enable public access" }),
    ).toBeChecked()
    expect(
      screen.getByRole("switch", { name: "Enable private access" }),
    ).not.toBeChecked()
    expect(
      screen.getByRole("switch", {
        name: "Grant the cluster creator admin permissions",
      }),
    ).toBeChecked()
    expect(screen.getByLabelText("Public access CIDR 1")).toHaveValue(
      "0.0.0.0/0",
    )
  })

  it("hides the public CIDR editor when public access is disabled", async () => {
    const user = userEvent.setup()
    renderPage()

    await user.click(
      screen.getByRole("switch", { name: "Enable public access" }),
    )

    expect(
      screen.queryByLabelText("Public access CIDR 1"),
    ).not.toBeInTheDocument()
  })

  it("lists the chosen VPC's subnets as switches with their AZ and CIDR", async () => {
    const user = userEvent.setup()
    renderPage()

    await user.click(screen.getByLabelText("VPC"))
    await user.click(await screen.findByRole("option", { name: /vpc-1/ }))

    expect(
      screen.getByRole("switch", { name: "Subnet subnet-a" }),
    ).not.toBeChecked()
    expect(
      screen.getByText("ap-southeast-2a · 10.0.1.0/24"),
    ).toBeInTheDocument()
  })

  it("creates the cluster role inline and folds the section away", async () => {
    const user = userEvent.setup()
    mockSend.mockResolvedValue({
      Role: { Arn: "arn:aws:iam::123456789012:role/web-eks-cluster-role" },
    })
    renderPage()

    await user.type(screen.getByLabelText("Name"), "web")
    await user.click(screen.getByRole("button", { name: "Create role" }))

    const section = screen.getByRole("region", {
      name: "Create cluster IAM role",
    })

    expect(within(section).getByLabelText("Role name")).toHaveValue(
      "web-eks-cluster-role",
    )
    await user.click(
      within(section).getByRole("button", { name: "Create role" }),
    )

    await waitFor(() => {
      expect(
        screen.queryByRole("region", { name: "Create cluster IAM role" }),
      ).not.toBeInTheDocument()
    })
    expect(onClose).not.toHaveBeenCalled()
  })

  it("blocks the form when the EKS system image is missing", () => {
    renderPage({ withEksImage: false })
    expect(screen.getByText("EKS system image not found")).toBeInTheDocument()
    expect(screen.queryByLabelText("Name")).not.toBeInTheDocument()
    expect(
      screen.queryByRole("button", { name: "Create cluster" }),
    ).not.toBeInTheDocument()
  })

  it("shows the create-cluster command with the typed name and chosen subnet", async () => {
    const user = userEvent.setup()
    renderPage()

    await user.type(screen.getByLabelText("Name"), "web")
    await user.click(screen.getByLabelText("VPC"))
    await user.click(await screen.findByRole("option", { name: /vpc-1/ }))
    await user.click(screen.getByRole("switch", { name: "Subnet subnet-a" }))

    const code = await cliText(user)

    expect(code).toHaveTextContent("aws eks create-cluster")
    expect(code).toHaveTextContent("--name web")
    expect(code).toHaveTextContent(
      "--resources-vpc-config 'subnetIds=subnet-a,endpointPublicAccess=true,endpointPrivateAccess=false,publicAccessCidrs=0.0.0.0/0'",
    )
    expect(code).toHaveTextContent(
      "bootstrapClusterCreatorAdminPermissions=true",
    )
  })
})

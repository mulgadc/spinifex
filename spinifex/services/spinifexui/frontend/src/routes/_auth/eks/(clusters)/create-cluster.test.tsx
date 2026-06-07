import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getEksClient: () => ({ send: mockSend }),
}))

vi.mock("@tanstack/react-router", () => ({
  useNavigate: () => vi.fn(),
  Link: ({ children, to }: { children: ReactNode; to?: string }) => (
    <a href={to}>{children}</a>
  ),
}))

import {
  ec2ImagesQueryOptions,
  ec2SecurityGroupsQueryOptions,
  ec2SubnetsQueryOptions,
  ec2VpcsQueryOptions,
} from "@/queries/ec2"
import { iamRolesQueryOptions } from "@/queries/iam"

import { CreateClusterPage } from "./-components/create-cluster-page"

const EKS_IMAGE = {
  ImageId: "ami-eks",
  Tags: [{ Key: "spinifex:managed-by", Value: "eks" }],
}

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
    Subnets: [],
  })
  queryClient.setQueryData(ec2SecurityGroupsQueryOptions.queryKey, {
    $metadata: {},
    SecurityGroups: [],
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
      <CreateClusterPage />
    </QueryClientProvider>,
  )
}

describe("CreateClusterPage", () => {
  it("renders the cluster name field and submit button", () => {
    renderPage()
    expect(screen.getByLabelText("Name")).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: "Create Cluster" }),
    ).toBeInTheDocument()
  })

  it("blocks submit and shows validation when required fields are empty", async () => {
    const user = userEvent.setup()
    renderPage()

    await user.click(screen.getByRole("button", { name: "Create Cluster" }))

    await expect(
      screen.findByText("Name is required"),
    ).resolves.toBeInTheDocument()
    expect(mockSend).not.toHaveBeenCalled()
  })

  it("blocks the form when the EKS system image is missing", () => {
    renderPage({ withEksImage: false })
    expect(screen.getByText("EKS system image not found")).toBeInTheDocument()
    expect(screen.queryByLabelText("Name")).not.toBeInTheDocument()
    expect(
      screen.queryByRole("button", { name: "Create Cluster" }),
    ).not.toBeInTheDocument()
  })
})

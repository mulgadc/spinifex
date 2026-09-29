import { fireEvent, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

const { routerState } = vi.hoisted(() => ({
  routerState: { navigate: vi.fn() },
}))

vi.mock("@/lib/awsClient", () => ({
  getEcrClient: () => ({ send: vi.fn() }),
}))

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-router")>()
  return {
    ...actual,
    useNavigate: () => routerState.navigate,
    Link: ({ children, to }: { children: React.ReactNode; to?: string }) => (
      <a href={to}>{children}</a>
    ),
  }
})

import { RepositoriesPage } from "./repositories-page"

function seed(repositories: unknown[]) {
  const qc = createTestQueryClient()
  qc.setQueryData(["ecr", "repositories"], { $metadata: {}, repositories })
  return qc
}

const REPO = {
  repositoryName: "team/app",
  repositoryUri: "111.dkr.ecr.ap-southeast-2.local/team/app",
  imageTagMutability: "MUTABLE",
  createdAt: new Date("2026-01-01T00:00:00Z"),
}

describe("RepositoriesPage", () => {
  it("renders repository rows", () => {
    renderWithClient(<RepositoriesPage />, seed([REPO]))
    expect(screen.getByRole("link", { name: "team/app" })).toHaveAttribute(
      "href",
      "/ecr/list-repositories/$id",
    )
    expect(
      screen.getByText("111.dkr.ecr.ap-southeast-2.local/team/app"),
    ).toBeInTheDocument()
    expect(screen.getByText("MUTABLE")).toBeInTheDocument()
  })

  it("shows empty state with no repositories", () => {
    renderWithClient(<RepositoriesPage />, seed([]))
    expect(screen.getByText("No repositories found.")).toBeInTheDocument()
  })

  it("opens the create dialog", () => {
    renderWithClient(<RepositoriesPage />, seed([]))
    fireEvent.click(screen.getByRole("button", { name: "Create Repository" }))
    expect(
      screen.getByText(/Repository names may include namespaces/),
    ).toBeInTheDocument()
  })

  it("opens the delete confirmation for a repo", () => {
    renderWithClient(<RepositoriesPage />, seed([REPO]))
    fireEvent.click(screen.getByRole("button", { name: "Delete" }))
    expect(
      screen.getByText(/permanently deletes team\/app/),
    ).toBeInTheDocument()
  })
})

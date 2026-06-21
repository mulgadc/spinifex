import { fireEvent, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

vi.mock("@/lib/awsClient", () => ({
  getEcrClient: () => ({ send: vi.fn() }),
}))

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-router")>()
  return {
    ...actual,
    Link: ({ children, to }: { children: React.ReactNode; to?: string }) => (
      <a href={to}>{children}</a>
    ),
  }
})

import { RepositoryDetailPage } from "./repository-detail-page"

const REPO = "team/app"
const URI = "111.dkr.ecr.ap-southeast-2.local/team/app"

function seed(imageDetails: unknown[]) {
  const qc = createTestQueryClient()
  qc.setQueryData(["ecr", "repositories"], {
    repositories: [
      {
        repositoryName: REPO,
        repositoryUri: URI,
        imageTagMutability: "MUTABLE",
      },
    ],
  })
  qc.setQueryData(["ecr", "repositories", REPO, "images"], { imageDetails })
  qc.setQueryData(["ecr", "repositories", REPO, "policy"], null)
  return qc
}

function openImagesTab() {
  fireEvent.click(screen.getByRole("tab", { name: "Images" }))
}

const IMAGE = {
  imageDigest: "sha256:abcdef0123456789abcdef",
  imageTags: ["v1", "latest"],
  imageSizeInBytes: 4096,
  imagePushedAt: new Date("2026-01-01T00:00:00Z"),
}

describe("RepositoryDetailPage", () => {
  it("renders the push commands on the Summary tab and image rows on Images", () => {
    renderWithClient(
      <RepositoryDetailPage repositoryName={REPO} />,
      seed([IMAGE]),
    )
    expect(screen.getByText(/docker push/)).toBeInTheDocument()
    openImagesTab()
    expect(screen.getByText("v1, latest")).toBeInTheDocument()
  })

  it("labels an untagged image", () => {
    renderWithClient(
      <RepositoryDetailPage repositoryName={REPO} />,
      seed([{ imageDigest: "sha256:deadbeef00112233445566", imageTags: [] }]),
    )
    openImagesTab()
    expect(screen.getByText("<untagged>")).toBeInTheDocument()
  })

  it("shows the empty state when no images are pushed", () => {
    renderWithClient(<RepositoryDetailPage repositoryName={REPO} />, seed([]))
    openImagesTab()
    expect(screen.getByText("No images pushed yet.")).toBeInTheDocument()
  })

  it("shows scanning is unsupported on the Scan tab", () => {
    renderWithClient(<RepositoryDetailPage repositoryName={REPO} />, seed([]))
    fireEvent.click(screen.getByRole("tab", { name: "Scan" }))
    expect(
      screen.getByText(/Vulnerability scanning is not supported/),
    ).toBeInTheDocument()
  })

  it("opens the delete-image confirmation", () => {
    renderWithClient(
      <RepositoryDetailPage repositoryName={REPO} />,
      seed([IMAGE]),
    )
    openImagesTab()
    fireEvent.click(screen.getAllByRole("button", { name: "Delete" })[0])
    expect(
      screen.getByText(/permanently deletes the image manifest/),
    ).toBeInTheDocument()
  })
})

import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen } from "@testing-library/react"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, to }: { children: ReactNode; to?: string }) => (
    <a href={to}>{children}</a>
  ),
}))

import { eksClustersQueryOptions } from "@/queries/eks"

import { ClustersListPage } from "./clusters-list-page"

function renderSeeded(clusters: string[]) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  queryClient.setQueryData(eksClustersQueryOptions.queryKey, {
    $metadata: {},
    clusters,
  })
  return render(
    <QueryClientProvider client={queryClient}>
      <ClustersListPage />
    </QueryClientProvider>,
  )
}

describe("ClustersListPage", () => {
  it("renders cluster names sorted", () => {
    renderSeeded(["zebra", "alpha"])
    const links = screen.getAllByRole("link", { name: /zebra|alpha/ })
    expect(links.map((l) => l.textContent)).toStrictEqual(["alpha", "zebra"])
  })

  it("shows empty state when no clusters", () => {
    renderSeeded([])
    expect(screen.getByText("No EKS clusters found.")).toBeInTheDocument()
  })

  it("links each cluster to its detail route", () => {
    renderSeeded(["c1"])
    expect(screen.getByRole("link", { name: "c1" })).toHaveAttribute(
      "href",
      "/eks/list-clusters/$clusterName",
    )
  })
})

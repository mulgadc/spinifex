import type { GuardrailSummary } from "@aws-sdk/client-bedrock"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen } from "@testing-library/react"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

vi.mock("@tanstack/react-router", () => ({
  Link: ({
    children,
    to,
    params,
  }: {
    children: ReactNode
    to?: string
    params?: Record<string, string>
  }) => (
    <a href={`${to}${params?.guardrailId ? `/${params.guardrailId}` : ""}`}>
      {children}
    </a>
  ),
}))

import { guardrailsQueryOptions } from "@/queries/bedrock"

import { GuardrailsPage } from "./guardrails-page"

function renderSeeded(guardrails: GuardrailSummary[]) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  queryClient.setQueryData(guardrailsQueryOptions.queryKey, {
    $metadata: {},
    guardrails,
  })
  return render(
    <QueryClientProvider client={queryClient}>
      <GuardrailsPage />
    </QueryClientProvider>,
  )
}

const GUARDRAIL = {
  id: "gr-1",
  arn: "arn:aws:bedrock:us-east-1:000000000000:guardrail/gr-1",
  name: "content-safety",
  status: "READY",
  version: "1",
  createdAt: new Date("2026-01-01T00:00:00Z"),
  updatedAt: new Date("2026-01-01T00:00:00Z"),
} satisfies GuardrailSummary

describe("GuardrailsPage", () => {
  it("renders guardrail cards sorted by name", () => {
    renderSeeded([
      { ...GUARDRAIL, id: "gr-2", name: "zebra" },
      { ...GUARDRAIL, id: "gr-1", name: "alpha" },
    ])
    const links = screen.getAllByRole("link")
    expect(links.map((l) => l.getAttribute("href"))).toStrictEqual([
      "/bedrock/list-guardrails/$guardrailId/gr-1",
      "/bedrock/list-guardrails/$guardrailId/gr-2",
    ])
  })

  it("shows empty state when no guardrails", () => {
    renderSeeded([])
    expect(screen.getByText("No guardrails found.")).toBeInTheDocument()
  })

  it("links each guardrail to its detail route", () => {
    renderSeeded([GUARDRAIL])
    expect(
      screen.getByRole("link", { name: /content-safety/ }),
    ).toHaveAttribute("href", "/bedrock/list-guardrails/$guardrailId/gr-1")
  })

  it("shows the guardrail id and status", () => {
    renderSeeded([GUARDRAIL])
    expect(screen.getByText("gr-1")).toBeInTheDocument()
    expect(screen.getByText("READY")).toBeInTheDocument()
  })
})

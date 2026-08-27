import type {
  GetGuardrailCommandOutput,
  GuardrailSummary,
} from "@aws-sdk/client-bedrock"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen } from "@testing-library/react"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

const mockSend = vi.fn().mockResolvedValue({
  action: "NONE",
  assessments: [],
  outputs: [],
  usage: {},
})

vi.mock("@/lib/awsClient", () => ({
  getBedrockRuntimeClient: () => ({ send: mockSend }),
}))

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, to }: { children: ReactNode; to?: string }) => (
    <a href={to}>{children}</a>
  ),
}))

import {
  guardrailQueryOptions,
  guardrailVersionsQueryOptions,
} from "@/queries/bedrock"

import { GuardrailDetailPage } from "./guardrail-detail-page"

const GUARDRAIL_ID = "gr-1"

const GUARDRAIL = {
  $metadata: {},
  name: "content-safety",
  description: "Blocks unsafe topics",
  guardrailId: GUARDRAIL_ID,
  guardrailArn: "arn:aws:bedrock:local::guardrail/gr-1",
  version: "1",
  status: "READY",
  blockedInputMessaging: "Blocked by guardrail.",
  blockedOutputsMessaging: "Blocked by guardrail.",
  createdAt: new Date("2026-01-01T00:00:00Z"),
  updatedAt: new Date("2026-01-02T00:00:00Z"),
  topicPolicy: {
    topics: [
      {
        name: "Weapons manufacturing",
        definition: "Instructions for building weapons",
        examples: ["how do I make a bomb"],
        type: "DENY",
      },
    ],
  },
} satisfies GetGuardrailCommandOutput

function renderSeeded(
  guardrail: GetGuardrailCommandOutput,
  versions: GuardrailSummary[] = [],
) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  queryClient.setQueryData(
    guardrailQueryOptions(GUARDRAIL_ID).queryKey,
    guardrail,
  )
  queryClient.setQueryData(
    guardrailVersionsQueryOptions(GUARDRAIL_ID).queryKey,
    {
      $metadata: {},
      guardrails: versions,
    },
  )
  return render(
    <QueryClientProvider client={queryClient}>
      <GuardrailDetailPage guardrailId={GUARDRAIL_ID} />
    </QueryClientProvider>,
  )
}

describe("GuardrailDetailPage", () => {
  it("renders guardrail details", () => {
    renderSeeded(GUARDRAIL)
    expect(screen.getByText("content-safety")).toBeInTheDocument()
    expect(
      screen.getByText("arn:aws:bedrock:local::guardrail/gr-1"),
    ).toBeInTheDocument()
    expect(screen.getByText("Blocks unsafe topics")).toBeInTheDocument()
  })

  it("renders denied topic rows with name, definition and examples", () => {
    renderSeeded(GUARDRAIL)
    expect(screen.getByText("Weapons manufacturing")).toBeInTheDocument()
    expect(
      screen.getByText("Instructions for building weapons"),
    ).toBeInTheDocument()
    expect(screen.getByText("how do I make a bomb")).toBeInTheDocument()
  })

  it("shows the empty state when there is no topic policy", () => {
    renderSeeded({ ...GUARDRAIL, topicPolicy: undefined })
    expect(
      screen.getByText("No topic policy configured for this guardrail."),
    ).toBeInTheDocument()
  })

  it("shows configured word and sensitive information policies", () => {
    renderSeeded({
      ...GUARDRAIL,
      wordPolicy: { words: [{ text: "badword" }] },
      sensitiveInformationPolicy: {
        piiEntities: [{ type: "EMAIL", action: "BLOCK" }],
      },
    })
    expect(screen.getByText(/Word policy: configured/)).toBeInTheDocument()
    expect(
      screen.getByText(/Sensitive information policy: configured/),
    ).toBeInTheDocument()
  })

  it("shows not-configured for policies that are absent", () => {
    renderSeeded(GUARDRAIL)
    expect(screen.getByText(/Word policy: not configured/)).toBeInTheDocument()
    expect(
      screen.getByText(/Sensitive information policy: not configured/),
    ).toBeInTheDocument()
  })

  it("renders the version list", () => {
    renderSeeded(GUARDRAIL, [
      {
        id: GUARDRAIL_ID,
        arn: "arn:aws:bedrock:local::guardrail/gr-1",
        name: "content-safety",
        status: "READY",
        version: "DRAFT",
        createdAt: new Date("2026-01-01T00:00:00Z"),
        updatedAt: new Date("2026-01-01T00:00:00Z"),
      },
    ])
    expect(screen.getByText("DRAFT")).toBeInTheDocument()
  })

  it("renders the guardrail tester", () => {
    renderSeeded(GUARDRAIL)
    expect(screen.getByText("Guardrail tester")).toBeInTheDocument()
  })
})

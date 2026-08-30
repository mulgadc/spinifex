import type {
  GetGuardrailCommandOutput,
  GuardrailSummary,
} from "@aws-sdk/client-bedrock"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen, waitFor } from "@testing-library/react"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

const mockRuntimeSend = vi.fn().mockResolvedValue({
  action: "NONE",
  assessments: [],
  outputs: [],
  usage: {},
})
const mockBedrockSend = vi.fn().mockResolvedValue({})
const mockNavigate = vi.fn()

vi.mock("@/lib/awsClient", () => ({
  getBedrockClient: () => ({ send: mockBedrockSend }),
  getBedrockRuntimeClient: () => ({ send: mockRuntimeSend }),
}))

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
  useNavigate: () => mockNavigate,
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

  it("renders an edit link to the edit route", () => {
    renderSeeded(GUARDRAIL)
    expect(screen.getByRole("link", { name: /Edit/ })).toHaveAttribute(
      "href",
      "/bedrock/list-guardrails/$guardrailId/edit/gr-1",
    )
  })

  it("deletes the guardrail and navigates back on confirm", async () => {
    mockBedrockSend.mockResolvedValueOnce({})
    renderSeeded(GUARDRAIL)

    screen.getByRole("button", { name: "Delete" }).click()
    await screen.findByText("Delete Guardrail")
    const deleteButtons = screen.getAllByRole("button", { name: "Delete" })
    deleteButtons.at(-1)?.click()

    await waitFor(() => {
      expect(mockBedrockSend).toHaveBeenCalled()
    })
    const input = mockBedrockSend.mock.calls[0]?.[0].input as {
      guardrailIdentifier: string
    }
    expect(input.guardrailIdentifier).toBe(GUARDRAIL_ID)
    await waitFor(() => {
      expect(mockNavigate).toHaveBeenCalledWith({
        to: "/bedrock/list-guardrails",
      })
    })
  })

  it("creates a new version for the guardrail", async () => {
    mockBedrockSend.mockResolvedValueOnce({ version: "2" })
    renderSeeded(GUARDRAIL)

    screen.getByRole("button", { name: "Create version" }).click()

    await waitFor(() => {
      expect(mockBedrockSend).toHaveBeenCalled()
    })
    const input = mockBedrockSend.mock.calls[0]?.[0].input as {
      guardrailIdentifier: string
    }
    expect(input.guardrailIdentifier).toBe(GUARDRAIL_ID)
    await expect(
      screen.findByText("Version 2 created."),
    ).resolves.toBeInTheDocument()
  })
})

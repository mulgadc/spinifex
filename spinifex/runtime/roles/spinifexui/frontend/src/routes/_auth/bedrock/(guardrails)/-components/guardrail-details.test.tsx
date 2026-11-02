import type {
  GetGuardrailCommandOutput,
  GuardrailSummary,
} from "@aws-sdk/client-bedrock"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

const mockRuntimeSend = vi.hoisted(() =>
  vi.fn().mockResolvedValue({
    action: "NONE",
    assessments: [],
    outputs: [],
    usage: {},
  }),
)

const mockBedrockSend = vi.hoisted(() => vi.fn().mockResolvedValue({}))

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockBedrockSend, {
    getBedrockRuntimeClient: mockRuntimeSend,
  })
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import type { PanelTab } from "@/lib/panel-search"
import {
  guardrailQueryOptions,
  guardrailVersionsQueryOptions,
} from "@/queries/bedrock"

import { GuardrailDetails } from "./guardrail-details"

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
  tab: PanelTab = "overview",
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

  const onClose = vi.fn()
  render(
    <QueryClientProvider client={queryClient}>
      <GuardrailDetails
        guardrail={{ id: GUARDRAIL_ID, name: guardrail.name, status: "READY" }}
        onClose={onClose}
        onTabChange={vi.fn()}
        tab={tab}
      />
    </QueryClientProvider>,
  )

  return onClose
}

describe("GuardrailDetails", () => {
  it("renders guardrail details", () => {
    renderSeeded(GUARDRAIL)
    expect(
      screen.getByRole("heading", { name: "content-safety" }),
    ).toBeInTheDocument()
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

  it("renders the guardrail tester on the settings tab", () => {
    renderSeeded(GUARDRAIL, [], "settings")
    expect(screen.getByText("Guardrail tester")).toBeInTheDocument()
  })

  it("renders an edit link to the edit route", () => {
    renderSeeded(GUARDRAIL, [], "settings")
    expect(screen.getByRole("link", { name: /Edit/ })).toHaveAttribute(
      "href",
      "/bedrock/list-guardrails/gr-1/edit",
    )
  })

  it("deletes the guardrail and closes on confirm", async () => {
    mockBedrockSend.mockResolvedValueOnce({})
    const user = userEvent.setup()
    const onClose = renderSeeded(GUARDRAIL, [], "settings")

    await user.click(screen.getByRole("button", { name: "Delete guardrail" }))
    await user.click(await screen.findByRole("button", { name: "Delete" }))

    await waitFor(() => {
      expect(mockBedrockSend).toHaveBeenCalled()
    })

    const input = mockBedrockSend.mock.calls[0]?.[0].input as {
      guardrailIdentifier: string
    }

    expect(input.guardrailIdentifier).toBe(GUARDRAIL_ID)
    await waitFor(() => {
      expect(onClose).toHaveBeenCalled()
    })
  })

  it("creates a new version for the guardrail", async () => {
    mockBedrockSend.mockResolvedValueOnce({ version: "2" })
    const user = userEvent.setup()
    renderSeeded(GUARDRAIL, [], "settings")

    await user.click(screen.getByRole("button", { name: "Create version" }))

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

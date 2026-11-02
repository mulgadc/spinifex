import type { ListFoundationModelsCommandOutput } from "@aws-sdk/client-bedrock"
import { screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

const mockSend = vi.hoisted(() =>
  vi.fn().mockResolvedValue({
    knowledgeBase: { knowledgeBaseId: "kb-new" },
  }),
)

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import { foundationModelsQueryOptions } from "@/queries/bedrock"
import { createTestQueryClient, renderWithClient } from "@/test/render"

import { KnowledgeBaseFormPage } from "./knowledge-base-form-page"

const MODELS: ListFoundationModelsCommandOutput = {
  $metadata: {},
  modelSummaries: [
    {
      modelArn: "arn:aws:bedrock:local::foundation-model/nomic-embed-text-v1.5",
      modelId: "nomic-embed-text-v1.5",
      modelName: "Nomic Embed Text v1.5",
      outputModalities: ["EMBEDDING"],
    },
    {
      modelArn: "arn:aws:bedrock:local::foundation-model/meta.llama3-2-1b",
      modelId: "meta.llama3-2-1b",
      modelName: "Llama 3.2 1B Instruct",
      outputModalities: ["TEXT"],
    },
  ],
}

function renderCreate(onClose = vi.fn()) {
  const queryClient = createTestQueryClient()
  queryClient.setQueryData(foundationModelsQueryOptions.queryKey, MODELS)

  renderWithClient(<KnowledgeBaseFormPage onClose={onClose} />, queryClient)

  return onClose
}

describe("KnowledgeBaseFormPage", () => {
  it("renders the required fields", () => {
    renderCreate()
    expect(screen.getByLabelText("Name")).toBeInTheDocument()
    expect(screen.getByLabelText("Embedding model")).toBeInTheDocument()
    expect(screen.getByLabelText("Dimensions")).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: "Create Knowledge Base" }),
    ).toBeInTheDocument()
  })

  it("notes that data sources are added after the knowledge base is created", () => {
    renderCreate()
    expect(
      screen.getByText(/add one or more S3 data sources to it/),
    ).toBeInTheDocument()
  })

  it("only offers models with an EMBEDDING output modality", async () => {
    const user = userEvent.setup()
    renderCreate()

    await user.click(screen.getByLabelText("Embedding model"))
    expect(
      screen.getByRole("option", { name: "Nomic Embed Text v1.5" }),
    ).toBeInTheDocument()
    expect(
      screen.queryByRole("option", { name: "Llama 3.2 1B Instruct" }),
    ).toBeNull()
  })

  it("submits CreateKnowledgeBaseCommand and opens the new knowledge base", async () => {
    const user = userEvent.setup()
    const onClose = renderCreate()

    await user.type(screen.getByLabelText("Name"), "docs-kb")
    await user.click(screen.getByLabelText("Embedding model"))
    await user.click(
      screen.getByRole("option", { name: "Nomic Embed Text v1.5" }),
    )
    await user.click(
      screen.getByRole("button", { name: "Create Knowledge Base" }),
    )

    await screen.findByRole("button", { name: "Create Knowledge Base" })
    expect(mockSend).toHaveBeenCalled()
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.name).toBe("docs-kb")
    expect(
      input.knowledgeBaseConfiguration.vectorKnowledgeBaseConfiguration
        .embeddingModelArn,
    ).toBe("arn:aws:bedrock:local::foundation-model/nomic-embed-text-v1.5")
    expect(onClose).toHaveBeenCalledWith("kb-new")
  })

  it("prefills dimensions with the known value for the selected model", async () => {
    const user = userEvent.setup()
    renderCreate()

    await user.click(screen.getByLabelText("Embedding model"))
    await user.click(
      screen.getByRole("option", { name: "Nomic Embed Text v1.5" }),
    )

    expect(screen.getByLabelText("Dimensions")).toHaveValue(768)
  })

  it("closes the panel on cancel", async () => {
    const user = userEvent.setup()
    const onClose = renderCreate()

    await user.click(screen.getByRole("button", { name: "Cancel" }))

    expect(onClose).toHaveBeenCalledWith()
  })
})

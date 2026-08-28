import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

type AdminQueries = typeof import("@/queries/admin")

const { mockGrantModelAccess, mockRevokeModelAccess } = vi.hoisted(() => ({
  mockGrantModelAccess: vi.fn<AdminQueries["grantModelAccess"]>(),
  mockRevokeModelAccess: vi.fn<AdminQueries["revokeModelAccess"]>(),
}))

vi.mock("@/queries/admin", async () => {
  const actual = await vi.importActual<AdminQueries>("@/queries/admin")
  return {
    ...actual,
    grantModelAccess: mockGrantModelAccess,
    revokeModelAccess: mockRevokeModelAccess,
  }
})

import {
  adminModelAccessQueryOptions,
  adminOchreCatalogQueryOptions,
  type AdminCatalogEntry,
} from "@/queries/admin"
import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

import { ModelAccessPage } from "./model-access-page"

const CATALOG_ENTRY = {
  modelId: "meta.llama3-2-1b-instruct-v1:0",
  modelName: "Llama 3.2 1B Instruct",
  family: "vllm",
  inputModalities: ["TEXT"],
  outputModalities: ["TEXT"],
  responseStreamingSupported: false,
  inputPriceMicroUsdPerMillion: 0,
  outputPriceMicroUsdPerMillion: 0,
  priceKnown: false,
  minVramMib: 5120,
  instanceType: "g5.xlarge",
  coServeGroup: "ochre-demo-bundle",
  availability: "available",
} satisfies AdminCatalogEntry

const OTHER_ENTRY = {
  ...CATALOG_ENTRY,
  modelId: "anthropic.claude-3-haiku",
  modelName: "Claude 3 Haiku",
} satisfies AdminCatalogEntry

const ACCOUNT_ID = "000000000002"

function seed(entries: AdminCatalogEntry[], grantedModelIds: string[]) {
  const qc = createTestQueryClient()
  qc.setQueryData(adminOchreCatalogQueryOptions.queryKey, { entries })
  qc.setQueryData(adminModelAccessQueryOptions(ACCOUNT_ID).queryKey, {
    AccountId: ACCOUNT_ID,
    ModelIds: grantedModelIds,
  })
  return qc
}

describe("ModelAccessPage", () => {
  it("prompts for an account id when none is entered", () => {
    renderWithClient(<ModelAccessPage />, seed([CATALOG_ENTRY], []))
    expect(
      screen.getByText(
        "Enter an account ID to view and manage its model access.",
      ),
    ).toBeInTheDocument()
  })

  it("shows the empty state when the account has no models in the catalog", async () => {
    const user = userEvent.setup()
    renderWithClient(<ModelAccessPage />, seed([], []))

    await user.type(screen.getByPlaceholderText("Account ID"), ACCOUNT_ID)

    expect(screen.getByText("No models found.")).toBeInTheDocument()
  })

  it("renders the catalog cross-referenced with grants", async () => {
    const user = userEvent.setup()
    renderWithClient(
      <ModelAccessPage />,
      seed([CATALOG_ENTRY, OTHER_ENTRY], [CATALOG_ENTRY.modelId]),
    )

    await user.type(screen.getByPlaceholderText("Account ID"), ACCOUNT_ID)

    expect(screen.getByText("Llama 3.2 1B Instruct")).toBeInTheDocument()
    expect(screen.getByText("Claude 3 Haiku")).toBeInTheDocument()
    expect(screen.getByText("Granted")).toBeInTheDocument()
    expect(screen.getByText("Ungranted")).toBeInTheDocument()
    expect(screen.getByRole("button", { name: "Revoke" })).toBeInTheDocument()
    expect(screen.getByRole("button", { name: "Grant" })).toBeInTheDocument()
  })

  it("grants access with the entered account id and the row's model id", async () => {
    mockGrantModelAccess.mockResolvedValue({
      AccountId: ACCOUNT_ID,
      ModelId: OTHER_ENTRY.modelId,
    })
    const user = userEvent.setup()
    renderWithClient(<ModelAccessPage />, seed([OTHER_ENTRY], []))

    await user.type(screen.getByPlaceholderText("Account ID"), ACCOUNT_ID)
    await user.click(screen.getByRole("button", { name: "Grant" }))

    await waitFor(() => {
      expect(mockGrantModelAccess.mock.calls[0]?.[0]).toStrictEqual({
        accountId: ACCOUNT_ID,
        modelId: OTHER_ENTRY.modelId,
      })
    })
  })

  it("revokes access with the entered account id and the row's model id", async () => {
    mockRevokeModelAccess.mockResolvedValue({
      AccountId: ACCOUNT_ID,
      ModelId: CATALOG_ENTRY.modelId,
    })
    const user = userEvent.setup()
    renderWithClient(
      <ModelAccessPage />,
      seed([CATALOG_ENTRY], [CATALOG_ENTRY.modelId]),
    )

    await user.type(screen.getByPlaceholderText("Account ID"), ACCOUNT_ID)
    await user.click(screen.getByRole("button", { name: "Revoke" }))

    await waitFor(() => {
      expect(mockRevokeModelAccess.mock.calls[0]?.[0]).toStrictEqual({
        accountId: ACCOUNT_ID,
        modelId: CATALOG_ENTRY.modelId,
      })
    })
  })
})

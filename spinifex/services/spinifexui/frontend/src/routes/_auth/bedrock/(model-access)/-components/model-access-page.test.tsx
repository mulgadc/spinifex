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
  adminListAccountsQueryOptions,
  adminModelAccessQueryOptions,
  adminOchreCatalogQueryOptions,
  type AccountSummary,
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
const ACCOUNT_NAME = "Tenant One"

const ACCOUNT = {
  accountId: ACCOUNT_ID,
  accountName: ACCOUNT_NAME,
  status: "ACTIVE",
  createdAt: "2026-01-01T00:00:00Z",
} satisfies AccountSummary

function seed({
  entries = [],
  grantedModelIds = [],
  accounts,
}: {
  entries?: AdminCatalogEntry[]
  grantedModelIds?: string[]
  accounts?: AccountSummary[]
}) {
  const qc = createTestQueryClient()
  qc.setQueryData(adminOchreCatalogQueryOptions.queryKey, { entries })
  qc.setQueryData(adminModelAccessQueryOptions(ACCOUNT_ID).queryKey, {
    AccountId: ACCOUNT_ID,
    ModelIds: grantedModelIds,
  })
  if (accounts !== undefined) {
    qc.setQueryData(adminListAccountsQueryOptions.queryKey, {
      accounts,
      count: accounts.length,
    })
  }
  return qc
}

async function selectAccount(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByLabelText("Account"))
  await user.click(
    screen.getByRole("option", { name: new RegExp(ACCOUNT_NAME) }),
  )
}

describe("ModelAccessPage", () => {
  describe("account picker", () => {
    it("lists accounts sorted by name, excluding the global account", async () => {
      const user = userEvent.setup()
      renderWithClient(
        <ModelAccessPage />,
        seed({
          accounts: [
            { ...ACCOUNT, accountId: "000000000003", accountName: "Zeta Co" },
            ACCOUNT,
            {
              accountId: "000000000000",
              accountName: "Global",
              status: "ACTIVE",
              createdAt: "",
            },
          ],
        }),
      )

      await user.click(screen.getByLabelText("Account"))

      const options = screen.getAllByRole("option").map((el) => el.textContent)
      expect(options).toStrictEqual([
        "Tenant One (000000000002) — ACTIVE",
        "Zeta Co (000000000003) — ACTIVE",
      ])
    })

    it("selecting an account drives the catalog cross-referenced with grants", async () => {
      const user = userEvent.setup()
      renderWithClient(
        <ModelAccessPage />,
        seed({
          entries: [CATALOG_ENTRY, OTHER_ENTRY],
          grantedModelIds: [CATALOG_ENTRY.modelId],
          accounts: [ACCOUNT],
        }),
      )

      await selectAccount(user)

      expect(screen.getByText("Llama 3.2 1B Instruct")).toBeInTheDocument()
      expect(screen.getByText("Claude 3 Haiku")).toBeInTheDocument()
      expect(screen.getByText("Granted")).toBeInTheDocument()
      expect(screen.getByText("Ungranted")).toBeInTheDocument()
    })

    it("grants access with the selected account id", async () => {
      mockGrantModelAccess.mockResolvedValue({
        AccountId: ACCOUNT_ID,
        ModelId: OTHER_ENTRY.modelId,
      })
      const user = userEvent.setup()
      renderWithClient(
        <ModelAccessPage />,
        seed({ entries: [OTHER_ENTRY], accounts: [ACCOUNT] }),
      )

      await selectAccount(user)
      await user.click(screen.getByRole("button", { name: "Grant" }))

      await waitFor(() => {
        expect(mockGrantModelAccess.mock.calls[0]?.[0]).toStrictEqual({
          accountId: ACCOUNT_ID,
          modelId: OTHER_ENTRY.modelId,
        })
      })
    })

    it("revokes access with the selected account id", async () => {
      mockRevokeModelAccess.mockResolvedValue({
        AccountId: ACCOUNT_ID,
        ModelId: CATALOG_ENTRY.modelId,
      })
      const user = userEvent.setup()
      renderWithClient(
        <ModelAccessPage />,
        seed({
          entries: [CATALOG_ENTRY],
          grantedModelIds: [CATALOG_ENTRY.modelId],
          accounts: [ACCOUNT],
        }),
      )

      await selectAccount(user)
      await user.click(screen.getByRole("button", { name: "Revoke" }))

      await waitFor(() => {
        expect(mockRevokeModelAccess.mock.calls[0]?.[0]).toStrictEqual({
          accountId: ACCOUNT_ID,
          modelId: CATALOG_ENTRY.modelId,
        })
      })
    })
  })

  describe("free-text fallback", () => {
    it("falls back to a validated text input when the account list is empty", () => {
      renderWithClient(<ModelAccessPage />, seed({ accounts: [] }))
      expect(
        screen.getByText(
          "Enter a 12-digit account ID — account list unavailable.",
        ),
      ).toBeInTheDocument()
    })

    it("falls back to a validated text input when the account list errors", async () => {
      // adminListAccountsQueryOptions is left unseeded, so its real queryFn
      // runs and throws "Not authenticated" (no stored session in this env).
      renderWithClient(<ModelAccessPage />, seed({}))
      await expect(
        screen.findByText(
          "Enter a 12-digit account ID — account list unavailable.",
        ),
      ).resolves.toBeInTheDocument()
    })

    it("rejects a non-12-digit account id and does not show the catalog", async () => {
      const user = userEvent.setup()
      renderWithClient(
        <ModelAccessPage />,
        seed({ entries: [CATALOG_ENTRY], accounts: [] }),
      )

      await user.type(screen.getByPlaceholderText("123456789012"), "12345")

      expect(
        screen.getByText("Enter a 12-digit numeric account ID."),
      ).toBeInTheDocument()
      expect(
        screen.queryByText("Llama 3.2 1B Instruct"),
      ).not.toBeInTheDocument()
    })

    it("shows the catalog once a valid 12-digit account id is entered", async () => {
      const user = userEvent.setup()
      renderWithClient(
        <ModelAccessPage />,
        seed({
          entries: [CATALOG_ENTRY],
          grantedModelIds: [CATALOG_ENTRY.modelId],
          accounts: [],
        }),
      )

      await user.type(screen.getByPlaceholderText("123456789012"), ACCOUNT_ID)

      expect(screen.getByText("Llama 3.2 1B Instruct")).toBeInTheDocument()
      expect(screen.getByText("Granted")).toBeInTheDocument()
    })

    it("grants access with the validated free-text account id", async () => {
      mockGrantModelAccess.mockResolvedValue({
        AccountId: ACCOUNT_ID,
        ModelId: CATALOG_ENTRY.modelId,
      })
      const user = userEvent.setup()
      renderWithClient(
        <ModelAccessPage />,
        seed({ entries: [CATALOG_ENTRY], accounts: [] }),
      )

      await user.type(screen.getByPlaceholderText("123456789012"), ACCOUNT_ID)
      await user.click(screen.getByRole("button", { name: "Grant" }))

      await waitFor(() => {
        expect(mockGrantModelAccess.mock.calls[0]?.[0]).toStrictEqual({
          accountId: ACCOUNT_ID,
          modelId: CATALOG_ENTRY.modelId,
        })
      })
    })
  })
})

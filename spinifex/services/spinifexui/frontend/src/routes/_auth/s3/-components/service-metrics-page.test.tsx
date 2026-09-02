import { screen, within } from "@testing-library/react"
import { beforeEach, describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

vi.mock("@/lib/auth", () => ({
  getCredentials: vi.fn(() => ({
    accessKeyId: "ASIAak",
    secretAccessKey: "sk",
    sessionToken: "token",
    expiration: new Date(Date.now() + 60_000).toISOString(),
  })),
}))

vi.mock("@/lib/signed-fetch", () => ({
  signedFetch: vi.fn(),
  signedAdminFetch: vi.fn(),
}))

vi.mock("@/contexts/admin-context", () => ({
  useAdmin: () => ({ isAdmin: true }),
}))

import { signedFetch } from "@/lib/signed-fetch"
import type { MetaNodeStatus, StorageStatusOutput } from "@/queries/admin"

import { ServiceMetricsPage } from "./service-metrics-page"

const mockSignedFetch = vi.mocked(signedFetch)

function metaNode(
  id: number,
  overrides: Partial<MetaNodeStatus> = {},
): MetaNodeStatus {
  return {
    id,
    host: "10.0.0.1",
    port: 9000 + id,
    healthy: true,
    state: "Follower",
    leader: "node-1",
    term: "7",
    commit_index: "42",
    applied_index: "42",
    is_leader: false,
    ...overrides,
  }
}

function storageStatus(nodes: MetaNodeStatus[]): StorageStatusOutput {
  return {
    encoding: { type: "Reed-Solomon", data_shards: 4, parity_shards: 2 },
    meta_nodes: nodes,
    blob_nodes: [],
    buckets: [],
  }
}

async function renderPage(nodes: MetaNodeStatus[]) {
  mockSignedFetch.mockResolvedValue(storageStatus(nodes))
  renderWithClient(<ServiceMetricsPage />, createTestQueryClient())
  // The encoding badge renders only once the query resolves.
  await screen.findAllByText(/Reed-Solomon/)
}

// clusterHealth reads the overview badge only. The per-node Health column uses
// the same words, so an unscoped query would match either.
function clusterHealth(): string {
  const badge =
    screen.getByText("Cluster Health").parentElement?.lastElementChild
  if (!badge) {
    throw new Error("Cluster Health label has no badge beside it")
  }
  return badge.textContent ?? ""
}

// metaRow returns one body row of the meta nodes table; row 0 is the header.
function metaRow(index: number): HTMLElement {
  const row = screen.getAllByRole("row")[index + 1]
  if (!row) {
    throw new Error(`no meta node row at index ${index}`)
  }
  return row
}

describe("service metrics cluster health", () => {
  beforeEach(() => {
    mockSignedFetch.mockReset()
  })

  it("reports Healthy when every node answers and names the same leader", async () => {
    await renderPage([
      metaNode(1, { state: "Leader", is_leader: true }),
      metaNode(2),
      metaNode(3),
    ])

    expect(clusterHealth()).toBe("Healthy")
  })

  it("reports Degraded when the nodes answer but observe no leader", async () => {
    await renderPage([
      metaNode(1, { state: "Candidate", leader: "" }),
      metaNode(2, { state: "Candidate", leader: "" }),
      metaNode(3, { state: "Candidate", leader: "" }),
    ])

    expect(clusterHealth()).toBe("Degraded")
  })

  it("reports Degraded when the nodes disagree about the leader", async () => {
    await renderPage([
      metaNode(1, { leader: "node-1" }),
      metaNode(2, { leader: "node-3" }),
      metaNode(3, { leader: "node-3" }),
    ])

    expect(clusterHealth()).toBe("Degraded")
  })

  it("reports Degraded when a node does not answer", async () => {
    await renderPage([
      metaNode(1, { state: "Leader", is_leader: true }),
      metaNode(2),
      metaNode(3, {
        healthy: false,
        state: "",
        leader: "",
        term: undefined,
        commit_index: undefined,
        applied_index: undefined,
      }),
    ])

    expect(clusterHealth()).toBe("Degraded")
    expect(within(metaRow(2)).getByText("Unreachable")).toBeInTheDocument()
  })

  it("reports Unknown when the topology has no meta nodes", async () => {
    await renderPage([])

    expect(clusterHealth()).toBe("Unknown")
  })
})

describe("service metrics raft columns", () => {
  beforeEach(() => {
    mockSignedFetch.mockReset()
  })

  it("renders the raft state each node reports", async () => {
    await renderPage([metaNode(1, { state: "Leader", is_leader: true })])

    const row = within(metaRow(0))
    expect(row.getByText("Leader")).toBeInTheDocument()
    expect(row.getByText("node-1")).toBeInTheDocument()
    expect(row.getByText("7")).toBeInTheDocument()
    expect(row.getAllByText("42")).toHaveLength(2)
  })

  it("renders a placeholder for every raft column of a node that did not answer", async () => {
    await renderPage([
      metaNode(1, {
        healthy: false,
        state: "",
        leader: undefined,
        term: undefined,
        commit_index: undefined,
        applied_index: undefined,
      }),
    ])

    const row = within(metaRow(0))
    expect(row.getByText("Unreachable")).toBeInTheDocument()
    // State, Leader, Term, Commit Index and Applied Index.
    expect(row.getAllByText("-")).toHaveLength(5)
  })
})

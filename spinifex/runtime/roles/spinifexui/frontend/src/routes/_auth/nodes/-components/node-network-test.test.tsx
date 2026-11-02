import { screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"

import { networkTestsQueryOptions, type NodeFacts } from "@/queries/nodes"
import { createTestQueryClient, renderWithClient } from "@/test/render"

import { NodeNetworkTest } from "./node-network-test"

// SAFETY: NodeNetworkTest reads only planes, so the other NodeFacts fields are left out.
const facts = {
  node: "node2",
  planes: [
    { plane: "lan", address: "10.10.8.5", prefix: 24, nics: ["enp8s0"] },
  ],
} as NodeFacts

function renderTest() {
  const queryClient = createTestQueryClient()

  queryClient.setQueryData(networkTestsQueryOptions.queryKey, { tests: [] })

  return renderWithClient(
    <NodeNetworkTest
      facts={facts}
      node="node2"
      peers={["node1", "node2", "node3"]}
    />,
    queryClient,
  )
}

describe("NodeNetworkTest", () => {
  it("shows each select's option label, never its raw value", () => {
    renderTest()

    expect(screen.getByLabelText("Peer")).toHaveTextContent(
      "Every node (full mesh)",
    )
    expect(screen.queryByText("__all__")).not.toBeInTheDocument()
  })
})

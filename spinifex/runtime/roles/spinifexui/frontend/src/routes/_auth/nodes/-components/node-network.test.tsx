import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"

import type { NodeFacts } from "@/queries/nodes"

import { NodeNetwork } from "./node-network"

const OVERLAY_NOTE = /The VPC overlay rides the LAN plane/

// A Vultr VM: no bridges, the cluster and overlay on one private NIC.
const cloudVM = {
  node: "node2",
  planes: [
    {
      plane: "wan",
      address: "64.177.121.61",
      prefix: 23,
      nics: ["enp1s0"],
      mtu: 1500,
    },
    {
      plane: "lan",
      address: "10.10.8.4",
      prefix: 24,
      nics: ["enp8s0"],
      mtu: 1450,
    },
    {
      plane: "vpc",
      address: "10.10.8.4",
      prefix: 24,
      nics: ["enp8s0"],
      mtu: 1450,
    },
  ],
} as NodeFacts

describe("NodeNetwork planes", () => {
  it("lists a plane on a plain NIC without an empty bridge", () => {
    render(<NodeNetwork facts={cloudVM} />)

    expect(
      screen.getByText("64.177.121.61/23 · enp1s0 · MTU 1500"),
    ).toBeInTheDocument()
    expect(screen.getByText(OVERLAY_NOTE)).toBeInTheDocument()
  })

  it("names the bridge when the plane sits on one", () => {
    const bridged = {
      ...cloudVM,
      planes: [
        {
          plane: "lan",
          address: "10.10.8.4",
          prefix: 24,
          bridge: "br-lan",
          nics: ["enp193s0f1np1"],
        },
        {
          plane: "vpc",
          address: "10.10.8.4",
          prefix: 24,
          bridge: "br-lan",
          nics: ["enp193s0f1np1"],
        },
      ],
    } as NodeFacts

    render(<NodeNetwork facts={bridged} />)

    expect(
      screen.getAllByText("10.10.8.4/24 · br-lan · enp193s0f1np1"),
    ).toHaveLength(2)
    expect(screen.getByText(OVERLAY_NOTE)).toBeInTheDocument()
  })

  it("does not claim a shared plane when the overlay has its own port", () => {
    const split = {
      ...cloudVM,
      planes: [
        { plane: "lan", address: "10.10.8.4", prefix: 24, nics: ["enp8s0"] },
        { plane: "vpc", address: "10.10.9.4", prefix: 24, nics: ["enp9s0"] },
      ],
    } as NodeFacts

    render(<NodeNetwork facts={split} />)

    expect(screen.queryByText(OVERLAY_NOTE)).not.toBeInTheDocument()
  })
})

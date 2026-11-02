import { describe, expect, it } from "vitest"

import type { NICFacts, NodeFacts } from "@/queries/nodes"

import {
  buildTopology,
  formatBitRate,
  formatLinkSpeed,
  layoutTopology,
  rateThroughput,
} from "./node-topology"

function node(name: string, nics: NICFacts[]): NodeFacts {
  return {
    node: name,
    form_factor: "rack-server",
    system: { vendor: "GIGABYTE", product: "H262-Z62-ZB" },
    os: {},
    cpu: { sockets: 2, cores: 64, threads: 128 },
    memory: { total_bytes: 0, slots_used: 0, slots_total: 0, groups: [] },
    nics,
    planes: [],
    sources: {},
  }
}

const crs = {
  chassis_id: "d0:ea:11:a8:91:4c",
  system_name: "CRS520",
}

// As prod reports it: every port has an LLDP neighbour, and both 100G ports
// of every node land on one switch.
const prod = ["node1", "node2"].map((name, i) =>
  node(name, [
    {
      name: "eno1",
      state: "up",
      speed_mbps: 1000,
      planes: ["wan"],
      neighbor: {
        chassis_id: "aa:aa",
        system_name: "MikroTik 1gbe",
        port_id: `ether${18 + i}`,
      },
    },
    {
      name: "enp33s0f0np0",
      state: "up",
      speed_mbps: 100_000,
      planes: ["lan"],
      neighbor: { ...crs, port_id: `qsfp28-${7 + i}-1` },
    },
    {
      name: "enp33s0f1np1",
      state: "up",
      speed_mbps: 100_000,
      planes: ["vpc"],
      neighbor: { ...crs, port_id: `qsfp28-${7 + i}-2` },
    },
    { name: "enp1s0", state: "down" },
  ]),
)

describe("buildTopology", () => {
  it("draws one switch per LLDP chassis, however many nodes reach it", () => {
    const topology = buildTopology(prod)
    const switches = topology.devices.filter((d) => d.kind === "switch")

    expect(switches.map((d) => d.label).toSorted()).toStrictEqual([
      "CRS520",
      "MikroTik 1gbe",
    ])
    expect(
      switches.find((d) => d.label === "CRS520")?.planes.toSorted(),
    ).toStrictEqual(["lan", "vpc"])
    expect(
      topology.links.filter((l) => l.target === "switch:d0:ea:11:a8:91:4c"),
    ).toHaveLength(4)
    expect(topology.links.find((l) => l.id === "node2:eno1")?.port).toBe(
      "ether19",
    )
  })

  it("leaves out ports that carry no plane", () => {
    expect(
      buildTopology(prod).links.some((l) => l.nic === "enp1s0"),
    ).toBeFalsy()
  })

  it("puts the internet behind whatever carries WAN", () => {
    const topology = buildTopology(prod)

    expect(topology.links.filter((l) => l.target === "internet")).toStrictEqual(
      [expect.objectContaining({ source: "switch:aa:aa" })],
    )
  })

  it("draws a generic network per plane set where LLDP is silent", () => {
    // dev-prod: no networkd, and LAN and VPC share one 25 GbE port.
    const devprod = ["bottlebrush", "ironbark"].map((name) =>
      node(name, [
        { name: "eno1", state: "up", speed_mbps: 1000, planes: ["wan"] },
        {
          name: "enp193s0f1np1",
          state: "up",
          speed_mbps: 25_000,
          planes: ["lan", "vpc"],
        },
      ]),
    )

    const topology = buildTopology(devprod)

    expect(
      topology.devices
        .filter((d) => d.kind === "segment")
        .map((d) => d.label)
        .toSorted(),
    ).toStrictEqual(["LAN + VPC network", "WAN network"])
    expect(
      topology.links.filter((l) => l.target === "segment:lan+vpc"),
    ).toHaveLength(2)
  })

  it("lays switches carrying WAN nearest the internet", () => {
    const placed = layoutTopology(buildTopology(prod))
    const at = (id: string) => placed.find((p) => p.id === id)

    expect(at("internet")?.y).toBeLessThan(at("switch:aa:aa")?.y ?? 0)
    expect(at("switch:aa:aa")?.x).toBeLessThan(
      at("switch:d0:ea:11:a8:91:4c")?.x ?? 0,
    )
    expect(at("node:node1")?.y).toBeGreaterThan(at("switch:aa:aa")?.y ?? 0)
  })

  it("lays the tiers out as columns when drawn across", () => {
    const placed = layoutTopology(buildTopology(prod), "across")
    const at = (id: string) => placed.find((p) => p.id === id)

    expect(at("internet")?.x).toBeLessThan(at("switch:aa:aa")?.x ?? 0)
    expect(at("switch:aa:aa")?.y).toBeLessThan(
      at("switch:d0:ea:11:a8:91:4c")?.y ?? 0,
    )
    expect(at("node:node1")?.x).toBeGreaterThan(at("switch:aa:aa")?.x ?? 0)
    expect(at("node:node1")?.y).toBeLessThan(at("node:node2")?.y ?? 0)
  })
})

describe("rateThroughput", () => {
  it("rates against the port's own line rate", () => {
    expect(rateThroughput(940e6, 1000).rating).toBe("excellent")
    expect(rateThroughput(23.5e9, 25_000).rating).toBe("excellent")
    expect(rateThroughput(18e9, 25_000).rating).toBe("good")
    expect(rateThroughput(0.99e9, 25_000).rating).toBe("poor")
    expect(rateThroughput(1e9, undefined).rating).toBe("unknown")
  })

  it("never reads above 100 percent", () => {
    expect(rateThroughput(30e9, 25_000).percent).toBe(100)
  })
})

describe("formatting", () => {
  it("names links the way a switch does", () => {
    expect(formatLinkSpeed(25_000)).toBe("25 GbE")
    expect(formatLinkSpeed(2500)).toBe("2.5 GbE")
    expect(formatLinkSpeed(100)).toBe("100 Mb/s")
    expect(formatLinkSpeed(undefined)).toBe("unknown speed")
  })

  it("scales bit rates", () => {
    expect(formatBitRate(23_456_000_000)).toBe("23.5 Gbit/s")
    expect(formatBitRate(941_000_000)).toBe("941 Mbit/s")
  })
})

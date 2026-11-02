import type { FormFactor, NodeFacts, Plane } from "@/queries/nodes"

export const PLANE_LABELS: Record<Plane, string> = {
  wan: "WAN",
  lan: "LAN",
  vpc: "VPC",
}

export const PLANE_ORDER: readonly Plane[] = ["wan", "lan", "vpc"]

export function planesLabel(planes: readonly string[]): string {
  return PLANE_ORDER.flatMap((plane) =>
    planes.includes(plane) ? [PLANE_LABELS[plane]] : [],
  ).join(" + ")
}

// "25 GbE", "1 GbE", "100 Mb/s": the way a port is named on a switch.
export function formatLinkSpeed(mbps: number | undefined): string {
  if (!mbps || mbps <= 0) {
    return "unknown speed"
  }

  if (mbps >= 1000) {
    const gb = mbps / 1000

    return `${Number.isInteger(gb) ? gb : gb.toFixed(1)} GbE`
  }

  return `${mbps} Mb/s`
}

export function formatBitRate(bps: number): string {
  const units = ["bit/s", "Kbit/s", "Mbit/s", "Gbit/s", "Tbit/s"]
  let value = bps
  let unit = 0

  while (value >= 1000 && unit < units.length - 1) {
    value /= 1000
    unit += 1
  }

  return `${value.toFixed(significantDecimals(value))} ${units[unit]}`
}

// Three significant figures: 941 Mbit/s, 23.5 Gbit/s, 9.41 Gbit/s.
function significantDecimals(value: number): number {
  if (value >= 100) {
    return 0
  }

  return value >= 10 ? 1 : 2
}

export type ThroughputRating =
  | "excellent"
  | "good"
  | "fair"
  | "poor"
  | "unknown"

export interface ThroughputScore {
  percent: number
  rating: ThroughputRating
}

// Measured against the port's own line rate, so a 1 GbE link at 940 Mbit/s
// rates the same as a 25 GbE link at 23.5 Gbit/s.
export function rateThroughput(
  bps: number,
  linkMbps: number | undefined,
): ThroughputScore {
  if (!linkMbps || linkMbps <= 0) {
    return { percent: 0, rating: "unknown" }
  }

  const percent = Math.min(100, (bps / (linkMbps * 1e6)) * 100)

  if (percent >= 90) {
    return { percent, rating: "excellent" }
  }

  if (percent >= 70) {
    return { percent, rating: "good" }
  }

  return { percent, rating: percent >= 40 ? "fair" : "poor" }
}

export type DeviceKind = "node" | "switch" | "segment" | "internet"

export interface TopologyDevice {
  id: string
  kind: DeviceKind
  label: string
  detail?: string
  formFactor?: FormFactor
  planes: string[]
}

export interface TopologyLink {
  id: string
  source: string
  target: string
  planes: string[]
  speedMbps?: number
  // The NIC on the node's end and, when LLDP named it, the switch port.
  nic?: string
  port?: string
}

export interface Topology {
  devices: TopologyDevice[]
  links: TopologyLink[]
}

// Draws what the nodes report: a switch for every LLDP chassis, deduplicated
// across nodes, and a generic network per plane set where LLDP is silent.
export function buildTopology(facts: readonly NodeFacts[]): Topology {
  const devices = new Map<string, TopologyDevice>()
  const links: TopologyLink[] = []
  const reachesWAN = new Set<string>()

  for (const node of facts) {
    const nodePlanes = new Set<string>()

    for (const nic of node.nics ?? []) {
      const planes = nic.planes ?? []

      if (planes.length === 0) {
        continue
      }

      for (const plane of planes) {
        nodePlanes.add(plane)
      }

      let upstream: string

      if (nic.neighbor?.chassis_id) {
        upstream = `switch:${nic.neighbor.chassis_id}`
        const existing = devices.get(upstream)

        devices.set(upstream, {
          id: upstream,
          kind: "switch",
          label: nic.neighbor.system_name || nic.neighbor.chassis_id,
          detail: nic.neighbor.system_description,
          planes: union(existing?.planes ?? [], planes),
        })
      } else {
        const key = PLANE_ORDER.filter((p) => planes.includes(p)).join("+")
        upstream = `segment:${key}`
        devices.set(upstream, {
          id: upstream,
          kind: "segment",
          label: `${planesLabel(planes)} network`,
          detail: "No LLDP neighbour reported",
          planes,
        })
      }

      if (planes.includes("wan")) {
        reachesWAN.add(upstream)
      }

      links.push({
        id: `${node.node}:${nic.name}`,
        source: `node:${node.node}`,
        target: upstream,
        planes,
        speedMbps: nic.speed_mbps,
        nic: nic.name,
        port: nic.neighbor?.port_id || undefined,
      })
    }

    devices.set(`node:${node.node}`, {
      id: `node:${node.node}`,
      kind: "node",
      label: node.node,
      detail: [node.system.vendor, node.system.product]
        .filter(Boolean)
        .join(" "),
      formFactor: node.form_factor,
      planes: [...nodePlanes],
    })
  }

  if (reachesWAN.size > 0) {
    devices.set("internet", {
      id: "internet",
      kind: "internet",
      label: "Internet",
      planes: ["wan"],
    })

    for (const upstream of reachesWAN) {
      links.push({
        id: `internet:${upstream}`,
        source: upstream,
        target: "internet",
        planes: ["wan"],
      })
    }
  }

  return { devices: [...devices.values()], links }
}

function union(a: readonly string[], b: readonly string[]): string[] {
  return [...new Set([...a, ...b])]
}

export interface Placed {
  id: string
  x: number
  y: number
}

// "down" draws the tiers as rows, top to bottom; "across" draws them as
// columns, left to right, so each tier stacks its devices vertically.
export type LayoutDirection = "down" | "across"

// Node cards list their ports, so they are tall; across stacks them with room.
const SPACING: Record<LayoutDirection, { along: number; between: number }> = {
  down: { along: 400, between: 240 },
  across: { along: 230, between: 400 },
}

// Three tiers: the internet, the switches, the nodes. Each tier is centred on
// the widest, and switches carrying WAN sit nearest the internet.
export function layoutTopology(
  topology: Topology,
  direction: LayoutDirection = "down",
): Placed[] {
  const tier = (kind: DeviceKind) =>
    topology.devices
      .filter((device) =>
        kind === "switch"
          ? device.kind === "switch" || device.kind === "segment"
          : device.kind === kind,
      )
      .toSorted((a, b) => {
        const wan =
          Number(b.planes.includes("wan")) - Number(a.planes.includes("wan"))

        return wan === 0 ? a.label.localeCompare(b.label) : wan
      })

  const tiers = [tier("internet"), tier("switch"), tier("node")]
  const widest = Math.max(...tiers.map((row) => row.length), 1)
  const { along, between } = SPACING[direction]

  return tiers.flatMap((row, depth) => {
    const offset = ((widest - row.length) * along) / 2

    return row.map((device, index) => {
      const a = offset + index * along
      const b = depth * between

      return direction === "down"
        ? { id: device.id, x: a, y: b }
        : { id: device.id, x: b, y: a }
    })
  })
}

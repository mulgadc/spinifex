import { type DataColumn, DataTable } from "@/components/data-table"
import {
  PanelField,
  PanelFields,
  PanelSection,
} from "@/components/detail-panel"
import { Badge } from "@/components/ui/badge"
import {
  formatLinkSpeed,
  PLANE_LABELS,
  PLANE_ORDER,
  planesLabel,
} from "@/lib/node-topology"
import type { NICFacts, NodeFacts } from "@/queries/nodes"

const NIC_COLUMNS: DataColumn<NICFacts>[] = [
  {
    id: "name",
    header: "Port",
    sortValue: (nic) => nic.name,
    cell: (nic) => (
      <div className="flex flex-col">
        <span className="font-mono text-xs font-medium">{nic.name}</span>
        {nic.model && (
          <span className="max-w-56 truncate text-2xs text-muted-foreground">
            {nic.model}
          </span>
        )}
      </div>
    ),
  },
  {
    id: "planes",
    header: "Carries",
    sortValue: (nic) => planesLabel(nic.planes ?? []),
    cell: (nic) =>
      nic.planes && nic.planes.length > 0 ? (
        <span className="text-xs">{planesLabel(nic.planes)}</span>
      ) : (
        <span className="text-xs text-muted-foreground">unused</span>
      ),
  },
  {
    id: "link",
    header: "Link",
    sortValue: (nic) => nic.speed_mbps ?? 0,
    cell: (nic) =>
      nic.state === "up" ? (
        <span className="text-xs tabular-nums">
          {formatLinkSpeed(nic.speed_mbps)}
          {nic.max_mbps && nic.speed_mbps && nic.max_mbps > nic.speed_mbps
            ? ` of ${formatLinkSpeed(nic.max_mbps)}`
            : ""}
        </span>
      ) : (
        <Badge variant="secondary">{nic.state}</Badge>
      ),
  },
  {
    id: "switch",
    header: "Switch port",
    hideBelow: "md",
    sortValue: (nic) => nic.neighbor?.system_name,
    cell: (nic) =>
      nic.neighbor ? (
        <div className="flex flex-col">
          <span className="text-xs">
            {nic.neighbor.system_name || nic.neighbor.chassis_id}
          </span>
          <span className="font-mono text-2xs text-muted-foreground">
            {nic.neighbor.port_id}
            {nic.neighbor.port_description &&
            nic.neighbor.port_description !== nic.neighbor.port_id
              ? ` · ${nic.neighbor.port_description}`
              : ""}
          </span>
        </div>
      ) : (
        <span className="text-xs text-muted-foreground">-</span>
      ),
  },
  {
    id: "driver",
    header: "Driver",
    hideBelow: "lg",
    sortValue: (nic) => nic.driver,
    cell: (nic) => (
      <span className="font-mono text-2xs">
        {[nic.driver, nic.firmware].filter(Boolean).join(" ") || "-"}
      </span>
    ),
  },
]

// Each plane the installer configured, then every physical port and what it
// carries. A switch port is named only where LLDP said so.
export function NodeNetwork({ facts }: { facts: NodeFacts | undefined }) {
  if (!facts) {
    return (
      <PanelSection title="Network">
        <p className="text-xs text-muted-foreground">
          This node did not report its network.
        </p>
      </PanelSection>
    )
  }

  const planes = PLANE_ORDER.flatMap((plane) =>
    (facts.planes ?? []).filter((p) => p.plane === plane),
  )

  const lldp = facts.sources?.lldp === "ok"

  return (
    <>
      <PanelSection title="Planes">
        <PanelFields>
          {planes.map((plane) => (
            <PanelField
              key={plane.plane}
              label={PLANE_LABELS[plane.plane]}
              mono
            >
              {plane.address
                ? `${plane.address}/${plane.prefix ?? "?"}`
                : "no address"}
              {" · "}
              {plane.bridge ?? "-"}
              {plane.nics && plane.nics.length > 0
                ? ` · ${plane.nics.join(", ")}`
                : ""}
              {plane.mtu ? ` · MTU ${plane.mtu}` : ""}
            </PanelField>
          ))}
        </PanelFields>
        {planes.some((p) => p.plane === "vpc" && p.bridge === "br-lan") && (
          <p className="mt-2 text-2xs text-muted-foreground">
            The VPC overlay rides the LAN bridge on this node, so the two planes
            share its ports.
          </p>
        )}
      </PanelSection>
      <PanelSection title="Ports">
        <DataTable
          caption="Physical network ports"
          columns={NIC_COLUMNS}
          empty="No physical ports reported."
          getRowId={(nic) => nic.name}
          rows={facts.nics ?? []}
        />
        {!lldp && (
          <p className="mt-2 text-2xs text-muted-foreground">
            No LLDP neighbours: this node does not run systemd-networkd, or its
            switch does not send LLDP. Ports are shown from their link speed
            alone.
          </p>
        )}
      </PanelSection>
    </>
  )
}

import {
  PanelField,
  PanelFields,
  PanelSection,
} from "@/components/detail-panel"
import { formatSize } from "@/lib/utils"
import type { MemoryGroup, NodeFacts } from "@/queries/nodes"

const FORM_FACTOR_LABELS: Record<NodeFacts["form_factor"], string> = {
  "rack-server": "Rack server",
  tower: "Tower",
  "virtual-machine": "Virtual machine",
  sbc: "Single-board computer",
  jetson: "NVIDIA Jetson",
  unknown: "Unknown",
}

function formatUptime(seconds: number | undefined): string {
  if (!seconds) {
    return "-"
  }

  const days = Math.floor(seconds / 86_400)
  const hours = Math.floor((seconds % 86_400) / 3600)

  return days > 0
    ? `${days}d ${hours}h`
    : `${hours}h ${Math.floor((seconds % 3600) / 60)}m`
}

function memorySpeed(group: MemoryGroup): string {
  if (!group.speed_mts) {
    return ""
  }

  if (group.configured_mts && group.configured_mts !== group.speed_mts) {
    return `${group.configured_mts} MT/s (rated ${group.speed_mts})`
  }

  return `${group.speed_mts} MT/s`
}

function memoryGroupLabel(group: MemoryGroup): string {
  const speed = memorySpeed(group)

  return [
    `${group.count} × ${formatSize(group.size_bytes)}`,
    group.type,
    group.type_detail,
    group.form_factor,
    speed,
  ]
    .filter(Boolean)
    .join(" ")
}

function SystemSection({ facts }: { facts: NodeFacts }) {
  const { system } = facts
  const dmiMissing = facts.sources?.dmi && facts.sources.dmi !== "ok"

  return (
    <PanelSection title="System">
      <PanelFields>
        <PanelField label="Form factor">
          {FORM_FACTOR_LABELS[facts.form_factor]}
          {facts.virt && facts.virt !== "none" && ` (${facts.virt})`}
        </PanelField>
        <PanelField label="Vendor">{system.vendor || "-"}</PanelField>
        <PanelField label="Model">
          {[system.product, system.version].filter(Boolean).join(" ") ||
            system.device_model ||
            "-"}
        </PanelField>
        {system.family && (
          <PanelField label="Family">{system.family}</PanelField>
        )}
        <PanelField copy={system.serial} label="Serial number" mono>
          {system.serial || "-"}
        </PanelField>
        {system.chassis_asset && (
          <PanelField copy={system.chassis_asset} label="Asset tag" mono>
            {system.chassis_asset}
          </PanelField>
        )}
        {system.uuid && (
          <PanelField copy={system.uuid} label="System UUID" mono>
            {system.uuid}
          </PanelField>
        )}
        <PanelField label="Chassis">{system.chassis_type || "-"}</PanelField>
        {system.board_product && (
          <PanelField label="Board">
            {[system.board_vendor, system.board_product]
              .filter(Boolean)
              .join(" ")}
          </PanelField>
        )}
        {system.board_serial && (
          <PanelField copy={system.board_serial} label="Board serial" mono>
            {system.board_serial}
          </PanelField>
        )}
        <PanelField label="Firmware">
          {[system.bios_vendor, system.bios_version, system.bios_date]
            .filter(Boolean)
            .join(" · ") || "-"}
        </PanelField>
      </PanelFields>
      {dmiMissing && (
        <p className="mt-2 text-2xs text-muted-foreground">
          Firmware tables were not readable on this node ({facts.sources?.dmi}
          ).
        </p>
      )}
    </PanelSection>
  )
}

function ProcessorSection({ cpu }: { cpu: NodeFacts["cpu"] }) {
  return (
    <PanelSection title="Processor">
      <PanelFields>
        <PanelField label="Model">{cpu.model || "-"}</PanelField>
        <PanelField label="Topology">
          {cpu.sockets > 0
            ? `${cpu.sockets} socket${cpu.sockets === 1 ? "" : "s"} · ${cpu.cores} cores · ${cpu.threads} threads`
            : `${cpu.threads} threads`}
        </PanelField>
        {cpu.max_mhz ? (
          <PanelField label="Max clock">
            {(cpu.max_mhz / 1000).toFixed(2)} GHz
          </PanelField>
        ) : null}
        <PanelField label="Virtualisation">
          {cpu.virtualization || "none"}
        </PanelField>
        {cpu.features && cpu.features.length > 0 && (
          <PanelField label="Features" mono>
            {cpu.features.join(" ")}
          </PanelField>
        )}
      </PanelFields>
    </PanelSection>
  )
}

function MemorySection({ memory }: { memory: NodeFacts["memory"] }) {
  return (
    <PanelSection title="Memory">
      <PanelFields>
        <PanelField label="Installed">
          {formatSize(
            memory.total_bytes > 0
              ? memory.total_bytes
              : (memory.os_total_bytes ?? 0),
          )}
        </PanelField>
        {memory.slots_total > 0 && (
          <PanelField label="Slots">
            {memory.slots_used} of {memory.slots_total} used
            {memory.max_capacity && ` · up to ${memory.max_capacity}`}
          </PanelField>
        )}
        {memory.ecc && (
          <PanelField label="Error correction">{memory.ecc}</PanelField>
        )}
        {(memory.groups ?? []).map((group) => (
          <PanelField
            info={
              group.serials && group.serials.length > 0 ? (
                <span className="font-mono">{group.serials.join(", ")}</span>
              ) : undefined
            }
            key={memoryGroupLabel(group) + group.part_number}
            label={
              [group.manufacturer, group.part_number]
                .filter(Boolean)
                .join(" ") || "DIMMs"
            }
          >
            {memoryGroupLabel(group)}
            {group.rank && ` · rank ${group.rank}`}
          </PanelField>
        ))}
      </PanelFields>
    </PanelSection>
  )
}

function SoftwareSection({ facts }: { facts: NodeFacts }) {
  const { os } = facts

  return (
    <PanelSection title="Software">
      <PanelFields>
        <PanelField label="Operating system">{os.name || "-"}</PanelField>
        <PanelField label="Kernel" mono>
          {[os.kernel, os.arch].filter(Boolean).join(" ") || "-"}
        </PanelField>
        <PanelField label="Spinifex" mono>
          {os.spx_version || "-"}
        </PanelField>
        <PanelField label="Uptime">
          {formatUptime(os.uptime_seconds)}
        </PanelField>
        {facts.collected_at && (
          <PanelField label="Hardware read at">{facts.collected_at}</PanelField>
        )}
      </PanelFields>
    </PanelSection>
  )
}

// What the node's firmware and kernel report about it. Serials are shown: this
// panel is the super-admin's, and they are what a support case asks for.
export function NodeHardware({ facts }: { facts: NodeFacts | undefined }) {
  if (!facts) {
    return (
      <PanelSection title="Hardware">
        <p className="text-xs text-muted-foreground">
          This node did not report its hardware. It may be running a release
          without the node inventory.
        </p>
      </PanelSection>
    )
  }

  return (
    <>
      <SystemSection facts={facts} />
      <ProcessorSection cpu={facts.cpu} />
      <MemorySection memory={facts.memory} />
      <SoftwareSection facts={facts} />
    </>
  )
}

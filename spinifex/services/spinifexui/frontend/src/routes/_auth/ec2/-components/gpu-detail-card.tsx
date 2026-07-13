import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { formatVRAMMiB } from "@/lib/utils"
import type { VMGPUInfo } from "@/queries/admin"

export function GpuDetailCard({ gpu }: { gpu?: VMGPUInfo }) {
  if (!gpu) {
    return null
  }

  return (
    <DetailCard>
      <DetailCard.Header>GPU</DetailCard.Header>
      <DetailCard.Content>
        <DetailRow label="Model" value={gpu.model} />
        <DetailRow label="VRAM" value={formatVRAMMiB(gpu.vram_mib)} />
        <DetailRow
          label="Attachment"
          value={gpu.profile ? "MIG slice" : "PCIe passthrough"}
        />
        {gpu.profile && <DetailRow label="Profile" value={gpu.profile} />}
        {gpu.mdev_path && <DetailRow label="Mdev path" value={gpu.mdev_path} />}
        {gpu.pci_address && (
          <DetailRow label="PCI address" value={gpu.pci_address} />
        )}
      </DetailCard.Content>
    </DetailCard>
  )
}

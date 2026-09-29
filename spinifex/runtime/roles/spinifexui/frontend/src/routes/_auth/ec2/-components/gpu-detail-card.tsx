import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { formatVRAMMiB } from "@/lib/utils"
import type { VMGPUInfo } from "@/queries/admin"

export function GpuDetailCard({ gpus }: { gpus?: VMGPUInfo[] }) {
  if (!gpus?.length) {
    return null
  }

  return (
    <DetailCard>
      <DetailCard.Header>
        {gpus.length === 1 ? "GPU" : `GPUs (${gpus.length})`}
      </DetailCard.Header>
      {gpus.map((gpu, index) => (
        <div
          className="border-b last:border-b-0"
          key={gpu.mdev_path ?? gpu.pci_address ?? `${gpu.model}-${index}`}
        >
          {gpus.length > 1 && (
            <h3 className="bg-muted/30 px-4 py-2 text-sm font-medium">
              GPU {index + 1}
            </h3>
          )}
          <DetailCard.Content>
            <DetailRow label="Model" value={gpu.model} />
            <DetailRow label="VRAM" value={formatVRAMMiB(gpu.vram_mib)} />
            <DetailRow
              label="Attachment"
              value={gpu.profile ? "MIG slice" : "PCIe passthrough"}
            />
            {gpu.profile && <DetailRow label="Profile" value={gpu.profile} />}
            {gpu.mdev_path && (
              <DetailRow label="Mdev path" value={gpu.mdev_path} />
            )}
            {gpu.pci_address && (
              <DetailRow label="PCI address" value={gpu.pci_address} />
            )}
          </DetailCard.Content>
        </div>
      ))}
    </DetailCard>
  )
}

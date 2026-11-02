import {
  PanelField,
  PanelFields,
  PanelSection,
} from "@/components/detail-panel"
import { formatVRAMMiB } from "@/lib/utils"
import type { VMGPUInfo } from "@/queries/admin"

export function GpuSection({ gpus }: { gpus?: VMGPUInfo[] }) {
  if (!gpus?.length) {
    return null
  }

  return (
    <PanelSection title={gpus.length === 1 ? "GPU" : `GPUs (${gpus.length})`}>
      {gpus.map((gpu, index) => (
        <div
          className="not-first:mt-3"
          key={gpu.mdev_path ?? gpu.pci_address ?? `${gpu.model}-${index}`}
        >
          {gpus.length > 1 && (
            <h3 className="text-xs font-medium text-muted-foreground">
              GPU {index + 1}
            </h3>
          )}
          <PanelFields>
            <PanelField label="Model">{gpu.model}</PanelField>
            <PanelField label="VRAM">{formatVRAMMiB(gpu.vram_mib)}</PanelField>
            <PanelField label="Attachment">
              {gpu.profile ? "MIG slice" : "PCIe passthrough"}
            </PanelField>
            {gpu.profile && (
              <PanelField label="Profile" mono>
                {gpu.profile}
              </PanelField>
            )}
            {gpu.mdev_path && (
              <PanelField label="Mdev path" mono>
                {gpu.mdev_path}
              </PanelField>
            )}
            {gpu.pci_address && (
              <PanelField label="PCI address" mono>
                {gpu.pci_address}
              </PanelField>
            )}
          </PanelFields>
        </div>
      ))}
    </PanelSection>
  )
}

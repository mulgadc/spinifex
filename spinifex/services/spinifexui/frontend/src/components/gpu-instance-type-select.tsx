import type { GpuDeviceInfo, InstanceTypeInfo } from "@aws-sdk/client-ec2"
import { useMemo } from "react"

import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectLabel,
  SelectSeparator,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { formatVRAMMiB } from "@/lib/utils"

export interface GpuInstanceTypeSelectProps {
  instanceTypes: InstanceTypeInfo[]
  value: string
  onValueChange: (value: string) => void
  availabilityByType?: Record<string, number>
  disabled?: boolean
  id?: string
  className?: string
  "aria-invalid"?: boolean
}

interface InstanceTypeEntry {
  name: string
  gpu?: GpuDeviceInfo
}

/** Structural GPU detection: a type is GPU-accelerated iff GpuInfo.Gpus is non-empty. No name heuristics. */
export function isGpuInstanceType(type: InstanceTypeInfo): boolean {
  return (type.GpuInfo?.Gpus?.length ?? 0) > 0
}

function dedupeInstanceTypes(
  instanceTypes: InstanceTypeInfo[],
): InstanceTypeEntry[] {
  const byName = new Map<string, InstanceTypeEntry>()
  for (const type of instanceTypes) {
    const name = type.InstanceType
    if (!name || byName.has(name)) {
      continue
    }
    byName.set(name, { name, gpu: type.GpuInfo?.Gpus?.[0] })
  }
  return [...byName.values()].toSorted((a, b) => a.name.localeCompare(b.name))
}

function InstanceTypeItemLabel({
  entry,
  availability,
}: {
  entry: InstanceTypeEntry
  availability?: number
}) {
  const vramMiB = entry.gpu?.MemoryInfo?.SizeInMiB
  return (
    <>
      {entry.name}
      {vramMiB !== undefined && ` — ${formatVRAMMiB(vramMiB)} GPU`}
      {availability !== undefined && ` (${availability} available)`}
    </>
  )
}

export function GpuInstanceTypeSelect({
  instanceTypes,
  value,
  onValueChange,
  availabilityByType,
  disabled,
  id,
  className,
  "aria-invalid": ariaInvalid,
}: GpuInstanceTypeSelectProps) {
  const { standardEntries, gpuEntries } = useMemo(() => {
    const entries = dedupeInstanceTypes(instanceTypes)
    return {
      standardEntries: entries.filter((e) => !e.gpu),
      gpuEntries: entries.filter((e) => e.gpu),
    }
  }, [instanceTypes])

  const hasBothGroups = standardEntries.length > 0 && gpuEntries.length > 0

  return (
    <Select
      disabled={disabled}
      onValueChange={(next) => onValueChange(next ?? "")}
      value={value}
    >
      <SelectTrigger aria-invalid={ariaInvalid} className={className} id={id}>
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        {standardEntries.length > 0 && (
          <SelectGroup>
            {hasBothGroups && <SelectLabel>Standard</SelectLabel>}
            {standardEntries.map((entry) => (
              <SelectItem key={entry.name} value={entry.name}>
                <InstanceTypeItemLabel
                  availability={availabilityByType?.[entry.name]}
                  entry={entry}
                />
              </SelectItem>
            ))}
          </SelectGroup>
        )}
        {hasBothGroups && <SelectSeparator />}
        {gpuEntries.length > 0 && (
          <SelectGroup>
            {hasBothGroups && <SelectLabel>GPU-Accelerated</SelectLabel>}
            {gpuEntries.map((entry) => (
              <SelectItem key={entry.name} value={entry.name}>
                <InstanceTypeItemLabel
                  availability={availabilityByType?.[entry.name]}
                  entry={entry}
                />
              </SelectItem>
            ))}
          </SelectGroup>
        )}
      </SelectContent>
    </Select>
  )
}

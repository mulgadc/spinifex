import { Link } from "@tanstack/react-router"
import { ChevronDown, ChevronRight } from "lucide-react"
import { Fragment, useState } from "react"

import { Badge } from "@/components/ui/badge"
import { formatVRAMMiB } from "@/lib/utils"
import type { GPUInfo, NodeInfo } from "@/queries/admin"

function gpuUsedTotal(gpu: GPUInfo): string {
  if (gpu.mig_enabled) {
    const slices = gpu.slices ?? []
    const used = slices.filter((s) => s.instance_id).length
    return `${used} / ${slices.length}`
  }
  return `${gpu.instance_id ? 1 : 0} / 1`
}

export function GPUInventoryCard({ nodes }: { nodes: NodeInfo[] }) {
  const [expanded, setExpanded] = useState<Set<string>>(new Set())

  const toggle = (key: string) => {
    setExpanded((prev) => {
      const next = new Set(prev)
      if (next.has(key)) {
        next.delete(key)
      } else {
        next.add(key)
      }
      return next
    })
  }

  const rows = nodes.flatMap((node) =>
    (node.gpus ?? []).map((gpu) => ({ node: node.node, gpu })),
  )

  return (
    <div className="overflow-x-auto">
      <table className="w-full text-xs">
        <thead>
          <tr className="border-b text-left text-muted-foreground">
            <th className="w-6 pb-1">
              <span className="sr-only">Expand</span>
            </th>
            <th className="pr-4 pb-1 font-medium">Node</th>
            <th className="pr-4 pb-1 font-medium">Model</th>
            <th className="pr-4 pb-1 font-medium">VRAM</th>
            <th className="pr-4 pb-1 font-medium">Mode</th>
            <th className="pr-4 pb-1 font-medium">Profile</th>
            <th className="pb-1 font-medium">Used / Total</th>
          </tr>
        </thead>
        <tbody>
          {rows.map(({ node, gpu }) => {
            const key = `${node}:${gpu.pci_address}`
            const slices = gpu.slices ?? []
            const expandable = gpu.mig_enabled && slices.length > 0
            const isExpanded = expandable && expanded.has(key)
            return (
              <Fragment key={key}>
                <tr className="border-b last:border-0">
                  <td className="py-1.5">
                    {expandable && (
                      <button
                        aria-expanded={isExpanded}
                        aria-label={
                          isExpanded ? "Collapse slices" : "Expand slices"
                        }
                        onClick={() => toggle(key)}
                        type="button"
                      >
                        {isExpanded ? (
                          <ChevronDown className="size-3.5" />
                        ) : (
                          <ChevronRight className="size-3.5" />
                        )}
                      </button>
                    )}
                  </td>
                  <td className="py-1.5 pr-4 font-mono font-medium">{node}</td>
                  <td className="py-1.5 pr-4">{gpu.model}</td>
                  <td className="py-1.5 pr-4">{formatVRAMMiB(gpu.vram_mib)}</td>
                  <td className="py-1.5 pr-4">
                    <Badge className="text-[0.625rem]" variant="secondary">
                      {gpu.mig_enabled ? "MIG" : "Passthrough"}
                    </Badge>
                  </td>
                  <td className="py-1.5 pr-4 font-mono">
                    {gpu.mig_profile ?? "—"}
                  </td>
                  <td className="py-1.5 font-mono">{gpuUsedTotal(gpu)}</td>
                </tr>
                {isExpanded &&
                  slices.map((slice) => (
                    <tr
                      className="border-b text-muted-foreground last:border-0"
                      key={slice.gi_id}
                    >
                      <td className="py-1">
                        <span className="sr-only">Slice</span>
                      </td>
                      <td className="py-1 pr-4 font-mono" colSpan={2}>
                        ↳ slice {slice.gi_id}
                      </td>
                      <td className="py-1 pr-4">
                        {formatVRAMMiB(slice.vram_mib)}
                      </td>
                      <td className="py-1 pr-4">
                        <span className="sr-only">Mode</span>
                      </td>
                      <td className="py-1 pr-4 font-mono">{slice.profile}</td>
                      <td className="py-1 font-mono">
                        {slice.instance_id ? (
                          <Link
                            className="text-primary hover:underline"
                            params={{ id: slice.instance_id }}
                            to="/ec2/describe-instances/$id"
                          >
                            {slice.instance_id}
                          </Link>
                        ) : (
                          <span className="text-green-600">free</span>
                        )}
                      </td>
                    </tr>
                  ))}
              </Fragment>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}

import type { Subnet } from "@aws-sdk/client-ec2"
import { useState } from "react"

import { EditorShell } from "@/components/editor-shell"
import { getNameTag, sameSet } from "@/lib/utils"

interface SubnetsEditorProps {
  // Subnets in the load balancer's VPC, selectable for attachment.
  available: Subnet[]
  // Currently attached subnet IDs.
  current: string[]
  onSubmit: (subnetIds: string[]) => void
  isPending: boolean
  error?: Error | null
  isSuccess?: boolean
}

function subnetLabel(subnet: Subnet): string {
  const name = getNameTag(subnet.Tags)
  const az = subnet.AvailabilityZone ? ` · ${subnet.AvailabilityZone}` : ""
  const cidr = subnet.CidrBlock ? ` (${subnet.CidrBlock})` : ""

  return `${subnet.SubnetId}${name ? ` — ${name}` : ""}${cidr}${az}`
}

export function SubnetsEditor({
  available,
  current,
  onSubmit,
  isPending,
  error,
  isSuccess,
}: SubnetsEditorProps) {
  const initial = [...current]
  const [selected, setSelected] = useState<string[]>(initial)

  const toggle = (id: string) => {
    setSelected((prev) =>
      prev.includes(id) ? prev.filter((x) => x !== id) : [...prev, id],
    )
  }

  const handleReset = () => {
    setSelected(initial)
  }

  const dirty = !sameSet(selected, initial)
  // A load balancer must front at least one subnet.
  const isEmpty = selected.length === 0
  const selectedIds = new Set(selected)

  return (
    <EditorShell
      dirty={dirty}
      error={error}
      errorMsg="Failed to update subnets"
      invalid={isEmpty}
      isPending={isPending}
      isSuccess={isSuccess}
      notice={
        <div
          className="mb-4 rounded-md border border-warning bg-warning/10 p-3 text-sm text-warning-foreground"
          role="note"
        >
          Applying a subnet change re-homes the load balancer by relaunching its
          underlying system VM. Expect a brief data-plane interruption while it
          restarts.
        </div>
      }
      onReset={handleReset}
      onSave={() => {
        onSubmit(selected)
      }}
      successMsg="Subnets updated."
    >
      <div className="flex flex-col gap-1 p-4">
        {available.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            No subnets in this VPC.
          </p>
        ) : (
          available.map((subnet) => (
            <label
              className="flex items-center gap-2 text-sm"
              key={subnet.SubnetId}
            >
              <input
                aria-label={`Subnet ${subnet.SubnetId}`}
                checked={selectedIds.has(subnet.SubnetId ?? "")}
                onChange={() => {
                  toggle(subnet.SubnetId ?? "")
                }}
                type="checkbox"
              />
              <span className="font-mono">{subnetLabel(subnet)}</span>
            </label>
          ))
        )}
        {isEmpty && (
          <p className="text-xs text-destructive">
            At least one subnet is required.
          </p>
        )}
      </div>
    </EditorShell>
  )
}

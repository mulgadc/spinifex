import type { Subnet } from "@aws-sdk/client-ec2"
import { useState } from "react"

import { EditorShell } from "@/components/editor-shell"
import { SwitchList, type SwitchListItem } from "@/components/switch-list"
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

// Rows for a subnet SwitchList, ordered by zone, with the name, zone and CIDR
// beneath each ID.
export function subnetSwitchItems(subnets: Subnet[]): SwitchListItem[] {
  return subnets
    .toSorted(
      (a, b) =>
        (a.AvailabilityZone ?? "").localeCompare(b.AvailabilityZone ?? "") ||
        (a.SubnetId ?? "").localeCompare(b.SubnetId ?? ""),
    )
    .map((subnet) => ({
      id: subnet.SubnetId ?? "",
      label: subnet.SubnetId ?? "",
      detail: [
        getNameTag(subnet.Tags),
        subnet.AvailabilityZone,
        subnet.CidrBlock,
      ]
        .filter(Boolean)
        .join(" · "),
    }))
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

  const handleReset = () => {
    setSelected(initial)
  }

  const dirty = !sameSet(selected, initial)
  // A load balancer must front at least one subnet.
  const isEmpty = selected.length === 0

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
      <div className="flex flex-col gap-2 p-4">
        <SwitchList
          emptyText="No subnets in this VPC."
          itemLabel="Subnet"
          items={subnetSwitchItems(available)}
          onChange={setSelected}
          selected={selected}
        />
        {isEmpty && (
          <p className="text-xs text-destructive">
            At least one subnet is required.
          </p>
        )}
      </div>
    </EditorShell>
  )
}

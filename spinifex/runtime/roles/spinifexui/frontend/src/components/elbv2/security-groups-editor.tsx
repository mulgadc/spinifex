import type { SecurityGroup } from "@aws-sdk/client-ec2"
import { useState } from "react"

import { EditorShell } from "@/components/editor-shell"
import { SecurityGroupCheckboxes } from "@/components/security-group-checkboxes"
import { sameSet } from "@/lib/utils"

interface SecurityGroupsEditorProps {
  // Security groups in the load balancer's VPC, selectable for attachment.
  available: SecurityGroup[]
  // Currently attached security group IDs.
  current: string[]
  onSubmit: (securityGroupIds: string[]) => void
  isPending: boolean
  error?: Error | null
  isSuccess?: boolean
}

export function SecurityGroupsEditor({
  available,
  current,
  onSubmit,
  isPending,
  error,
  isSuccess,
}: SecurityGroupsEditorProps) {
  const initial = [...current]
  const [selected, setSelected] = useState<string[]>(initial)

  const handleReset = () => {
    setSelected(initial)
  }

  const dirty = !sameSet(selected, initial)
  // AWS requires at least one security group on an ALB.
  const isEmpty = selected.length === 0

  return (
    <EditorShell
      dirty={dirty}
      error={error}
      errorMsg="Failed to update security groups"
      invalid={isEmpty}
      isPending={isPending}
      isSuccess={isSuccess}
      onReset={handleReset}
      onSave={() => {
        onSubmit(selected)
      }}
      successMsg="Security groups updated."
    >
      <div className="flex flex-col gap-1 p-4">
        <SecurityGroupCheckboxes
          emptyText="No security groups in this VPC."
          groups={available}
          onChange={setSelected}
          selected={selected}
        />
        {isEmpty && (
          <p className="text-xs text-destructive">
            At least one security group is required.
          </p>
        )}
      </div>
    </EditorShell>
  )
}

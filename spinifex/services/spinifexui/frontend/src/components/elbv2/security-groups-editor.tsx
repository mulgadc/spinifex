import type { SecurityGroup } from "@aws-sdk/client-ec2"
import { useMemo, useState } from "react"

import { Button } from "@/components/ui/button"

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

function sameSet(a: string[], b: string[]): boolean {
  if (a.length !== b.length) {
    return false
  }
  const setB = new Set(b)
  return a.every((id) => setB.has(id))
}

export function SecurityGroupsEditor({
  available,
  current,
  onSubmit,
  isPending,
  error,
  isSuccess,
}: SecurityGroupsEditorProps) {
  const initial = useMemo(() => [...current], [current])
  const [selected, setSelected] = useState<string[]>(initial)

  const toggle = (id: string) => {
    setSelected((prev) =>
      prev.includes(id) ? prev.filter((x) => x !== id) : [...prev, id],
    )
  }
  const handleReset = () => setSelected(initial)

  const dirty = !sameSet(selected, initial)
  // AWS requires at least one security group on an ALB.
  const isEmpty = selected.length === 0

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault()
        if (!dirty || isEmpty) {
          return
        }
        onSubmit(selected)
      }}
    >
      {error && (
        <div
          className="mb-4 rounded-md border border-destructive bg-destructive/10 p-3 text-sm text-destructive"
          role="alert"
        >
          {error.message || "Failed to update security groups"}
        </div>
      )}
      {isSuccess && !dirty && (
        <div
          className="mb-4 rounded-md border border-emerald-500 bg-emerald-500/10 p-3 text-sm text-emerald-700 dark:text-emerald-400"
          role="status"
        >
          Security groups updated.
        </div>
      )}
      <div className="rounded-lg border bg-card">
        <div className="flex flex-col gap-1 p-4">
          {available.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No security groups in this VPC.
            </p>
          ) : (
            available.map((sg) => (
              <label
                className="flex items-center gap-2 text-sm"
                key={sg.GroupId}
              >
                <input
                  aria-label={`Security group ${sg.GroupId} (${sg.GroupName})`}
                  checked={selected.includes(sg.GroupId ?? "")}
                  onChange={() => toggle(sg.GroupId ?? "")}
                  type="checkbox"
                />
                <span className="font-mono">
                  {sg.GroupId} ({sg.GroupName})
                </span>
              </label>
            ))
          )}
          {isEmpty && (
            <p className="text-xs text-destructive">
              At least one security group is required.
            </p>
          )}
        </div>
        <div className="flex items-center justify-end gap-2 border-t p-4">
          <span className="mr-auto text-xs text-muted-foreground">
            {dirty ? "Unsaved changes" : "No changes"}
          </span>
          <Button
            disabled={isPending || !dirty}
            onClick={handleReset}
            size="sm"
            type="button"
            variant="secondary"
          >
            Reset
          </Button>
          <Button
            disabled={isPending || !dirty || isEmpty}
            size="sm"
            type="submit"
          >
            {isPending ? "Saving…" : "Save changes"}
          </Button>
        </div>
      </div>
    </form>
  )
}

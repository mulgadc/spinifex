import type { Subnet } from "@aws-sdk/client-ec2"
import { useMemo, useState } from "react"

import { Button } from "@/components/ui/button"
import { getNameTag } from "@/lib/utils"

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

function sameSet(a: string[], b: string[]): boolean {
  if (a.length !== b.length) {
    return false
  }
  const setB = new Set(b)
  return a.every((id) => setB.has(id))
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
  const initial = useMemo(() => [...current], [current])
  const [selected, setSelected] = useState<string[]>(initial)

  const toggle = (id: string) => {
    setSelected((prev) =>
      prev.includes(id) ? prev.filter((x) => x !== id) : [...prev, id],
    )
  }
  const handleReset = () => setSelected(initial)

  const dirty = !sameSet(selected, initial)
  // A load balancer must front at least one subnet.
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
      <div
        className="mb-4 rounded-md border border-amber-500 bg-amber-500/10 p-3 text-sm text-amber-700 dark:text-amber-400"
        role="note"
      >
        Applying a subnet change re-homes the load balancer by relaunching its
        underlying system VM. Expect a brief data-plane interruption while it
        restarts.
      </div>
      {error && (
        <div
          className="mb-4 rounded-md border border-destructive bg-destructive/10 p-3 text-sm text-destructive"
          role="alert"
        >
          {error.message || "Failed to update subnets"}
        </div>
      )}
      {isSuccess && !dirty && (
        <div
          className="mb-4 rounded-md border border-emerald-500 bg-emerald-500/10 p-3 text-sm text-emerald-700 dark:text-emerald-400"
          role="status"
        >
          Subnets updated.
        </div>
      )}
      <div className="rounded-lg border bg-card">
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
                  checked={selected.includes(subnet.SubnetId ?? "")}
                  onChange={() => toggle(subnet.SubnetId ?? "")}
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

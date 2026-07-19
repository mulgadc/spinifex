import type { Instance } from "@aws-sdk/client-ec2"
import { useState } from "react"

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Input } from "@/components/ui/input"
import { getNameTag } from "@/lib/utils"
import type { TargetInput } from "@/mutations/elbv2"

interface SelectionState {
  checked: boolean
  port: string
}

interface RegisterTargetsDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  instances: Instance[]
  defaultPort: number
  isPending: boolean
  onConfirm: (targets: TargetInput[]) => void
}

function parsePort(raw: string): number | undefined {
  const trimmed = raw.trim()
  if (trimmed === "") {
    return undefined
  }
  const n = Number(trimmed)
  if (!Number.isInteger(n) || n < 1 || n > 65_535) {
    return undefined
  }
  return n
}

export function RegisterTargetsDialog({
  open,
  onOpenChange,
  instances,
  defaultPort,
  isPending,
  onConfirm,
}: RegisterTargetsDialogProps) {
  const [selections, setSelections] = useState<Record<string, SelectionState>>(
    {},
  )
  const [error, setError] = useState<string | undefined>()

  // Clear the selection each time the dialog closes so it reopens fresh.
  const [wasOpen, setWasOpen] = useState(open)
  if (open !== wasOpen) {
    setWasOpen(open)
    if (!open) {
      setSelections({})
      setError(undefined)
    }
  }

  const toggle = (id: string) => {
    setSelections((prev) => {
      const existing = prev[id]
      if (existing?.checked) {
        return { ...prev, [id]: { ...existing, checked: false } }
      }
      return {
        ...prev,
        [id]: { checked: true, port: existing?.port ?? "" },
      }
    })
  }

  const setPort = (id: string, port: string) => {
    setSelections((prev) => ({
      ...prev,
      [id]: { checked: prev[id]?.checked ?? true, port },
    }))
  }

  const handleConfirm = () => {
    const selected = Object.entries(selections).filter(
      ([, v]) => v.checked && v !== undefined,
    )
    if (selected.length === 0) {
      setError("Select at least one instance.")
      return
    }
    const targets: TargetInput[] = []
    for (const [id, v] of selected) {
      if (v.port.trim() === "") {
        targets.push({ id })
        continue
      }
      const parsed = parsePort(v.port)
      if (parsed === undefined) {
        setError(`Port for ${id} must be an integer between 1 and 65535.`)
        return
      }
      targets.push({ id, port: parsed })
    }
    setError(undefined)
    onConfirm(targets)
  }

  return (
    <AlertDialog onOpenChange={onOpenChange} open={open}>
      <AlertDialogContent className="sm:max-w-lg">
        <AlertDialogHeader>
          <AlertDialogTitle>Register targets</AlertDialogTitle>
          <AlertDialogDescription>
            Select instances to register. Leave port blank to use the target
            group port ({defaultPort}).
          </AlertDialogDescription>
        </AlertDialogHeader>

        {instances.length === 0 ? (
          <p className="text-xs text-muted-foreground">
            No instances available in this VPC.
          </p>
        ) : (
          <div className="max-h-72 space-y-1 overflow-y-auto">
            {instances.map((inst) => {
              const id = inst.InstanceId ?? ""
              const selection = selections[id]
              const checked = selection?.checked ?? false
              const name = getNameTag(inst.Tags)
              return (
                <div
                  className="flex items-center gap-2 rounded px-1 py-1 text-xs hover:bg-accent"
                  key={id}
                >
                  <input
                    aria-label={`Register target ${id}`}
                    checked={checked}
                    id={`register-target-${id}`}
                    onChange={() => toggle(id)}
                    type="checkbox"
                  />
                  <label
                    className="flex flex-1 items-center gap-2"
                    htmlFor={`register-target-${id}`}
                  >
                    <span className="font-mono">{id}</span>
                    {name && (
                      <span className="text-muted-foreground">({name})</span>
                    )}
                    <span className="text-muted-foreground">
                      {inst.State?.Name ?? ""}
                    </span>
                  </label>
                  {checked && (
                    <Input
                      aria-label={`Port override for ${id}`}
                      className="w-24"
                      onChange={(e) => setPort(id, e.target.value)}
                      placeholder={String(defaultPort)}
                      value={selection?.port ?? ""}
                    />
                  )}
                </div>
              )
            })}
          </div>
        )}

        {error && (
          <p className="text-xs text-destructive" role="alert">
            {error}
          </p>
        )}

        <AlertDialogFooter>
          <AlertDialogCancel disabled={isPending}>Cancel</AlertDialogCancel>
          <AlertDialogAction disabled={isPending} onClick={handleConfirm}>
            {isPending ? "Registering\u2026" : "Register"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

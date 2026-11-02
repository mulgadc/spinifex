import type { Instance } from "@aws-sdk/client-ec2"
import { type SubmitEvent, useState } from "react"

import {
  type CliCommand,
  CliCommandPanel,
  cliPlaceholder,
  commandFlag,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { FormPanel } from "@/components/form-panel"
import { SwitchList } from "@/components/switch-list"
import { Field, FieldDescription, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { getNameTag } from "@/lib/utils"
import type { TargetInput } from "@/mutations/elbv2"

interface RegisterTargetsFormProps {
  targetGroupArn: string
  instances: Instance[]
  defaultPort: number
  isPending: boolean
  error: Error | null
  onSubmit: (targets: TargetInput[]) => Promise<void>
  onClose: () => void
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

export function RegisterTargetsForm({
  targetGroupArn,
  instances,
  defaultPort,
  isPending,
  error,
  onSubmit,
  onClose,
}: RegisterTargetsFormProps) {
  const [selected, setSelected] = useState<string[]>([])
  const [ports, setPorts] = useState<Record<string, string>>({})
  const [validation, setValidation] = useState<string | undefined>()

  const handleSubmit = (event: SubmitEvent<HTMLFormElement>) => {
    event.preventDefault()
    event.stopPropagation()

    if (selected.length === 0) {
      setValidation("Select at least one instance.")

      return
    }

    const targets: TargetInput[] = []

    for (const id of selected) {
      const raw = ports[id] ?? ""

      if (raw.trim() === "") {
        targets.push({ id })
        continue
      }

      const parsed = parsePort(raw)

      if (parsed === undefined) {
        setValidation(`Port for ${id} must be an integer between 1 and 65535.`)

        return
      }

      targets.push({ id, port: parsed })
    }

    setValidation(undefined)
    void onSubmit(targets)
  }

  return (
    <FormPanel onClose={onClose} title="Register targets">
      <form onSubmit={handleSubmit}>
        <FormLock className="space-y-4" locked={isPending}>
          <FieldDescription>
            Select instances to register. Leave port blank to use the target
            group port ({defaultPort}).
          </FieldDescription>

          <SwitchList
            emptyText="No instances available in this VPC."
            itemLabel="Register target"
            items={instances.map((inst) => {
              const name = getNameTag(inst.Tags)
              const state = inst.State?.Name ?? ""

              return {
                id: inst.InstanceId ?? "",
                label: inst.InstanceId ?? "",
                detail: [name && `(${name})`, state].filter(Boolean).join(" "),
              }
            })}
            onChange={setSelected}
            selected={selected}
          />

          {selected.map((id) => (
            <Field key={id}>
              <FieldTitle>
                <label htmlFor={`register-port-${id}`}>Port for {id}</label>
              </FieldTitle>
              <Input
                aria-label={`Port override for ${id}`}
                className="w-32"
                id={`register-port-${id}`}
                inputMode="numeric"
                onChange={(e) => {
                  setPorts((prev) => ({ ...prev, [id]: e.target.value }))
                }}
                placeholder={String(defaultPort)}
                value={ports[id] ?? ""}
              />
            </Field>
          ))}

          {validation && (
            <p className="text-xs text-destructive" role="alert">
              {validation}
            </p>
          )}

          <CliCommandPanel
            commands={buildRegisterTargetsCommands(
              targetGroupArn,
              selected,
              ports,
            )}
          />
          <FormActions
            error={error}
            errorMsg="Failed to register targets"
            isPending={isPending}
            isSubmitting={false}
            onCancel={onClose}
            pendingLabel="Registering…"
            submitLabel="Register"
          />
        </FormLock>
      </form>
    </FormPanel>
  )
}

export function buildRegisterTargetsCommands(
  targetGroupArn: string,
  selected: string[],
  ports: Record<string, string>,
): CliCommand[] {
  const targets =
    selected.length === 0
      ? [`Id=${cliPlaceholder("", "InstanceId")}`]
      : selected.map((id) => {
          const port = parsePort(ports[id] ?? "")

          return port === undefined ? `Id=${id}` : `Id=${id},Port=${port}`
        })

  return [
    {
      label: "Register targets",
      parts: [
        {
          type: "bin",
          value: "AWS_PROFILE=spinifex aws elbv2 register-targets",
        },
        ...commandFlag("--target-group-arn", targetGroupArn),
        ...commandFlag("--targets", targets.join(" ")),
      ],
    },
  ]
}

import type { Instance } from "@aws-sdk/client-ec2"
import { useQuery } from "@tanstack/react-query"
import { useState } from "react"

import {
  CliCommandPanel,
  type CliCommand,
  cliPlaceholder,
  commandFlag,
  optionalFlag,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { FormPanel } from "@/components/form-panel"
import { Field, FieldDescription, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { getNameTag } from "@/lib/utils"
import { useAttachVolume } from "@/mutations/ec2"
import { ec2InstancesQueryOptions } from "@/queries/ec2"

// AWS attaches only to a running or stopped instance in the volume's zone.
function isAttachTarget(instance: Instance, zone: string | undefined) {
  const state = instance.State?.Name
  const inZone = !zone || instance.Placement?.AvailabilityZone === zone

  return (state === "running" || state === "stopped") && inZone
}

function buildAttachCommands(
  volumeId: string,
  instanceId: string,
  device: string,
): CliCommand[] {
  return [
    {
      label: "Attach Volume",
      parts: [
        { type: "bin", value: "AWS_PROFILE=spinifex aws ec2 attach-volume" },
        ...commandFlag("\\\n  --volume-id", volumeId),
        ...commandFlag(
          "\\\n  --instance-id",
          cliPlaceholder(instanceId, "InstanceId"),
        ),
        ...optionalFlag("\\\n  --device", device),
      ],
    },
  ]
}

export function AttachVolumeForm({
  volumeId,
  zone,
  onClose,
}: {
  volumeId: string
  zone: string | undefined
  onClose: () => void
}) {
  const { data } = useQuery(ec2InstancesQueryOptions)
  const attach = useAttachVolume()
  const [instanceId, setInstanceId] = useState("")
  const [device, setDevice] = useState("")

  const targets =
    data?.Reservations?.flatMap(
      (r) => r.Instances?.filter((i) => isAttachTarget(i, zone)) ?? [],
    ) ?? []

  const where = zone ? ` in ${zone}` : ""

  async function onSubmit() {
    try {
      await attach.mutateAsync({
        volumeId,
        instanceId,
        device: device || undefined,
      })
    } catch {
      // Shown beside the submit button through the mutation's error.
      return
    }

    onClose()
  }

  return (
    <FormPanel onClose={onClose} subtitle={volumeId} title="Attach volume">
      <form
        onSubmit={(event) => {
          event.preventDefault()
          void onSubmit()
        }}
      >
        <FormLock className="space-y-6" locked={attach.isPending}>
          <Field>
            <FieldTitle>
              <label htmlFor="attachInstance">Instance</label>
            </FieldTitle>
            <Select
              onValueChange={(value) => {
                setInstanceId(value ?? "")
              }}
              value={instanceId}
            >
              <SelectTrigger className="w-full" id="attachInstance">
                <SelectValue
                  placeholder={
                    targets.length
                      ? "Select an instance"
                      : `No running or stopped instances${where}.`
                  }
                />
              </SelectTrigger>
              <SelectContent>
                {targets.map((instance) => {
                  const name = getNameTag(instance.Tags)

                  return (
                    <SelectItem
                      key={instance.InstanceId}
                      value={instance.InstanceId ?? ""}
                    >
                      {instance.InstanceId}
                      {name ? ` (${name})` : ""}
                    </SelectItem>
                  )
                })}
              </SelectContent>
            </Select>
            <FieldDescription>
              A running or stopped instance{where}.
            </FieldDescription>
          </Field>
          <Field>
            <FieldTitle>
              <label htmlFor="attachDevice">Device (optional)</label>
            </FieldTitle>
            <Input
              id="attachDevice"
              onChange={(e) => {
                setDevice(e.target.value)
              }}
              placeholder="/dev/sdf"
              value={device}
            />
          </Field>
          <CliCommandPanel
            commands={buildAttachCommands(volumeId, instanceId, device)}
          />

          <FormActions
            error={attach.error}
            errorMsg="Failed to attach volume"
            isPending={attach.isPending}
            isSubmitting={false}
            onCancel={onClose}
            pendingLabel="Attaching…"
            submitDisabled={!instanceId}
            submitLabel="Attach"
          />
        </FormLock>
      </form>
    </FormPanel>
  )
}

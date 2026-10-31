import type { Instance } from "@aws-sdk/client-ec2"
import { useQuery } from "@tanstack/react-query"
import { useState } from "react"

import {
  CliCommandPanel,
  type CliCommand,
  cliPlaceholder,
  commandFlag,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { FormPanel } from "@/components/form-panel"
import { GpuInstanceTypeSelect } from "@/components/gpu-instance-type-select"
import { Field, FieldDescription, FieldTitle } from "@/components/ui/field"
import { useModifyInstanceAttribute } from "@/mutations/ec2"
import { ec2InstanceTypesQueryOptions } from "@/queries/ec2"

function buildChangeTypeCommands(
  instanceId: string,
  instanceType: string,
): CliCommand[] {
  return [
    {
      label: "Modify Instance Attribute",
      parts: [
        {
          type: "bin",
          value: "AWS_PROFILE=spinifex aws ec2 modify-instance-attribute",
        },
        ...commandFlag("\\\n  --instance-id", instanceId),
        ...commandFlag(
          "\\\n  --instance-type",
          `Value=${cliPlaceholder(instanceType, "InstanceType")}`,
        ),
      ],
    },
  ]
}

export function ChangeInstanceTypeForm({
  instance,
  onClose,
}: {
  instance: Instance
  onClose: () => void
}) {
  const { data } = useQuery(ec2InstanceTypesQueryOptions)
  const modify = useModifyInstanceAttribute()
  const [selected, setSelected] = useState(instance.InstanceType ?? "")
  const instanceId = instance.InstanceId ?? ""

  async function onSubmit() {
    try {
      await modify.mutateAsync({ instanceId, instanceType: selected })
    } catch {
      // Shown beside the submit button through the mutation's error.
      return
    }

    onClose()
  }

  return (
    <FormPanel
      onClose={onClose}
      subtitle={instanceId}
      title="Change instance type"
    >
      <form
        onSubmit={(event) => {
          event.preventDefault()
          void onSubmit()
        }}
      >
        <FormLock className="space-y-6" locked={modify.isPending}>
          <Field>
            <FieldTitle>
              <label htmlFor="instanceType">Instance type</label>
            </FieldTitle>
            <GpuInstanceTypeSelect
              className="w-full"
              id="instanceType"
              instanceTypes={data?.InstanceTypes ?? []}
              onValueChange={setSelected}
              value={selected}
            />
            <FieldDescription>The instance must be stopped.</FieldDescription>
          </Field>

          <CliCommandPanel
            commands={buildChangeTypeCommands(instanceId, selected)}
          />

          <FormActions
            error={modify.error}
            errorMsg="Failed to change the instance type"
            isPending={modify.isPending}
            isSubmitting={false}
            onCancel={onClose}
            pendingLabel="Saving…"
            submitDisabled={!selected || selected === instance.InstanceType}
            submitLabel="Save"
          />
        </FormLock>
      </form>
    </FormPanel>
  )
}

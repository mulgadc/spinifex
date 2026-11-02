import { zodResolver } from "@hookform/resolvers/zod"
import { useForm, useWatch } from "react-hook-form"

import {
  CliCommandPanel,
  type CliCommand,
  cliPlaceholder,
  commandFlag,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { FormPanel } from "@/components/form-panel"
import {
  Field,
  FieldDescription,
  FieldError,
  FieldTitle,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { useModifyVolume } from "@/mutations/ec2"
import { type ModifyVolumeFormData, modifyVolumeSchema } from "@/types/ec2"

function buildModifyCommands(volumeId: string, size: number): CliCommand[] {
  return [
    {
      label: "Modify Volume",
      parts: [
        { type: "bin", value: "AWS_PROFILE=spinifex aws ec2 modify-volume" },
        ...commandFlag("\\\n  --volume-id", volumeId),
        ...commandFlag(
          "\\\n  --size",
          cliPlaceholder(Number.isFinite(size) ? String(size) : "", "Size"),
        ),
      ],
    },
  ]
}

export function ModifyVolumeForm({
  volumeId,
  currentSize,
  onClose,
}: {
  volumeId: string
  currentSize: number
  onClose: () => void
}) {
  const modify = useModifyVolume()

  const {
    control,
    handleSubmit,
    register,
    formState: { errors, isSubmitting },
  } = useForm({
    resolver: zodResolver(
      modifyVolumeSchema.refine((values) => values.size > currentSize, {
        message: `New size must be greater than current size (${currentSize} GiB)`,
        path: ["size"],
      }),
    ),
    defaultValues: { size: currentSize },
  })

  const size = useWatch({ control, name: "size" })

  const onSubmit = async (formData: ModifyVolumeFormData) => {
    try {
      await modify.mutateAsync({ ...formData, volumeId })
    } catch {
      // Shown beside the submit button through the mutation's error.
      return
    }

    onClose()
  }

  return (
    <FormPanel onClose={onClose} subtitle={volumeId} title="Resize volume">
      <form onSubmit={handleSubmit(onSubmit)}>
        <FormLock className="space-y-6" locked={modify.isPending}>
          <Field>
            <FieldTitle>
              <label htmlFor="size">New size (GiB)</label>
            </FieldTitle>
            <Input
              aria-invalid={!!errors.size}
              id="size"
              min={currentSize + 1}
              type="number"
              {...register("size", { valueAsNumber: true })}
            />
            <FieldDescription>Current size: {currentSize} GiB</FieldDescription>
            <FieldError errors={[errors.size]} />
          </Field>

          <CliCommandPanel commands={buildModifyCommands(volumeId, size)} />

          <FormActions
            error={modify.error}
            errorMsg="Failed to resize volume"
            isPending={modify.isPending}
            isSubmitting={isSubmitting}
            onCancel={onClose}
            pendingLabel="Resizing…"
            submitLabel="Resize volume"
          />
        </FormLock>
      </form>
    </FormPanel>
  )
}

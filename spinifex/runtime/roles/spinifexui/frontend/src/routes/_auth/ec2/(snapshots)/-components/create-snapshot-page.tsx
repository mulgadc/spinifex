import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import {
  Controller,
  type DeepPartialSkipArrayKey,
  useForm,
  useWatch,
} from "react-hook-form"

import {
  CliCommandPanel,
  type CliCommand,
  cliPlaceholder,
  commandFlag,
  optionalFlag,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Textarea } from "@/components/ui/textarea"
import { useCreateSnapshot } from "@/mutations/ec2"
import { ec2VolumesQueryOptions } from "@/queries/ec2"
import { type CreateSnapshotFormData, createSnapshotSchema } from "@/types/ec2"

interface CreateSnapshotPageProps {
  // Called with the new resource's ID once created, or with nothing on cancel.
  onClose: (id?: string) => void
  // The volume to snapshot, when the form is opened from one.
  volumeId?: string
}

export function CreateSnapshotPage({
  onClose,
  volumeId: searchVolumeId,
}: CreateSnapshotPageProps) {
  const { data: volumesData } = useSuspenseQuery(ec2VolumesQueryOptions)
  const createMutation = useCreateSnapshot()
  const volumes = volumesData.Volumes ?? []

  const defaultVolumeId = searchVolumeId || (volumes[0]?.VolumeId ?? "")

  const {
    control,
    handleSubmit,
    register,
    formState: { errors, isSubmitting },
  } = useForm<CreateSnapshotFormData>({
    resolver: zodResolver(createSnapshotSchema),
    defaultValues: {
      volumeId: defaultVolumeId,
      description: "",
    },
  })

  const values = useWatch({ control })

  const onSubmit = async (data: CreateSnapshotFormData) => {
    let result

    try {
      result = await createMutation.mutateAsync(data)
    } catch {
      // Surfaced beside the submit button through the mutation's error.
      return
    }

    onClose(result.SnapshotId)
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={createMutation.isPending || isSubmitting}
      >
        <Field>
          <FieldTitle>
            <label htmlFor="volumeId">Volume</label>
          </FieldTitle>
          <Controller
            control={control}
            name="volumeId"
            render={({ field }) => (
              <Select onValueChange={field.onChange} value={field.value ?? ""}>
                <SelectTrigger
                  aria-invalid={!!errors.volumeId}
                  className="w-full"
                  id="volumeId"
                >
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {volumes.map((volume) => (
                    <SelectItem
                      key={volume.VolumeId}
                      value={volume.VolumeId ?? ""}
                    >
                      {volume.VolumeId} ({volume.Size} GiB)
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
          />
          <FieldError errors={[errors.volumeId]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="description">Description (optional)</label>
          </FieldTitle>
          <Textarea
            id="description"
            placeholder="Snapshot description"
            {...register("description")}
          />
        </Field>

        <CliCommandPanel commands={buildCreateSnapshotCommands(values)} />

        <FormActions
          error={createMutation.error}
          errorMsg="Failed to create snapshot"
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            onClose()
          }}
          pendingLabel="Creating…"
          submitLabel="Create Snapshot"
        />
      </FormLock>
    </form>
  )
}

function buildCreateSnapshotCommands(
  values: DeepPartialSkipArrayKey<CreateSnapshotFormData>,
): CliCommand[] {
  const description = values.description ?? ""

  return [
    {
      label: "Create Snapshot",
      parts: [
        { type: "bin", value: "AWS_PROFILE=spinifex aws ec2 create-snapshot" },
        ...commandFlag(
          "\\\n  --volume-id",
          cliPlaceholder(values.volumeId ?? "", "VolumeId"),
        ),
        ...optionalFlag(
          "\\\n  --description",
          description && `"${description}"`,
        ),
      ],
    },
  ]
}

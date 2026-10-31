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
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { useCreateVolume } from "@/mutations/ec2"
import { ec2AvailabilityZonesQueryOptions } from "@/queries/ec2"
import { type CreateVolumeFormData, createVolumeSchema } from "@/types/ec2"

interface CreateVolumePageProps {
  // Called with the new resource's ID once created, or with nothing on cancel.
  onClose: (id?: string) => void
}

export function CreateVolumePage({ onClose }: CreateVolumePageProps) {
  const { data: azData } = useSuspenseQuery(ec2AvailabilityZonesQueryOptions)
  const createMutation = useCreateVolume()
  const availabilityZones = azData.AvailabilityZones ?? []

  const defaultAz = availabilityZones[0]?.ZoneName ?? ""

  const {
    control,
    handleSubmit,
    register,
    formState: { errors, isSubmitting },
  } = useForm<CreateVolumeFormData>({
    resolver: zodResolver(createVolumeSchema),
    defaultValues: {
      size: 1,
      availabilityZone: defaultAz,
    },
  })

  const values = useWatch({ control })

  const onSubmit = async (data: CreateVolumeFormData) => {
    let result

    try {
      result = await createMutation.mutateAsync(data)
    } catch {
      // Surfaced beside the submit button through the mutation's error.
      return
    }

    onClose(result.VolumeId)
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={createMutation.isPending || isSubmitting}
      >
        <Field>
          <FieldTitle>
            <label htmlFor="size">Size (GiB)</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.size}
            id="size"
            max={16_384}
            min={1}
            type="number"
            {...register("size", { valueAsNumber: true })}
          />
          <FieldError errors={[errors.size]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="availabilityZone">Availability Zone</label>
          </FieldTitle>
          <Controller
            control={control}
            name="availabilityZone"
            render={({ field }) => (
              <Select onValueChange={field.onChange} value={field.value ?? ""}>
                <SelectTrigger
                  aria-invalid={!!errors.availabilityZone}
                  className="w-full"
                  id="availabilityZone"
                >
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {availabilityZones.map((az) => (
                    <SelectItem key={az.ZoneName} value={az.ZoneName ?? ""}>
                      {az.ZoneName}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
          />
          <FieldError errors={[errors.availabilityZone]} />
        </Field>

        <CliCommandPanel commands={buildCreateVolumeCommands(values)} />

        <FormActions
          error={createMutation.error}
          errorMsg="Failed to create volume"
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            onClose()
          }}
          pendingLabel="Creating…"
          submitLabel="Create Volume"
        />
      </FormLock>
    </form>
  )
}

function buildCreateVolumeCommands(
  values: DeepPartialSkipArrayKey<CreateVolumeFormData>,
): CliCommand[] {
  return [
    {
      label: "Create Volume",
      parts: [
        { type: "bin", value: "AWS_PROFILE=spinifex aws ec2 create-volume" },
        ...commandFlag("\\\n  --size", (values.size ?? 0) || 1),
        ...commandFlag(
          "\\\n  --availability-zone",
          cliPlaceholder(values.availabilityZone ?? "", "AvailabilityZone"),
        ),
      ],
    },
  ]
}

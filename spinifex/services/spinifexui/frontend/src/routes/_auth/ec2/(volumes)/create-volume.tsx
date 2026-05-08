import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import { Controller, useForm } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import {
  CliCommandPanel,
  type CliCommand,
} from "@/components/cli-command-panel"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { PageHeading } from "@/components/page-heading"
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

export const Route = createFileRoute("/_auth/ec2/(volumes)/create-volume")({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(ec2AvailabilityZonesQueryOptions)
  },
  head: () => ({
    meta: [
      {
        title: "Create Volume | EC2 | Mulga",
      },
    ],
  }),
  component: CreateVolume,
})

function CreateVolume() {
  const navigate = useNavigate()
  const { data: azData } = useSuspenseQuery(ec2AvailabilityZonesQueryOptions)
  const createMutation = useCreateVolume()
  const availabilityZones = azData.AvailabilityZones ?? []

  const defaultAz = availabilityZones[0]?.ZoneName ?? ""

  const {
    control,
    handleSubmit,
    register,
    watch,
    formState: { errors, isSubmitting },
  } = useForm<CreateVolumeFormData>({
    resolver: zodResolver(createVolumeSchema),
    defaultValues: {
      size: 1,
      availabilityZone: defaultAz,
    },
  })

  const onSubmit = async (data: CreateVolumeFormData) => {
    await createMutation.mutateAsync(data)
    navigate({ to: "/ec2/describe-volumes" })
  }

  return (
    <>
      <BackLink to="/ec2/describe-volumes">Back to volumes</BackLink>

      <PageHeading title="Create Volume" />

      {createMutation.error && (
        <ErrorBanner
          error={createMutation.error}
          msg="Failed to create volume"
        />
      )}

      <form className="max-w-4xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
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
              <Select
                onValueChange={(value) => field.onChange(value)}
                value={field.value ?? ""}
              >
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

        <CliCommandPanel commands={buildCreateVolumeCommands(watch)} />

        <FormActions
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={async () => await navigate({ to: "/ec2/describe-volumes" })}
          pendingLabel="Creating…"
          submitLabel="Create Volume"
        />
      </form>
    </>
  )
}

function buildCreateVolumeCommands(
  watch: (name?: string) => unknown,
): CliCommand[] {
  const rawSize = watch("size")
  const size = typeof rawSize === "number" ? rawSize : 0
  const rawAz = watch("availabilityZone")
  const az = typeof rawAz === "string" ? rawAz : ""

  return [
    {
      label: "Create Volume",
      parts: [
        { type: "bin", value: "AWS_PROFILE=spinifex aws ec2 create-volume" },
        { type: "flag", value: " \\\n  --size" },
        { type: "value", value: ` ${size || 1}` },
        { type: "flag", value: " \\\n  --availability-zone" },
        { type: "value", value: ` ${az || "<AvailabilityZone>"}` },
      ],
    },
  ]
}

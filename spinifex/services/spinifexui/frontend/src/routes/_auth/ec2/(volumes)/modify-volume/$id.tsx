import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import { useForm } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { PageHeading } from "@/components/page-heading"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { useModifyVolume } from "@/mutations/ec2"
import { ec2VolumeQueryOptions } from "@/queries/ec2"
import { type ModifyVolumeFormData, modifyVolumeSchema } from "@/types/ec2"

export const Route = createFileRoute("/_auth/ec2/(volumes)/modify-volume/$id")({
  loader: async ({ context, params }) => {
    await context.queryClient.ensureQueryData(ec2VolumeQueryOptions(params.id))
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `Resize ${params.id} | EC2 | Mulga`,
      },
    ],
  }),
  component: ModifyVolume,
})

function ModifyVolume() {
  const { id } = Route.useParams()
  const navigate = useNavigate()
  const { data } = useSuspenseQuery(ec2VolumeQueryOptions(id))
  const volume = data.Volumes?.[0]
  const volumeId = volume?.VolumeId
  const modifyMutation = useModifyVolume()

  const currentSize = volume?.Size ?? 1

  const {
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
    defaultValues: {
      size: currentSize,
    },
  })

  if (!volumeId) {
    return (
      <>
        <BackLink to="/ec2/describe-volumes">Back to volumes</BackLink>
        <p className="text-muted-foreground">Volume not found.</p>
      </>
    )
  }

  const onSubmit = async (formData: ModifyVolumeFormData) => {
    await modifyMutation.mutateAsync({
      ...formData,
      volumeId,
    })

    navigate({ to: "/ec2/describe-volumes/$id", params: { id: volumeId } })
  }

  return (
    <>
      <BackLink params={{ id: volumeId }} to="/ec2/describe-volumes/$id">
        Back to volume
      </BackLink>

      <PageHeading subtitle={volumeId} title="Resize Volume" />

      {modifyMutation.error && (
        <ErrorBanner
          error={modifyMutation.error}
          msg="Failed to resize volume"
        />
      )}

      <form className="max-w-4xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
        <div className="space-y-2">
          <p className="text-sm text-muted-foreground">
            Current size: {currentSize} GiB
          </p>
        </div>

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
          <FieldError errors={[errors.size]} />
        </Field>

        <FormActions
          isPending={modifyMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={async () =>
            await navigate({
              to: "/ec2/describe-volumes/$id",
              params: { id: volumeId },
            })
          }
          pendingLabel="Resizing\u2026"
          submitLabel="Resize Volume"
        />
      </form>
    </>
  )
}

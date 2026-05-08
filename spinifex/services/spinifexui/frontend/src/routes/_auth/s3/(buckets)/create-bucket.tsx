import { zodResolver } from "@hookform/resolvers/zod"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import { useForm } from "react-hook-form"

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
import { useCreateBucket } from "@/mutations/s3"
import { type CreateBucketFormData, createBucketSchema } from "@/types/s3"

export const Route = createFileRoute("/_auth/s3/(buckets)/create-bucket")({
  head: () => ({
    meta: [
      {
        title: "Create Bucket | S3 | Mulga",
      },
    ],
  }),
  component: CreateBucket,
})

function CreateBucket() {
  const navigate = useNavigate()
  const createMutation = useCreateBucket()

  const {
    handleSubmit,
    register,
    watch,
    formState: { errors, isSubmitting },
  } = useForm({
    resolver: zodResolver(createBucketSchema),
  })

  const onSubmit = async (data: CreateBucketFormData) => {
    await createMutation.mutateAsync(data)
    navigate({ to: "/s3/ls" })
  }

  return (
    <>
      <BackLink to="/s3/ls">Back to buckets</BackLink>

      <PageHeading title="Create Bucket" />

      {createMutation.error && (
        <ErrorBanner
          error={createMutation.error}
          msg="Failed to create bucket"
        />
      )}

      <form className="max-w-4xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
        <Field>
          <FieldTitle>
            <label htmlFor="bucketName">Bucket Name</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.bucketName}
            id="bucketName"
            placeholder="my-bucket"
            type="text"
            {...register("bucketName")}
          />
          <FieldError errors={[errors.bucketName]} />
        </Field>

        <CliCommandPanel commands={buildCreateBucketCommands(watch)} />

        <FormActions
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={async () => await navigate({ to: "/s3/ls" })}
          pendingLabel="Creating…"
          submitLabel="Create Bucket"
        />
      </form>
    </>
  )
}

function buildCreateBucketCommands(
  watch: (name?: string) => unknown,
): CliCommand[] {
  const rawBucket = watch("bucketName")
  const bucket = typeof rawBucket === "string" ? rawBucket : ""

  return [
    {
      label: "Create Bucket",
      parts: [
        { type: "bin", value: "AWS_PROFILE=spinifex aws s3api create-bucket" },
        { type: "flag", value: " --bucket" },
        { type: "value", value: ` ${bucket || "<BucketName>"}` },
      ],
    },
  ]
}

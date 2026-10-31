import { zodResolver } from "@hookform/resolvers/zod"
import {
  type DeepPartialSkipArrayKey,
  useForm,
  useWatch,
} from "react-hook-form"

import {
  CliCommandPanel,
  type CliCommand,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { useCreateBucket } from "@/mutations/s3"
import { type CreateBucketFormData, createBucketSchema } from "@/types/s3"

interface CreateBucketPageProps {
  // Called with the new bucket's name once created, or with nothing on cancel.
  onClose: (bucket?: string) => void
}

export function CreateBucketPage({ onClose }: CreateBucketPageProps) {
  const createMutation = useCreateBucket()

  const {
    handleSubmit,
    register,
    control,
    formState: { errors, isSubmitting },
  } = useForm({
    resolver: zodResolver(createBucketSchema),
  })

  const values = useWatch({ control })

  const onSubmit = async (data: CreateBucketFormData) => {
    try {
      await createMutation.mutateAsync(data)
      onClose(data.bucketName)
    } catch {
      // Surfaced beside the submit button through createMutation.error.
    }
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={createMutation.isPending || isSubmitting}
      >
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

        <CliCommandPanel commands={buildCreateBucketCommands(values)} />

        <FormActions
          error={createMutation.error}
          errorMsg="Failed to create bucket"
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            onClose()
          }}
          pendingLabel="Creating…"
          submitLabel="Create Bucket"
        />
      </FormLock>
    </form>
  )
}

function buildCreateBucketCommands(
  values: DeepPartialSkipArrayKey<CreateBucketFormData>,
): CliCommand[] {
  const bucket = values.bucketName ?? ""

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

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
import { useCreateInstanceProfile } from "@/mutations/iam"
import {
  type CreateInstanceProfileFormData,
  createInstanceProfileSchema,
} from "@/types/iam"

interface CreateInstanceProfilePageProps {
  // Called with the new instance profile's key once created, or with nothing on cancel.
  onClose: (id?: string) => void
}

export function CreateInstanceProfilePage({
  onClose,
}: CreateInstanceProfilePageProps) {
  const createMutation = useCreateInstanceProfile()

  const {
    handleSubmit,
    register,
    control,
    formState: { errors, isSubmitting },
  } = useForm({
    resolver: zodResolver(createInstanceProfileSchema),
  })

  const values = useWatch({ control })

  const onSubmit = async (data: CreateInstanceProfileFormData) => {
    try {
      await createMutation.mutateAsync(data)
    } catch {
      // Surfaced beside the submit button through the mutation's error.
      return
    }

    onClose(data.instanceProfileName)
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={createMutation.isPending || isSubmitting}
      >
        <Field>
          <FieldTitle>
            <label htmlFor="instanceProfileName">Instance Profile Name</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.instanceProfileName}
            id="instanceProfileName"
            placeholder="my-instance-profile..."
            {...register("instanceProfileName")}
          />
          <FieldError errors={[errors.instanceProfileName]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="path">Path (optional)</label>
          </FieldTitle>
          <Input id="path" placeholder="/" {...register("path")} />
          <FieldError errors={[errors.path]} />
        </Field>

        <CliCommandPanel
          commands={buildCreateInstanceProfileCommands(values)}
        />

        <FormActions
          error={createMutation.error}
          errorMsg="Failed to create instance profile"
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            onClose()
          }}
          pendingLabel="Creating…"
          submitLabel="Create Instance Profile"
        />
      </FormLock>
    </form>
  )
}

function buildCreateInstanceProfileCommands(
  values: DeepPartialSkipArrayKey<CreateInstanceProfileFormData>,
): CliCommand[] {
  const name = values.instanceProfileName ?? ""
  const path = values.path ?? ""

  const parts = [
    {
      type: "bin" as const,
      value: "AWS_PROFILE=spinifex aws iam create-instance-profile",
    },
    { type: "flag" as const, value: " --instance-profile-name" },
    { type: "value" as const, value: ` ${name || "<InstanceProfileName>"}` },
  ]

  if (path && path !== "/") {
    parts.push(
      { type: "flag" as const, value: " --path" },
      { type: "value" as const, value: ` ${path}` },
    )
  }

  return [{ label: "Create Instance Profile", parts }]
}

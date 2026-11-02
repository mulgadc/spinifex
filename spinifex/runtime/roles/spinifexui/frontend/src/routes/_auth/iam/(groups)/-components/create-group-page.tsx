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
import { useCreateGroup } from "@/mutations/iam"
import { type CreateGroupFormData, createGroupSchema } from "@/types/iam"

interface CreateGroupPageProps {
  // Called with the new group's key once created, or with nothing on cancel.
  onClose: (id?: string) => void
}

export function CreateGroupPage({ onClose }: CreateGroupPageProps) {
  const createMutation = useCreateGroup()

  const {
    handleSubmit,
    register,
    control,
    formState: { errors, isSubmitting },
  } = useForm({
    resolver: zodResolver(createGroupSchema),
  })

  const values = useWatch({ control })

  const onSubmit = async (data: CreateGroupFormData) => {
    try {
      await createMutation.mutateAsync(data)
    } catch {
      // Surfaced beside the submit button through the mutation's error.
      return
    }

    onClose(data.groupName)
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={createMutation.isPending || isSubmitting}
      >
        <Field>
          <FieldTitle>
            <label htmlFor="groupName">Group Name</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.groupName}
            id="groupName"
            placeholder="my-group..."
            {...register("groupName")}
          />
          <FieldError errors={[errors.groupName]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="path">Path (optional)</label>
          </FieldTitle>
          <Input id="path" placeholder="/" {...register("path")} />
          <FieldError errors={[errors.path]} />
        </Field>

        <CliCommandPanel commands={buildCreateGroupCommands(values)} />

        <FormActions
          error={createMutation.error}
          errorMsg="Failed to create group"
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            onClose()
          }}
          pendingLabel="Creating…"
          submitLabel="Create Group"
        />
      </FormLock>
    </form>
  )
}

function buildCreateGroupCommands(
  values: DeepPartialSkipArrayKey<CreateGroupFormData>,
): CliCommand[] {
  const groupName = values.groupName ?? ""
  const path = values.path ?? ""

  const parts = [
    {
      type: "bin" as const,
      value: "AWS_PROFILE=spinifex aws iam create-group",
    },
    { type: "flag" as const, value: " --group-name" },
    { type: "value" as const, value: ` ${groupName || "<GroupName>"}` },
  ]

  if (path && path !== "/") {
    parts.push(
      { type: "flag" as const, value: " --path" },
      { type: "value" as const, value: ` ${path}` },
    )
  }

  return [{ label: "Create Group", parts }]
}

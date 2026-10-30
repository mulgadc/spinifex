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
import { useCreateUser } from "@/mutations/iam"
import { type CreateUserFormData, createUserSchema } from "@/types/iam"

interface CreateUserPageProps {
  // Called with the new user's key once created, or with nothing on cancel.
  onClose: (id?: string) => void
}

export function CreateUserPage({ onClose }: CreateUserPageProps) {
  const createMutation = useCreateUser()

  const {
    handleSubmit,
    register,
    control,
    formState: { errors, isSubmitting },
  } = useForm({
    resolver: zodResolver(createUserSchema),
  })

  const values = useWatch({ control })

  const onSubmit = async (data: CreateUserFormData) => {
    try {
      await createMutation.mutateAsync(data)
    } catch {
      // Surfaced beside the submit button through the mutation's error.
      return
    }

    onClose(data.userName)
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={createMutation.isPending || isSubmitting}
      >
        <Field>
          <FieldTitle>
            <label htmlFor="userName">User Name</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.userName}
            id="userName"
            placeholder="my-user..."
            {...register("userName")}
          />
          <FieldError errors={[errors.userName]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="path">Path (optional)</label>
          </FieldTitle>
          <Input id="path" placeholder="/" {...register("path")} />
          <FieldError errors={[errors.path]} />
        </Field>

        <CliCommandPanel commands={buildCreateUserCommands(values)} />

        <FormActions
          error={createMutation.error}
          errorMsg="Failed to create user"
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            onClose()
          }}
          pendingLabel="Creating…"
          submitLabel="Create User"
        />
      </FormLock>
    </form>
  )
}

function buildCreateUserCommands(
  values: DeepPartialSkipArrayKey<CreateUserFormData>,
): CliCommand[] {
  const userName = values.userName ?? ""
  const path = values.path ?? ""

  const parts = [
    { type: "bin" as const, value: "AWS_PROFILE=spinifex aws iam create-user" },
    { type: "flag" as const, value: " --user-name" },
    { type: "value" as const, value: ` ${userName || "<UserName>"}` },
  ]

  if (path && path !== "/") {
    parts.push(
      { type: "flag" as const, value: " --path" },
      { type: "value" as const, value: ` ${path}` },
    )
  }

  return [{ label: "Create User", parts }]
}

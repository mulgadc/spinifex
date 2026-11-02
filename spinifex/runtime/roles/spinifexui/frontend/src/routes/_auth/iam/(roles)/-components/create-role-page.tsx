import { zodResolver } from "@hookform/resolvers/zod"
import {
  Controller,
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
import { JsonEditor } from "@/components/ui/json-editor"
import { useCreateRole } from "@/mutations/iam"
import {
  type CreateRoleFormData,
  createRoleSchema,
  DEFAULT_ASSUME_ROLE_POLICY_DOCUMENT,
} from "@/types/iam"

interface CreateRolePageProps {
  // Called with the new role's key once created, or with nothing on cancel.
  onClose: (id?: string) => void
}

export function CreateRolePage({ onClose }: CreateRolePageProps) {
  const createMutation = useCreateRole()

  const {
    handleSubmit,
    register,
    control,
    formState: { errors, isSubmitting },
  } = useForm({
    resolver: zodResolver(createRoleSchema),
    defaultValues: {
      assumeRolePolicyDocument: DEFAULT_ASSUME_ROLE_POLICY_DOCUMENT,
    },
  })

  const values = useWatch({ control })

  const onSubmit = async (data: CreateRoleFormData) => {
    try {
      await createMutation.mutateAsync(data)
    } catch {
      // Surfaced beside the submit button through the mutation's error.
      return
    }

    onClose(data.roleName)
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={createMutation.isPending || isSubmitting}
      >
        <Field>
          <FieldTitle>
            <label htmlFor="roleName">Role Name</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.roleName}
            id="roleName"
            placeholder="my-role..."
            {...register("roleName")}
          />
          <FieldError errors={[errors.roleName]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="description">Description (optional)</label>
          </FieldTitle>
          <Input
            id="description"
            placeholder="A brief description of this role..."
            {...register("description")}
          />
          <FieldError errors={[errors.description]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="assumeRolePolicyDocument">
              Trust Policy (JSON)
            </label>
          </FieldTitle>
          <Controller
            control={control}
            name="assumeRolePolicyDocument"
            render={({ field: { onBlur, onChange, ref, value } }) => (
              <JsonEditor
                error={!!errors.assumeRolePolicyDocument}
                id="assumeRolePolicyDocument"
                onBlur={onBlur}
                onChange={onChange}
                ref={ref}
                rows={15}
                value={value}
              />
            )}
          />
          <FieldError errors={[errors.assumeRolePolicyDocument]} />
        </Field>

        <CliCommandPanel commands={buildCreateRoleCommands(values)} />

        <FormActions
          error={createMutation.error}
          errorMsg="Failed to create role"
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            onClose()
          }}
          pendingLabel="Creating…"
          submitLabel="Create Role"
        />
      </FormLock>
    </form>
  )
}

function buildCreateRoleCommands(
  values: DeepPartialSkipArrayKey<CreateRoleFormData>,
): CliCommand[] {
  const name = values.roleName ?? ""
  const desc = values.description ?? ""
  const doc = values.assumeRolePolicyDocument ?? ""

  const parts = [
    {
      type: "bin" as const,
      value: "AWS_PROFILE=spinifex aws iam create-role",
    },
    { type: "flag" as const, value: " \\\n  --role-name" },
    { type: "value" as const, value: ` ${name || "<RoleName>"}` },
  ]

  if (desc) {
    parts.push(
      { type: "flag" as const, value: " \\\n  --description" },
      { type: "value" as const, value: ` "${desc}"` },
    )
  }

  parts.push(
    { type: "flag" as const, value: " \\\n  --assume-role-policy-document" },
    { type: "value" as const, value: ` '${doc || "<TrustPolicy>"}'` },
  )

  return [{ label: "Create Role", parts }]
}

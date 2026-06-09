import { zodResolver } from "@hookform/resolvers/zod"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import { useForm, useWatch } from "react-hook-form"

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
import { Textarea } from "@/components/ui/textarea"
import { useCreateRole } from "@/mutations/iam"
import {
  type CreateRoleFormData,
  createRoleSchema,
  DEFAULT_ASSUME_ROLE_POLICY_DOCUMENT,
} from "@/types/iam"

export const Route = createFileRoute("/_auth/iam/(roles)/create-role")({
  head: () => ({
    meta: [{ title: "Create Role | IAM | Mulga" }],
  }),
  component: CreateRole,
})

function CreateRole() {
  const navigate = useNavigate()
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
  const cliWatch = (name?: string): unknown =>
    name ? (values as Record<string, unknown>)[name] : undefined

  const onSubmit = async (data: CreateRoleFormData) => {
    await createMutation.mutateAsync(data)
    navigate({ to: "/iam/list-roles" })
  }

  return (
    <>
      <BackLink to="/iam/list-roles">Back to roles</BackLink>
      <PageHeading title="Create Role" />

      {createMutation.error && (
        <ErrorBanner error={createMutation.error} msg="Failed to create role" />
      )}

      <form className="max-w-4xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
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
          <Textarea
            aria-invalid={!!errors.assumeRolePolicyDocument}
            className="font-mono text-sm"
            id="assumeRolePolicyDocument"
            rows={15}
            {...register("assumeRolePolicyDocument")}
          />
          <FieldError errors={[errors.assumeRolePolicyDocument]} />
        </Field>

        <CliCommandPanel commands={buildCreateRoleCommands(cliWatch)} />

        <FormActions
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={async () => await navigate({ to: "/iam/list-roles" })}
          pendingLabel="Creating..."
          submitLabel="Create Role"
        />
      </form>
    </>
  )
}

function buildCreateRoleCommands(
  watch: (name?: string) => unknown,
): CliCommand[] {
  const rawName = watch("roleName")
  const name = typeof rawName === "string" ? rawName : ""
  const rawDesc = watch("description")
  const desc = typeof rawDesc === "string" ? rawDesc : ""
  const rawDoc = watch("assumeRolePolicyDocument")
  const doc = typeof rawDoc === "string" ? rawDoc : ""

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

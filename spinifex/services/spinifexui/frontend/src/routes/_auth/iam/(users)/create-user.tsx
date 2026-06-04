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
import { useCreateUser } from "@/mutations/iam"
import { type CreateUserFormData, createUserSchema } from "@/types/iam"

export const Route = createFileRoute("/_auth/iam/(users)/create-user")({
  head: () => ({
    meta: [{ title: "Create User | IAM | Mulga" }],
  }),
  component: CreateUser,
})

function CreateUser() {
  const navigate = useNavigate()
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
  const cliWatch = (name?: string): unknown =>
    name ? (values as Record<string, unknown>)[name] : undefined

  const onSubmit = async (data: CreateUserFormData) => {
    await createMutation.mutateAsync(data)
    navigate({ to: "/iam/list-users" })
  }

  return (
    <>
      <BackLink to="/iam/list-users">Back to users</BackLink>
      <PageHeading title="Create User" />

      {createMutation.error && (
        <ErrorBanner error={createMutation.error} msg="Failed to create user" />
      )}

      <form className="max-w-4xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
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

        <CliCommandPanel commands={buildCreateUserCommands(cliWatch)} />

        <FormActions
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={async () => await navigate({ to: "/iam/list-users" })}
          pendingLabel="Creating..."
          submitLabel="Create User"
        />
      </form>
    </>
  )
}

function buildCreateUserCommands(
  watch: (name?: string) => unknown,
): CliCommand[] {
  const rawUserName = watch("userName")
  const userName = typeof rawUserName === "string" ? rawUserName : ""
  const rawPath = watch("path")
  const path = typeof rawPath === "string" ? rawPath : ""

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

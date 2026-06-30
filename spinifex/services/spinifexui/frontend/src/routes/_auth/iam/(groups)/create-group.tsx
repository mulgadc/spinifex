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
import { useCreateGroup } from "@/mutations/iam"
import { type CreateGroupFormData, createGroupSchema } from "@/types/iam"

export const Route = createFileRoute("/_auth/iam/(groups)/create-group")({
  head: () => ({
    meta: [{ title: "Create Group | IAM | Mulga" }],
  }),
  component: CreateGroup,
})

function CreateGroup() {
  const navigate = useNavigate()
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
  const cliWatch = (name?: string): unknown =>
    name ? (values as Record<string, unknown>)[name] : undefined

  const onSubmit = async (data: CreateGroupFormData) => {
    await createMutation.mutateAsync(data)
    navigate({ to: "/iam/list-groups" })
  }

  return (
    <>
      <BackLink to="/iam/list-groups">Back to groups</BackLink>
      <PageHeading title="Create Group" />

      {createMutation.error && (
        <ErrorBanner
          error={createMutation.error}
          msg="Failed to create group"
        />
      )}

      <form className="max-w-4xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
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

        <CliCommandPanel commands={buildCreateGroupCommands(cliWatch)} />

        <FormActions
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={async () => await navigate({ to: "/iam/list-groups" })}
          pendingLabel="Creating..."
          submitLabel="Create Group"
        />
      </form>
    </>
  )
}

function buildCreateGroupCommands(
  watch: (name?: string) => unknown,
): CliCommand[] {
  const rawGroupName = watch("groupName")
  const groupName = typeof rawGroupName === "string" ? rawGroupName : ""
  const rawPath = watch("path")
  const path = typeof rawPath === "string" ? rawPath : ""

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

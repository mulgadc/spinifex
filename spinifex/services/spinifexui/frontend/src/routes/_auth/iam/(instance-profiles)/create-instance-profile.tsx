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
import { useCreateInstanceProfile } from "@/mutations/iam"
import {
  type CreateInstanceProfileFormData,
  createInstanceProfileSchema,
} from "@/types/iam"

export const Route = createFileRoute(
  "/_auth/iam/(instance-profiles)/create-instance-profile",
)({
  head: () => ({
    meta: [{ title: "Create Instance Profile | IAM | Mulga" }],
  }),
  component: CreateInstanceProfile,
})

function CreateInstanceProfile() {
  const navigate = useNavigate()
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
  const cliWatch = (name?: string): unknown =>
    name ? (values as Record<string, unknown>)[name] : undefined

  const onSubmit = async (data: CreateInstanceProfileFormData) => {
    await createMutation.mutateAsync(data)
    navigate({ to: "/iam/list-instance-profiles" })
  }

  return (
    <>
      <BackLink to="/iam/list-instance-profiles">
        Back to instance profiles
      </BackLink>
      <PageHeading title="Create Instance Profile" />

      {createMutation.error && (
        <ErrorBanner
          error={createMutation.error}
          msg="Failed to create instance profile"
        />
      )}

      <form className="max-w-4xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
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
          commands={buildCreateInstanceProfileCommands(cliWatch)}
        />

        <FormActions
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={async () =>
            await navigate({ to: "/iam/list-instance-profiles" })
          }
          pendingLabel="Creating..."
          submitLabel="Create Instance Profile"
        />
      </form>
    </>
  )
}

function buildCreateInstanceProfileCommands(
  watch: (name?: string) => unknown,
): CliCommand[] {
  const rawName = watch("instanceProfileName")
  const name = typeof rawName === "string" ? rawName : ""
  const rawPath = watch("path")
  const path = typeof rawPath === "string" ? rawPath : ""

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

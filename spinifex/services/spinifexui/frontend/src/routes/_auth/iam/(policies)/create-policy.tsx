import { zodResolver } from "@hookform/resolvers/zod"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import { Controller, useForm, useWatch } from "react-hook-form"

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
import { JsonEditor } from "@/components/ui/json-editor"
import { useCreatePolicy } from "@/mutations/iam"
import { type CreatePolicyFormData, createPolicySchema } from "@/types/iam"

const DEFAULT_POLICY_DOCUMENT = JSON.stringify(
  {
    Version: "2012-10-17",
    Statement: [
      {
        Effect: "Allow",
        Action: "*",
        Resource: "*",
      },
    ],
  },
  null,
  2,
)

export const Route = createFileRoute("/_auth/iam/(policies)/create-policy")({
  head: () => ({
    meta: [{ title: "Create Policy | IAM | Mulga" }],
  }),
  component: CreatePolicy,
})

function CreatePolicy() {
  const navigate = useNavigate()
  const createMutation = useCreatePolicy()

  const {
    handleSubmit,
    register,
    control,
    formState: { errors, isSubmitting },
  } = useForm({
    resolver: zodResolver(createPolicySchema),
    defaultValues: {
      policyDocument: DEFAULT_POLICY_DOCUMENT,
    },
  })

  const values = useWatch({ control })
  const cliWatch = (name?: string): unknown =>
    name ? (values as Record<string, unknown>)[name] : undefined

  const onSubmit = async (data: CreatePolicyFormData) => {
    await createMutation.mutateAsync(data)
    navigate({ to: "/iam/list-policies" })
  }

  return (
    <>
      <BackLink to="/iam/list-policies">Back to policies</BackLink>
      <PageHeading title="Create Policy" />

      {createMutation.error && (
        <ErrorBanner
          error={createMutation.error}
          msg="Failed to create policy"
        />
      )}

      <form className="max-w-4xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
        <Field>
          <FieldTitle>
            <label htmlFor="policyName">Policy Name</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.policyName}
            id="policyName"
            placeholder="my-policy..."
            {...register("policyName")}
          />
          <FieldError errors={[errors.policyName]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="description">Description (optional)</label>
          </FieldTitle>
          <Input
            id="description"
            placeholder="A brief description of this policy..."
            {...register("description")}
          />
          <FieldError errors={[errors.description]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="policyDocument">Policy Document (JSON)</label>
          </FieldTitle>
          <Controller
            control={control}
            name="policyDocument"
            render={({ field: { onBlur, onChange, ref, value } }) => (
              <JsonEditor
                error={!!errors.policyDocument}
                id="policyDocument"
                onBlur={onBlur}
                onChange={onChange}
                ref={ref}
                rows={15}
                value={value}
              />
            )}
          />
          <FieldError errors={[errors.policyDocument]} />
        </Field>

        <CliCommandPanel commands={buildCreatePolicyCommands(cliWatch)} />

        <FormActions
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={async () => await navigate({ to: "/iam/list-policies" })}
          pendingLabel="Creating..."
          submitLabel="Create Policy"
        />
      </form>
    </>
  )
}

function buildCreatePolicyCommands(
  watch: (name?: string) => unknown,
): CliCommand[] {
  const rawName = watch("policyName")
  const name = typeof rawName === "string" ? rawName : ""
  const rawDesc = watch("description")
  const desc = typeof rawDesc === "string" ? rawDesc : ""
  const rawDoc = watch("policyDocument")
  const doc = typeof rawDoc === "string" ? rawDoc : ""

  const parts = [
    {
      type: "bin" as const,
      value: "AWS_PROFILE=spinifex aws iam create-policy",
    },
    { type: "flag" as const, value: " \\\n  --policy-name" },
    { type: "value" as const, value: ` ${name || "<PolicyName>"}` },
  ]

  if (desc) {
    parts.push(
      { type: "flag" as const, value: " \\\n  --description" },
      { type: "value" as const, value: ` "${desc}"` },
    )
  }

  parts.push(
    { type: "flag" as const, value: " \\\n  --policy-document" },
    { type: "value" as const, value: ` '${doc || "<PolicyDocument>"}'` },
  )

  return [{ label: "Create Policy", parts }]
}

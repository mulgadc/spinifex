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

interface CreatePolicyPageProps {
  // Called with the new policy's key once created, or with nothing on cancel.
  onClose: (id?: string) => void
}

export function CreatePolicyPage({ onClose }: CreatePolicyPageProps) {
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

  const onSubmit = async (data: CreatePolicyFormData) => {
    let result

    try {
      result = await createMutation.mutateAsync(data)
    } catch {
      // Surfaced beside the submit button through the mutation's error.
      return
    }

    onClose(result.Policy?.Arn)
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={createMutation.isPending || isSubmitting}
      >
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

        <CliCommandPanel commands={buildCreatePolicyCommands(values)} />

        <FormActions
          error={createMutation.error}
          errorMsg="Failed to create policy"
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            onClose()
          }}
          pendingLabel="Creating…"
          submitLabel="Create Policy"
        />
      </FormLock>
    </form>
  )
}

function buildCreatePolicyCommands(
  values: DeepPartialSkipArrayKey<CreatePolicyFormData>,
): CliCommand[] {
  const name = values.policyName ?? ""
  const desc = values.description ?? ""
  const doc = values.policyDocument ?? ""

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

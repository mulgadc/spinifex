import { zodResolver } from "@hookform/resolvers/zod"
import { useForm, useWatch } from "react-hook-form"

import {
  CliCommandPanel,
  type CliCommand,
  cliPlaceholder,
  commandFlag,
  optionalFlag,
  shellSingleQuote,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { FormPanel } from "@/components/form-panel"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { useCreateAccessEntry } from "@/mutations/eks"
import {
  type CreateAccessEntryFormValues,
  createAccessEntrySchema,
} from "@/types/eks"

function splitGroups(value: string): string[] {
  return value
    .split(",")
    .map((g) => g.trim())
    .filter(Boolean)
}

function buildCreateAccessEntryCommands(
  clusterName: string,
  principalArn: string,
  groups: string[],
): CliCommand[] {
  return [
    {
      label: "Create access entry",
      parts: [
        {
          type: "bin",
          value: "AWS_PROFILE=spinifex aws eks create-access-entry",
        },
        ...commandFlag("\\\n  --cluster-name", clusterName),
        ...commandFlag(
          "\\\n  --principal-arn",
          cliPlaceholder(principalArn, "principal-arn"),
        ),
        ...optionalFlag(
          "\\\n  --kubernetes-groups",
          groups.map((g) => shellSingleQuote(g)).join(" "),
        ),
      ],
    },
  ]
}

export function AddAccessEntryForm({
  clusterName,
  onClose,
}: {
  clusterName: string
  onClose: () => void
}) {
  const createEntry = useCreateAccessEntry()

  const {
    control,
    formState: { errors, isSubmitting },
    handleSubmit,
    register,
  } = useForm<CreateAccessEntryFormValues>({
    resolver: zodResolver(createAccessEntrySchema),
    defaultValues: { principalArn: "", kubernetesGroups: "" },
  })

  const [principalArn, kubernetesGroups] = useWatch({
    control,
    name: ["principalArn", "kubernetesGroups"],
  })

  const groups = splitGroups(kubernetesGroups)

  const onSubmit = async (data: CreateAccessEntryFormValues) => {
    try {
      await createEntry.mutateAsync({
        clusterName,
        principalArn: data.principalArn,
        kubernetesGroups: splitGroups(data.kubernetesGroups),
      })
    } catch {
      // Shown beside the submit button through the mutation's error.
      return
    }

    onClose()
  }

  return (
    <FormPanel
      onClose={onClose}
      subtitle={clusterName}
      title="Add access entry"
    >
      <form className="space-y-6" onSubmit={handleSubmit(onSubmit)}>
        <FormLock className="space-y-4" locked={createEntry.isPending}>
          <Field>
            <FieldTitle>
              <label htmlFor="ae-principal">IAM principal ARN</label>
            </FieldTitle>
            <Input
              aria-invalid={!!errors.principalArn}
              id="ae-principal"
              placeholder="arn:aws:iam::000000000000:role/my-role"
              {...register("principalArn")}
            />
            <FieldError errors={[errors.principalArn]} />
          </Field>

          <Field>
            <FieldTitle>
              <label htmlFor="ae-groups">Kubernetes groups</label>
            </FieldTitle>
            <Input
              id="ae-groups"
              placeholder="system:masters"
              {...register("kubernetesGroups")}
            />
          </Field>
        </FormLock>

        <CliCommandPanel
          commands={buildCreateAccessEntryCommands(
            clusterName,
            principalArn,
            groups,
          )}
        />

        <FormActions
          error={createEntry.error}
          errorMsg="Failed to create the access entry"
          isPending={createEntry.isPending}
          isSubmitting={isSubmitting}
          onCancel={onClose}
          pendingLabel="Creating…"
          submitLabel="Create"
        />
      </form>
    </FormPanel>
  )
}

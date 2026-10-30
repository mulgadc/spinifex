import { zodResolver } from "@hookform/resolvers/zod"
import { useForm, useWatch } from "react-hook-form"

import {
  CliCommandPanel,
  type CliCommand,
  optionalFlag,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { Field, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { useCreateInternetGateway } from "@/mutations/ec2"
import {
  type CreateInternetGatewayFormData,
  createInternetGatewaySchema,
} from "@/types/ec2"

interface CreateInternetGatewayPageProps {
  // Called with the new resource's ID once created, or with nothing on cancel.
  onClose: (internetGatewayId?: string) => void
}

export function CreateInternetGatewayPage({
  onClose,
}: CreateInternetGatewayPageProps) {
  const finish = onClose

  const createMutation = useCreateInternetGateway()

  const {
    control,
    handleSubmit,
    register,
    formState: { isSubmitting },
  } = useForm<CreateInternetGatewayFormData>({
    resolver: zodResolver(createInternetGatewaySchema),
    defaultValues: { name: "" },
  })

  const values = useWatch({ control })

  const onSubmit = async (data: CreateInternetGatewayFormData) => {
    let result

    try {
      result = await createMutation.mutateAsync({
        name: data.name?.trim() ?? undefined,
      })
    } catch {
      // Surfaced beside the submit button through the mutation's error.
      return
    }

    finish(result.InternetGateway?.InternetGatewayId)
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={createMutation.isPending || isSubmitting}
      >
        <Field>
          <FieldTitle>
            <label htmlFor="name">Name (optional)</label>
          </FieldTitle>
          <Input
            id="name"
            placeholder="my-internet-gateway"
            {...register("name")}
          />
        </Field>

        <CliCommandPanel
          commands={buildCreateInternetGatewayCommands(values.name ?? "")}
        />

        <FormActions
          error={createMutation.error}
          errorMsg="Failed to create Internet Gateway"
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            finish()
          }}
          pendingLabel="Creating…"
          submitLabel="Create"
        />
      </FormLock>
    </form>
  )
}

function buildCreateInternetGatewayCommands(name: string): CliCommand[] {
  const tagName = name.trim()

  return [
    {
      label: "Create Internet Gateway",
      parts: [
        {
          type: "bin",
          value: "AWS_PROFILE=spinifex aws ec2 create-internet-gateway",
        },
        ...optionalFlag(
          "\\\n  --tag-specifications",
          tagName &&
            `'ResourceType=internet-gateway,Tags=[{Key=Name,Value=${tagName}}]'`,
        ),
      ],
    },
  ]
}

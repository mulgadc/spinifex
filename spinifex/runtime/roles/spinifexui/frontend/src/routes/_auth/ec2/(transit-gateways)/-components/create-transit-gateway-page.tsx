import { zodResolver } from "@hookform/resolvers/zod"
import {
  type DeepPartialSkipArrayKey,
  useForm,
  useWatch,
} from "react-hook-form"

import {
  CliCommandPanel,
  type CliCommand,
  optionalFlag,
  shellSingleQuote,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { TextField } from "@/components/form-fields"
import { FormLock } from "@/components/form-lock"
import { nameTagSpecificationArg } from "@/lib/transit-gateway"
import { useCreateTransitGateway } from "@/mutations/ec2"
import {
  type CreateTransitGatewayFormData,
  createTransitGatewaySchema,
} from "@/types/ec2"

interface CreateTransitGatewayPageProps {
  // Called with the new resource's ID once created, or with nothing on cancel.
  onClose: (id?: string) => void
}

export function CreateTransitGatewayPage({
  onClose,
}: CreateTransitGatewayPageProps) {
  const createMutation = useCreateTransitGateway()

  const {
    control,
    handleSubmit,
    register,
    formState: { isSubmitting },
  } = useForm<CreateTransitGatewayFormData>({
    resolver: zodResolver(createTransitGatewaySchema),
    defaultValues: { name: "", description: "" },
  })

  const values = useWatch({ control })

  const onSubmit = async (data: CreateTransitGatewayFormData) => {
    let result

    try {
      result = await createMutation.mutateAsync({
        name: data.name?.trim() || undefined,
        description: data.description?.trim() || undefined,
      })
    } catch {
      // Surfaced beside the submit button through the mutation's error.
      return
    }

    onClose(result.TransitGateway?.TransitGatewayId)
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={createMutation.isPending || isSubmitting}
      >
        <TextField
          id="name"
          label="Name (optional)"
          placeholder="my-transit-gateway"
          {...register("name")}
        />

        <TextField
          id="description"
          label="Description (optional)"
          placeholder="Hub for the production VPCs"
          {...register("description")}
        />

        <CliCommandPanel commands={buildCreateTransitGatewayCommands(values)} />

        <FormActions
          error={createMutation.error}
          errorMsg="Failed to create transit gateway"
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            onClose()
          }}
          pendingLabel="Creating…"
          submitLabel="Create transit gateway"
        />
      </FormLock>
    </form>
  )
}

function buildCreateTransitGatewayCommands(
  values: DeepPartialSkipArrayKey<CreateTransitGatewayFormData>,
): CliCommand[] {
  const tagName = values.name?.trim()
  const description = values.description?.trim()

  return [
    {
      label: "Create transit gateway",
      parts: [
        {
          type: "bin",
          value: "AWS_PROFILE=spinifex aws ec2 create-transit-gateway",
        },
        ...optionalFlag(
          "\\\n  --description",
          description ? shellSingleQuote(description) : undefined,
        ),
        ...optionalFlag(
          "\\\n  --tag-specifications",
          tagName
            ? nameTagSpecificationArg("transit-gateway", tagName)
            : undefined,
        ),
      ],
    },
  ]
}

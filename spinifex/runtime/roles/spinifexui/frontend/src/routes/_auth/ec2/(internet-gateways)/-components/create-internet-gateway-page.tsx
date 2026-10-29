import { zodResolver } from "@hookform/resolvers/zod"
import { useNavigate } from "@tanstack/react-router"
import { useForm, useWatch } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import {
  CliCommandPanel,
  type CliCommand,
  optionalFlag,
} from "@/components/cli-command-panel"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { PageHeading } from "@/components/page-heading"
import { Field, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { useCreateInternetGateway } from "@/mutations/ec2"
import {
  type CreateInternetGatewayFormData,
  createInternetGatewaySchema,
} from "@/types/ec2"

interface CreateInternetGatewayPageProps {
  // Given when the form sits in the list's side panel: called with the new
  // gateway's ID once created, or with nothing on cancel.
  onClose?: (internetGatewayId?: string) => void
}

export function CreateInternetGatewayPage({
  onClose,
}: CreateInternetGatewayPageProps) {
  const navigate = useNavigate()

  async function finish(internetGatewayId?: string) {
    if (onClose) {
      onClose(internetGatewayId)

      return
    }

    await (internetGatewayId
      ? navigate({
          to: "/ec2/describe-internet-gateways/$id",
          params: { id: internetGatewayId },
        })
      : navigate({ to: "/ec2/describe-internet-gateways" }))
  }

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
    const result = await createMutation.mutateAsync({
      name: data.name?.trim() ?? undefined,
    })

    await finish(result.InternetGateway?.InternetGatewayId)
  }

  return (
    <>
      {!onClose && (
        <>
          <BackLink to="/ec2/describe-internet-gateways">
            Back to Internet Gateways
          </BackLink>
          <PageHeading title="Create Internet Gateway" />
        </>
      )}

      {createMutation.error && (
        <ErrorBanner
          error={createMutation.error}
          msg="Failed to create Internet Gateway"
        />
      )}

      <form className="max-w-4xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
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
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={async () => {
            await finish()
          }}
          pendingLabel="Creating…"
          submitLabel="Create"
        />
      </form>
    </>
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

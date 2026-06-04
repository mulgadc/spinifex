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
import { Field, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { useCreateInternetGateway } from "@/mutations/ec2"
import {
  type CreateInternetGatewayFormData,
  createInternetGatewaySchema,
} from "@/types/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(internet-gateways)/create-internet-gateway",
)({
  head: () => ({
    meta: [
      {
        title: "Create Internet Gateway | EC2 | Mulga",
      },
    ],
  }),
  component: CreateInternetGateway,
})

function CreateInternetGateway() {
  const navigate = useNavigate()
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
    const internetGatewayId = result.InternetGateway?.InternetGatewayId
    if (internetGatewayId) {
      navigate({
        to: "/ec2/describe-internet-gateways/$id",
        params: { id: internetGatewayId },
      })
    } else {
      navigate({ to: "/ec2/describe-internet-gateways" })
    }
  }

  return (
    <>
      <BackLink to="/ec2/describe-internet-gateways">
        Back to Internet Gateways
      </BackLink>

      <PageHeading title="Create Internet Gateway" />

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
          onCancel={async () =>
            await navigate({ to: "/ec2/describe-internet-gateways" })
          }
          pendingLabel="Creating…"
          submitLabel="Create"
        />
      </form>
    </>
  )
}

function buildCreateInternetGatewayCommands(name: string): CliCommand[] {
  const parts: CliCommand["parts"] = [
    {
      type: "bin",
      value: "AWS_PROFILE=spinifex aws ec2 create-internet-gateway",
    },
  ]

  if (name.trim()) {
    parts.push(
      { type: "flag" as const, value: " \\\n  --tag-specifications" },
      {
        type: "value" as const,
        value: ` 'ResourceType=internet-gateway,Tags=[{Key=Name,Value=${name.trim()}}]'`,
      },
    )
  }

  return [{ label: "Create Internet Gateway", parts }]
}

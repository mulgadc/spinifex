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
import { useAllocateAddress } from "@/mutations/ec2"
import {
  type AllocateAddressFormData,
  allocateAddressSchema,
} from "@/types/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(elastic-ips)/allocate-address",
)({
  head: () => ({
    meta: [
      {
        title: "Allocate Elastic IP | EC2 | Mulga",
      },
    ],
  }),
  component: AllocateAddress,
})

function AllocateAddress() {
  const navigate = useNavigate()
  const allocateMutation = useAllocateAddress()

  const {
    control,
    handleSubmit,
    register,
    formState: { isSubmitting },
  } = useForm<AllocateAddressFormData>({
    resolver: zodResolver(allocateAddressSchema),
    defaultValues: { name: "" },
  })

  const values = useWatch({ control })

  const onSubmit = async (data: AllocateAddressFormData) => {
    const result = await allocateMutation.mutateAsync({
      name: data.name?.trim() ?? undefined,
    })
    const allocationId = result.AllocationId
    if (allocationId) {
      navigate({
        to: "/ec2/describe-addresses/$id",
        params: { id: allocationId },
      })
    } else {
      navigate({ to: "/ec2/describe-addresses" })
    }
  }

  return (
    <>
      <BackLink to="/ec2/describe-addresses">Back to Elastic IPs</BackLink>

      <PageHeading title="Allocate Elastic IP" />

      {allocateMutation.error && (
        <ErrorBanner
          error={allocateMutation.error}
          msg="Failed to allocate Elastic IP"
        />
      )}

      <form className="max-w-4xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
        <Field>
          <FieldTitle>
            <label htmlFor="name">Name (optional)</label>
          </FieldTitle>
          <Input id="name" placeholder="my-elastic-ip" {...register("name")} />
        </Field>

        <CliCommandPanel
          commands={buildAllocateAddressCommands(values.name ?? "")}
        />

        <FormActions
          isPending={allocateMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={async () =>
            await navigate({ to: "/ec2/describe-addresses" })
          }
          pendingLabel="Allocating…"
          submitLabel="Allocate"
        />
      </form>
    </>
  )
}

function buildAllocateAddressCommands(name: string): CliCommand[] {
  const parts = [
    {
      type: "bin" as const,
      value: "AWS_PROFILE=spinifex aws ec2 allocate-address",
    },
    { type: "flag" as const, value: " \\\n  --domain" },
    { type: "value" as const, value: " vpc" },
  ]

  if (name.trim()) {
    parts.push(
      { type: "flag" as const, value: " \\\n  --tag-specifications" },
      {
        type: "value" as const,
        value: ` 'ResourceType=elastic-ip,Tags=[{Key=Name,Value=${name.trim()}}]'`,
      },
    )
  }

  return [{ label: "Allocate Elastic IP", parts }]
}

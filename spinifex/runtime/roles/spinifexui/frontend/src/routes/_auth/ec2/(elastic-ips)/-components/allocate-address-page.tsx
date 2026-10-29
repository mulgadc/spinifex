import { zodResolver } from "@hookform/resolvers/zod"
import { useNavigate } from "@tanstack/react-router"
import { useForm, useWatch } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import {
  CliCommandPanel,
  type CliCommand,
  commandFlag,
  optionalFlag,
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

interface AllocateAddressPageProps {
  // Given when the form sits in the list's side panel: called with the new
  // allocation's ID once allocated, or with nothing on cancel.
  onClose?: (allocationId?: string) => void
}

export function AllocateAddressPage({ onClose }: AllocateAddressPageProps) {
  const navigate = useNavigate()

  async function finish(allocationId?: string) {
    if (onClose) {
      onClose(allocationId)

      return
    }

    await (allocationId
      ? navigate({
          to: "/ec2/describe-addresses/$id",
          params: { id: allocationId },
        })
      : navigate({ to: "/ec2/describe-addresses" }))
  }

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

    await finish(result.AllocationId)
  }

  return (
    <>
      {!onClose && (
        <>
          <BackLink to="/ec2/describe-addresses">Back to Elastic IPs</BackLink>
          <PageHeading title="Allocate Elastic IP" />
        </>
      )}

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
          onCancel={async () => {
            await finish()
          }}
          pendingLabel="Allocating…"
          submitLabel="Allocate"
        />
      </form>
    </>
  )
}

function buildAllocateAddressCommands(name: string): CliCommand[] {
  const tagName = name.trim()

  return [
    {
      label: "Allocate Elastic IP",
      parts: [
        {
          type: "bin",
          value: "AWS_PROFILE=spinifex aws ec2 allocate-address",
        },
        ...commandFlag("\\\n  --domain", "vpc"),
        ...optionalFlag(
          "\\\n  --tag-specifications",
          tagName &&
            `'ResourceType=elastic-ip,Tags=[{Key=Name,Value=${tagName}}]'`,
        ),
      ],
    },
  ]
}

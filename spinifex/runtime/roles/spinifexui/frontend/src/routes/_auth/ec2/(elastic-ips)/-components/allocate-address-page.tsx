import { zodResolver } from "@hookform/resolvers/zod"
import { useForm, useWatch } from "react-hook-form"

import {
  CliCommandPanel,
  type CliCommand,
  commandFlag,
  optionalFlag,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { Field, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { useAllocateAddress } from "@/mutations/ec2"
import {
  type AllocateAddressFormData,
  allocateAddressSchema,
} from "@/types/ec2"

interface AllocateAddressPageProps {
  // Called with the new resource's ID once created, or with nothing on cancel.
  onClose: (allocationId?: string) => void
}

export function AllocateAddressPage({ onClose }: AllocateAddressPageProps) {
  const finish = onClose

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
    let result

    try {
      result = await allocateMutation.mutateAsync({
        name: data.name?.trim() ?? undefined,
      })
    } catch {
      // Surfaced beside the submit button through the mutation's error.
      return
    }

    finish(result.AllocationId)
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={allocateMutation.isPending || isSubmitting}
      >
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
          error={allocateMutation.error}
          errorMsg="Failed to allocate Elastic IP"
          isPending={allocateMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            finish()
          }}
          pendingLabel="Allocating…"
          submitLabel="Allocate"
        />
      </FormLock>
    </form>
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

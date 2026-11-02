import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import {
  type DeepPartialSkipArrayKey,
  useForm,
  useWatch,
} from "react-hook-form"

import {
  CliCommandPanel,
  type CliCommand,
  cliPlaceholder,
  commandFlag,
  optionalFlag,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { TextField } from "@/components/form-fields"
import { FormLock } from "@/components/form-lock"
import { VpcSelectField } from "@/components/vpc-select-field"
import { useCreateSecurityGroup } from "@/mutations/ec2"
import { ec2VpcsQueryOptions } from "@/queries/ec2"
import {
  type CreateSecurityGroupFormData,
  createSecurityGroupSchema,
} from "@/types/ec2"

interface CreateSecurityGroupPageProps {
  // Called with the new resource's ID once created, or with nothing on cancel.
  onClose: (id?: string) => void
}

export function CreateSecurityGroupPage({
  onClose,
}: CreateSecurityGroupPageProps) {
  const { data: vpcsData } = useSuspenseQuery(ec2VpcsQueryOptions)
  const createMutation = useCreateSecurityGroup()

  const vpcs = vpcsData.Vpcs ?? []

  const {
    control,
    handleSubmit,
    register,
    formState: { errors, isSubmitting },
  } = useForm<CreateSecurityGroupFormData>({
    resolver: zodResolver(createSecurityGroupSchema),
    defaultValues: {
      groupName: "",
      description: "",
      vpcId: vpcs[0]?.VpcId ?? "",
    },
  })

  const values = useWatch({ control })

  const onSubmit = async (data: CreateSecurityGroupFormData) => {
    let result

    try {
      result = await createMutation.mutateAsync(data)
    } catch {
      // Surfaced beside the submit button through the mutation's error.
      return
    }

    onClose(result.GroupId)
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={createMutation.isPending || isSubmitting}
      >
        <TextField
          error={errors.groupName}
          id="groupName"
          label="Group Name"
          placeholder="my-security-group"
          {...register("groupName")}
        />

        <TextField
          error={errors.description}
          id="description"
          label="Description"
          placeholder="My security group description"
          {...register("description")}
        />

        <VpcSelectField control={control} id="vpcId" name="vpcId" vpcs={vpcs} />

        <CliCommandPanel commands={buildCreateSecurityGroupCommands(values)} />

        <FormActions
          error={createMutation.error}
          errorMsg="Failed to create security group"
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            onClose()
          }}
          pendingLabel="Creating…"
          submitLabel="Create Security Group"
        />
      </FormLock>
    </form>
  )
}

function buildCreateSecurityGroupCommands(
  values: DeepPartialSkipArrayKey<CreateSecurityGroupFormData>,
): CliCommand[] {
  const description = cliPlaceholder(values.description ?? "", "Description")

  return [
    {
      label: "Create Security Group",
      parts: [
        {
          type: "bin",
          value: "AWS_PROFILE=spinifex aws ec2 create-security-group",
        },
        ...commandFlag(
          "\\\n  --group-name",
          cliPlaceholder(values.groupName ?? "", "GroupName"),
        ),
        ...commandFlag("\\\n  --description", `"${description}"`),
        ...optionalFlag("\\\n  --vpc-id", values.vpcId),
      ],
    },
  ]
}

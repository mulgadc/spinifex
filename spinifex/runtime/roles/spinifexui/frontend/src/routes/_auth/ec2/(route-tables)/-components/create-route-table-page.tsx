import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
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
import { TextField } from "@/components/form-fields"
import { FormLock } from "@/components/form-lock"
import { VpcSelectField } from "@/components/vpc-select-field"
import { useCreateRouteTable } from "@/mutations/ec2"
import { ec2VpcsQueryOptions } from "@/queries/ec2"
import {
  type CreateRouteTableFormData,
  createRouteTableSchema,
} from "@/types/ec2"

interface CreateRouteTablePageProps {
  // Called with the new resource's ID once created, or with nothing on cancel.
  onClose: (id?: string) => void
}

export function CreateRouteTablePage({ onClose }: CreateRouteTablePageProps) {
  const { data: vpcsData } = useSuspenseQuery(ec2VpcsQueryOptions)
  const createMutation = useCreateRouteTable()

  const vpcs = vpcsData.Vpcs ?? []

  const {
    control,
    handleSubmit,
    register,
    formState: { isSubmitting },
  } = useForm<CreateRouteTableFormData>({
    resolver: zodResolver(createRouteTableSchema),
    defaultValues: {
      vpcId: "",
      name: "",
    },
  })

  const values = useWatch({ control })

  const onSubmit = async (data: CreateRouteTableFormData) => {
    let result

    try {
      result = await createMutation.mutateAsync({
        vpcId: data.vpcId,
        name: data.name?.trim() ? data.name.trim() : undefined,
      })
    } catch {
      // Surfaced beside the submit button through the mutation's error.
      return
    }

    onClose(result.RouteTable?.RouteTableId)
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
          placeholder="my-route-table"
          {...register("name")}
        />

        <VpcSelectField
          control={control}
          id="vpcId"
          name="vpcId"
          placeholder="Select a VPC"
          vpcs={vpcs}
        />

        <CliCommandPanel
          commands={buildCreateRouteTableCommands(
            values.vpcId ?? "",
            values.name ?? "",
          )}
        />

        <FormActions
          error={createMutation.error}
          errorMsg="Failed to create Route Table"
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            onClose()
          }}
          pendingLabel="Creating…"
          submitLabel="Create Route Table"
        />
      </FormLock>
    </form>
  )
}

function buildCreateRouteTableCommands(
  vpcId: string,
  name: string,
): CliCommand[] {
  const tagName = name.trim()

  return [
    {
      label: "Create Route Table",
      parts: [
        {
          type: "bin",
          value: "AWS_PROFILE=spinifex aws ec2 create-route-table",
        },
        ...commandFlag("\\\n  --vpc-id", cliPlaceholder(vpcId, "vpc-id")),
        ...optionalFlag(
          "\\\n  --tag-specifications",
          tagName &&
            shellSingleQuote(
              `ResourceType=route-table,Tags=[{Key=Name,Value=${tagName}}]`,
            ),
        ),
      ],
    },
  ]
}

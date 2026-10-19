import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import {
  type DeepPartialSkipArrayKey,
  useForm,
  useWatch,
} from "react-hook-form"

import { BackLink } from "@/components/back-link"
import {
  CliCommandPanel,
  type CliCommand,
} from "@/components/cli-command-panel"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { TextField } from "@/components/form-fields"
import { PageHeading } from "@/components/page-heading"
import { VpcSelectField } from "@/components/vpc-select-field"
import { useCreateSecurityGroup } from "@/mutations/ec2"
import { ec2VpcsQueryOptions } from "@/queries/ec2"
import {
  type CreateSecurityGroupFormData,
  createSecurityGroupSchema,
} from "@/types/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(security-groups)/create-security-group",
)({
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ec2VpcsQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Create Security Group | EC2 | Mulga",
      },
    ],
  }),
  component: CreateSecurityGroup,
})

function CreateSecurityGroup() {
  const navigate = useNavigate()
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
    const result = await createMutation.mutateAsync(data)
    const groupId = result.GroupId

    if (groupId) {
      navigate({
        to: "/ec2/describe-security-groups/$id",
        params: { id: groupId },
      })
    } else {
      navigate({ to: "/ec2/describe-security-groups" })
    }
  }

  return (
    <>
      <BackLink to="/ec2/describe-security-groups">
        Back to Security Groups
      </BackLink>

      <PageHeading title="Create Security Group" />

      {createMutation.error && (
        <ErrorBanner
          error={createMutation.error}
          msg="Failed to create security group"
        />
      )}

      <form className="max-w-4xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
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
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={async () => {
            await navigate({ to: "/ec2/describe-security-groups" })
          }}
          pendingLabel="Creating…"
          submitLabel="Create Security Group"
        />
      </form>
    </>
  )
}

function buildCreateSecurityGroupCommands(
  values: DeepPartialSkipArrayKey<CreateSecurityGroupFormData>,
): CliCommand[] {
  const name = values.groupName ?? ""
  const desc = values.description ?? ""
  const vpcId = values.vpcId ?? ""

  const parts = [
    {
      type: "bin" as const,
      value: "AWS_PROFILE=spinifex aws ec2 create-security-group",
    },
    { type: "flag" as const, value: " \\\n  --group-name" },
    { type: "value" as const, value: ` ${name || "<GroupName>"}` },
    { type: "flag" as const, value: " \\\n  --description" },
    { type: "value" as const, value: ` "${desc || "<Description>"}"` },
  ]

  if (vpcId) {
    parts.push(
      { type: "flag" as const, value: " \\\n  --vpc-id" },
      { type: "value" as const, value: ` ${vpcId}` },
    )
  }

  return [{ label: "Create Security Group", parts }]
}

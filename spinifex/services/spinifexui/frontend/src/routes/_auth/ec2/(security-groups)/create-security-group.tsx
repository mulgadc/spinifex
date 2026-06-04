import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import { Controller, useForm, useWatch } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import {
  CliCommandPanel,
  type CliCommand,
} from "@/components/cli-command-panel"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { PageHeading } from "@/components/page-heading"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { getNameTag } from "@/lib/utils"
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
    await context.queryClient.ensureQueryData(ec2VpcsQueryOptions)
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
  const cliWatch = (name?: string): unknown =>
    name ? (values as Record<string, unknown>)[name] : undefined

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
        <Field>
          <FieldTitle>
            <label htmlFor="groupName">Group Name</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.groupName}
            id="groupName"
            placeholder="my-security-group"
            {...register("groupName")}
          />
          <FieldError errors={[errors.groupName]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="description">Description</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.description}
            id="description"
            placeholder="My security group description"
            {...register("description")}
          />
          <FieldError errors={[errors.description]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="vpcId">VPC</label>
          </FieldTitle>
          <Controller
            control={control}
            name="vpcId"
            render={({ field }) => (
              <Select
                onValueChange={(value) => field.onChange(value)}
                value={field.value ?? ""}
              >
                <SelectTrigger
                  aria-invalid={!!errors.vpcId}
                  className="w-full"
                  id="vpcId"
                >
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {vpcs.map((vpc) => {
                    const name = getNameTag(vpc.Tags)
                    return (
                      <SelectItem key={vpc.VpcId} value={vpc.VpcId ?? ""}>
                        {name
                          ? `${vpc.VpcId} (${name})`
                          : `${vpc.VpcId} (${vpc.CidrBlock})`}
                      </SelectItem>
                    )
                  })}
                </SelectContent>
              </Select>
            )}
          />
          <FieldError errors={[errors.vpcId]} />
        </Field>

        <CliCommandPanel
          commands={buildCreateSecurityGroupCommands(cliWatch)}
        />

        <FormActions
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={async () =>
            await navigate({ to: "/ec2/describe-security-groups" })
          }
          pendingLabel="Creating…"
          submitLabel="Create Security Group"
        />
      </form>
    </>
  )
}

function buildCreateSecurityGroupCommands(
  watch: (name?: string) => unknown,
): CliCommand[] {
  const rawName = watch("groupName")
  const name = typeof rawName === "string" ? rawName : ""
  const rawDesc = watch("description")
  const desc = typeof rawDesc === "string" ? rawDesc : ""
  const rawVpcId = watch("vpcId")
  const vpcId = typeof rawVpcId === "string" ? rawVpcId : ""

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

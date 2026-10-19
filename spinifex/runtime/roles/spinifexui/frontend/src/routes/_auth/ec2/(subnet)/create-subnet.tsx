import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import {
  Controller,
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
import { Field, FieldTitle } from "@/components/ui/field"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { VpcSelectField } from "@/components/vpc-select-field"
import { useCreateSubnet } from "@/mutations/ec2"
import {
  ec2AvailabilityZonesQueryOptions,
  ec2VpcsQueryOptions,
} from "@/queries/ec2"
import { type CreateSubnetFormData, createSubnetSchema } from "@/types/ec2"

export const Route = createFileRoute("/_auth/ec2/(subnet)/create-subnet")({
  loader: async ({ context }) => {
    await Promise.all([
      context.queryClient.query({
        ...ec2VpcsQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2AvailabilityZonesQueryOptions,
        staleTime: "static",
      }),
    ])
  },
  head: () => ({
    meta: [
      {
        title: "Create Subnet | EC2 | Mulga",
      },
    ],
  }),
  component: CreateSubnet,
})

function CreateSubnet() {
  const navigate = useNavigate()
  const { data: vpcsData } = useSuspenseQuery(ec2VpcsQueryOptions)
  const { data: azsData } = useSuspenseQuery(ec2AvailabilityZonesQueryOptions)
  const createMutation = useCreateSubnet()

  const vpcs = vpcsData.Vpcs ?? []
  const azs = azsData.AvailabilityZones ?? []

  const {
    control,
    handleSubmit,
    register,
    formState: { errors, isSubmitting },
  } = useForm<CreateSubnetFormData>({
    resolver: zodResolver(createSubnetSchema),
    defaultValues: {
      vpcId: vpcs[0]?.VpcId ?? "",
      cidrBlock: "10.0.1.0/24",
      availabilityZone: undefined,
      mapPublicIpOnLaunch: false,
    },
  })

  const values = useWatch({ control })

  const onSubmit = async (data: CreateSubnetFormData) => {
    const result = await createMutation.mutateAsync(data)
    const subnetId = result.Subnet?.SubnetId

    if (subnetId) {
      navigate({
        to: "/ec2/describe-subnets/$id",
        params: { id: subnetId },
      })
    } else {
      navigate({ to: "/ec2/describe-subnets" })
    }
  }

  return (
    <>
      <BackLink to="/ec2/describe-subnets">Back to Subnets</BackLink>

      <PageHeading title="Create Subnet" />

      {createMutation.error && (
        <ErrorBanner
          error={createMutation.error}
          msg="Failed to create subnet"
        />
      )}

      <form className="max-w-4xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
        <VpcSelectField control={control} id="vpcId" name="vpcId" vpcs={vpcs} />

        <TextField
          error={errors.cidrBlock}
          id="cidrBlock"
          label="CIDR Block"
          placeholder="10.0.1.0/24"
          {...register("cidrBlock")}
        />

        <Field>
          <FieldTitle>
            <label htmlFor="availabilityZone">
              Availability Zone (optional)
            </label>
          </FieldTitle>
          <Controller
            control={control}
            name="availabilityZone"
            render={({ field }) => (
              <Select
                onValueChange={(value) => {
                  field.onChange(value === "none" ? undefined : value)
                }}
                value={field.value ?? "none"}
              >
                <SelectTrigger className="w-full" id="availabilityZone">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="none">none</SelectItem>
                  {azs.map((az) => (
                    <SelectItem key={az.ZoneName} value={az.ZoneName ?? ""}>
                      {az.ZoneName}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
          />
        </Field>

        <Field>
          <FieldTitle>Auto-assign public IPv4</FieldTitle>
          <Controller
            control={control}
            name="mapPublicIpOnLaunch"
            render={({ field }) => (
              <label className="flex items-center gap-2 text-sm">
                <input
                  aria-label="Enable auto-assign public IPv4 address"
                  checked={field.value ?? false}
                  onChange={(e) => {
                    field.onChange(e.target.checked)
                  }}
                  type="checkbox"
                />
                Enable auto-assign public IPv4 address
              </label>
            )}
          />
        </Field>

        <CliCommandPanel commands={buildCreateSubnetCommands(values)} />

        <FormActions
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={async () => {
            await navigate({ to: "/ec2/describe-subnets" })
          }}
          pendingLabel="Creating…"
          submitLabel="Create Subnet"
        />
      </form>
    </>
  )
}

function buildCreateSubnetCommands(
  values: DeepPartialSkipArrayKey<CreateSubnetFormData>,
): CliCommand[] {
  const vpcId = values.vpcId ?? ""
  const cidr = values.cidrBlock ?? ""
  const az = values.availabilityZone ?? ""

  const parts = [
    {
      type: "bin" as const,
      value: "AWS_PROFILE=spinifex aws ec2 create-subnet",
    },
    { type: "flag" as const, value: " \\\n  --vpc-id" },
    { type: "value" as const, value: ` ${vpcId || "<VpcId>"}` },
    { type: "flag" as const, value: " \\\n  --cidr-block" },
    { type: "value" as const, value: ` ${cidr || "<CidrBlock>"}` },
  ]

  if (az) {
    parts.push(
      { type: "flag" as const, value: " \\\n  --availability-zone" },
      { type: "value" as const, value: ` ${az}` },
    )
  }

  const commands: CliCommand[] = [{ label: "Create Subnet", parts }]

  if (values.mapPublicIpOnLaunch === true) {
    commands.push({
      label: "Enable auto-assign public IPv4",
      parts: [
        {
          type: "bin" as const,
          value: "AWS_PROFILE=spinifex aws ec2 modify-subnet-attribute",
        },
        { type: "flag" as const, value: " \\\n  --subnet-id" },
        { type: "value" as const, value: " <SubnetId>" },
        { type: "flag" as const, value: " \\\n  --map-public-ip-on-launch" },
      ],
    })
  }

  return commands
}

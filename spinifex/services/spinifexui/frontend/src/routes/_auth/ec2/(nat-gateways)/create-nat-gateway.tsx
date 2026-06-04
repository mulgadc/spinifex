import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router"
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
import { useCreateNatGateway } from "@/mutations/ec2"
import { ec2AddressesQueryOptions, ec2SubnetsQueryOptions } from "@/queries/ec2"
import {
  type CreateNatGatewayFormData,
  createNatGatewaySchema,
} from "@/types/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(nat-gateways)/create-nat-gateway",
)({
  loader: async ({ context }) => {
    await Promise.all([
      context.queryClient.ensureQueryData(ec2SubnetsQueryOptions),
      context.queryClient.ensureQueryData(ec2AddressesQueryOptions),
    ])
  },
  head: () => ({
    meta: [
      {
        title: "Create NAT Gateway | EC2 | Mulga",
      },
    ],
  }),
  component: CreateNatGateway,
})

function CreateNatGateway() {
  const navigate = useNavigate()
  const { data: subnetsData } = useSuspenseQuery(ec2SubnetsQueryOptions)
  const { data: addressesData } = useSuspenseQuery(ec2AddressesQueryOptions)
  const createMutation = useCreateNatGateway()

  const subnets = subnetsData.Subnets ?? []
  // A NAT gateway must live in a public subnet (one with auto-assigned public
  // IP). Fall back to all subnets if none are flagged public.
  const publicSubnets = subnets.filter((s) => s.MapPublicIpOnLaunch)
  const subnetOptions = publicSubnets.length > 0 ? publicSubnets : subnets
  // Only Elastic IPs that are not already associated can back a NAT gateway.
  const availableAddresses = (addressesData.Addresses ?? []).filter(
    (a) => !a.AssociationId,
  )

  const {
    control,
    handleSubmit,
    register,
    formState: { errors, isSubmitting },
  } = useForm<CreateNatGatewayFormData>({
    resolver: zodResolver(createNatGatewaySchema),
    defaultValues: {
      subnetId: "",
      allocationId: "",
      name: "",
    },
  })

  const values = useWatch({ control })
  const cliWatch = (name?: string): unknown =>
    name ? (values as Record<string, unknown>)[name] : undefined

  const onSubmit = async (data: CreateNatGatewayFormData) => {
    const result = await createMutation.mutateAsync({
      subnetId: data.subnetId,
      allocationId: data.allocationId,
      name: data.name?.trim() ?? undefined,
    })
    const natGatewayId = result.NatGateway?.NatGatewayId
    if (natGatewayId) {
      navigate({
        to: "/ec2/describe-nat-gateways/$id",
        params: { id: natGatewayId },
      })
    } else {
      navigate({ to: "/ec2/describe-nat-gateways" })
    }
  }

  return (
    <>
      <BackLink to="/ec2/describe-nat-gateways">Back to NAT Gateways</BackLink>

      <PageHeading title="Create NAT Gateway" />

      {createMutation.error && (
        <ErrorBanner
          error={createMutation.error}
          msg="Failed to create NAT Gateway"
        />
      )}

      <form className="max-w-4xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
        <Field>
          <FieldTitle>
            <label htmlFor="name">Name (optional)</label>
          </FieldTitle>
          <Input id="name" placeholder="my-nat-gateway" {...register("name")} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="subnetId">Public subnet</label>
          </FieldTitle>
          <Controller
            control={control}
            name="subnetId"
            render={({ field }) => (
              <Select
                onValueChange={(value) => field.onChange(value)}
                value={field.value ?? ""}
              >
                <SelectTrigger
                  aria-invalid={!!errors.subnetId}
                  className="w-full"
                  id="subnetId"
                >
                  <SelectValue placeholder="Select a public subnet" />
                </SelectTrigger>
                <SelectContent>
                  {subnetOptions.map((subnet) => {
                    const name = getNameTag(subnet.Tags)
                    return (
                      <SelectItem
                        key={subnet.SubnetId}
                        value={subnet.SubnetId ?? ""}
                      >
                        {name
                          ? `${subnet.SubnetId} (${name})`
                          : `${subnet.SubnetId} (${subnet.CidrBlock})`}
                      </SelectItem>
                    )
                  })}
                </SelectContent>
              </Select>
            )}
          />
          <FieldError errors={[errors.subnetId]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="allocationId">Elastic IP</label>
          </FieldTitle>
          {availableAddresses.length > 0 ? (
            <Controller
              control={control}
              name="allocationId"
              render={({ field }) => (
                <Select
                  onValueChange={(value) => field.onChange(value)}
                  value={field.value ?? ""}
                >
                  <SelectTrigger
                    aria-invalid={!!errors.allocationId}
                    className="w-full"
                    id="allocationId"
                  >
                    <SelectValue placeholder="Select an Elastic IP" />
                  </SelectTrigger>
                  <SelectContent>
                    {availableAddresses.map((addr) => (
                      <SelectItem
                        key={addr.AllocationId}
                        value={addr.AllocationId ?? ""}
                      >
                        {`${addr.AllocationId} (${addr.PublicIp})`}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              )}
            />
          ) : (
            <p className="text-sm text-muted-foreground">
              No available Elastic IPs.{" "}
              <Link
                className="text-primary hover:underline"
                to="/ec2/allocate-address"
              >
                Allocate one
              </Link>{" "}
              first.
            </p>
          )}
          <FieldError errors={[errors.allocationId]} />
        </Field>

        <CliCommandPanel commands={buildCreateNatGatewayCommands(cliWatch)} />

        <FormActions
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={async () =>
            await navigate({ to: "/ec2/describe-nat-gateways" })
          }
          pendingLabel="Creating…"
          submitLabel="Create NAT Gateway"
        />
      </form>
    </>
  )
}

function buildCreateNatGatewayCommands(
  watch: (name?: string) => unknown,
): CliCommand[] {
  const rawSubnetId = watch("subnetId")
  const subnetId = typeof rawSubnetId === "string" ? rawSubnetId : ""
  const rawAllocationId = watch("allocationId")
  const allocationId =
    typeof rawAllocationId === "string" ? rawAllocationId : ""
  const rawName = watch("name")
  const name = typeof rawName === "string" ? rawName : ""

  const parts = [
    {
      type: "bin" as const,
      value: "AWS_PROFILE=spinifex aws ec2 create-nat-gateway",
    },
    { type: "flag" as const, value: " \\\n  --subnet-id" },
    { type: "value" as const, value: ` ${subnetId || "<SubnetId>"}` },
    { type: "flag" as const, value: " \\\n  --allocation-id" },
    { type: "value" as const, value: ` ${allocationId || "<AllocationId>"}` },
  ]

  if (name.trim()) {
    parts.push(
      { type: "flag" as const, value: " \\\n  --tag-specifications" },
      {
        type: "value" as const,
        value: ` 'ResourceType=natgateway,Tags=[{Key=Name,Value=${name.trim()}}]'`,
      },
    )
  }

  return [{ label: "Create NAT Gateway", parts }]
}

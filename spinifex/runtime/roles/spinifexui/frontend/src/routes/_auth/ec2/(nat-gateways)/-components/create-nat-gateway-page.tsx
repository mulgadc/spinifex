import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import {
  Controller,
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
import { FormLock } from "@/components/form-lock"
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

interface CreateNatGatewayPageProps {
  // Called with the new resource's ID once created, or with nothing on cancel.
  onClose: (id?: string) => void
}

export function CreateNatGatewayPage({ onClose }: CreateNatGatewayPageProps) {
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

  const onSubmit = async (data: CreateNatGatewayFormData) => {
    let result

    try {
      result = await createMutation.mutateAsync({
        subnetId: data.subnetId,
        allocationId: data.allocationId,
        name: data.name?.trim() ?? undefined,
      })
    } catch {
      // Surfaced beside the submit button through the mutation's error.
      return
    }

    onClose(result.NatGateway?.NatGatewayId)
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={createMutation.isPending || isSubmitting}
      >
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
              <Select onValueChange={field.onChange} value={field.value ?? ""}>
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
                  onValueChange={field.onChange}
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
                search={{ create: true }}
                to="/ec2/describe-addresses"
              >
                Allocate one
              </Link>{" "}
              first.
            </p>
          )}
          <FieldError errors={[errors.allocationId]} />
        </Field>

        <CliCommandPanel commands={buildCreateNatGatewayCommands(values)} />

        <FormActions
          error={createMutation.error}
          errorMsg="Failed to create NAT Gateway"
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            onClose()
          }}
          pendingLabel="Creating…"
          submitLabel="Create NAT Gateway"
        />
      </FormLock>
    </form>
  )
}

function buildCreateNatGatewayCommands(
  values: DeepPartialSkipArrayKey<CreateNatGatewayFormData>,
): CliCommand[] {
  const tagName = values.name?.trim()

  return [
    {
      label: "Create NAT Gateway",
      parts: [
        {
          type: "bin",
          value: "AWS_PROFILE=spinifex aws ec2 create-nat-gateway",
        },
        ...commandFlag(
          "\\\n  --subnet-id",
          cliPlaceholder(values.subnetId ?? "", "SubnetId"),
        ),
        ...commandFlag(
          "\\\n  --allocation-id",
          cliPlaceholder(values.allocationId ?? "", "AllocationId"),
        ),
        ...optionalFlag(
          "\\\n  --tag-specifications",
          tagName &&
            `'ResourceType=natgateway,Tags=[{Key=Name,Value=${tagName}}]'`,
        ),
      ],
    },
  ]
}

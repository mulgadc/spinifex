import { zodResolver } from "@hookform/resolvers/zod"
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
  captureId,
  commandFlag,
  optionalFlag,
  variableFlag,
  type CommandPart,
} from "@/components/cli-command-panel"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { PageHeading } from "@/components/page-heading"
import { TagsFieldArray } from "@/components/tags-field-array"
import { Button } from "@/components/ui/button"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { SubnetCidrInputs } from "@/components/vpc-wizard/subnet-cidr-inputs"
import { VpcPreview } from "@/components/vpc-wizard/vpc-preview"
import { WizardFailureBanner } from "@/components/wizard-failure-banner"
import {
  calculateSubnetCidrs,
  resolveSubnetCidrs,
} from "@/lib/subnet-calculator"
import { useCreateVpc, useCreateVpcWizard } from "@/mutations/ec2"
import {
  type CreateVpcWizardFormData,
  createVpcWizardSchema,
} from "@/types/ec2"

export const Route = createFileRoute("/_auth/ec2/(vpc)/create-vpc")({
  head: () => ({
    meta: [
      {
        title: "Create VPC | EC2 | Mulga",
      },
    ],
  }),
  component: CreateVpc,
})

const PUBLIC_SUBNET_COUNTS = [0, 1] as const
const PRIVATE_SUBNET_COUNTS = [0, 1, 2] as const

function CreateVpc() {
  const navigate = useNavigate()
  const createVpcMutation = useCreateVpc()
  const wizardMutation = useCreateVpcWizard()

  const form = useForm<CreateVpcWizardFormData>({
    resolver: zodResolver(createVpcWizardSchema),
    defaultValues: {
      mode: "vpc-only",
      namePrefix: "",
      autoGenerateNames: true,
      cidrBlock: "10.0.0.0/16",
      tenancy: "default",
      natGateway: "none",
      publicSubnetCount: 1,
      privateSubnetCount: 1,
      publicSubnetCidrs: [],
      privateSubnetCidrs: [],
      tags: [],
    },
  })

  const {
    handleSubmit,
    register,
    control,
    formState: { errors, isSubmitting },
  } = form

  const values = useWatch({ control })
  const mode = useWatch({ control, name: "mode" })
  const cidrBlock = useWatch({ control, name: "cidrBlock" })
  const publicSubnetCount = useWatch({ control, name: "publicSubnetCount" })
  const privateSubnetCount = useWatch({ control, name: "privateSubnetCount" })
  const publicSubnetCidrs = useWatch({ control, name: "publicSubnetCidrs" })
  const privateSubnetCidrs = useWatch({ control, name: "privateSubnetCidrs" })

  // Defaults are the input placeholders; the preview and CLI show what will be
  // created, custom CIDRs included.
  const vpcCidr = cidrBlock || "10.0.0.0/16"
  const shownPublicCount = mode === "vpc-and-more" ? publicSubnetCount : 0
  const shownPrivateCount = mode === "vpc-and-more" ? privateSubnetCount : 0

  const defaultCidrs = calculateSubnetCidrs(
    vpcCidr,
    shownPublicCount,
    shownPrivateCount,
  )

  const subnetCidrs = resolveSubnetCidrs(
    vpcCidr,
    shownPublicCount,
    shownPrivateCount,
    { publicCidrs: publicSubnetCidrs, privateCidrs: privateSubnetCidrs },
  )

  const onSubmit = async (data: CreateVpcWizardFormData) => {
    wizardMutation.reset()

    if (data.mode === "vpc-only") {
      const name =
        data.autoGenerateNames && data.namePrefix
          ? `${data.namePrefix}-vpc`
          : data.namePrefix

      const result = await createVpcMutation.mutateAsync({
        cidrBlock: data.cidrBlock,
        name: name || undefined,
        tags: data.tags,
      })

      const vpcId = result.Vpc?.VpcId

      if (vpcId) {
        navigate({ to: "/ec2/describe-vpcs/$id", params: { id: vpcId } })
      } else {
        navigate({ to: "/ec2/describe-vpcs" })
      }

      return
    }

    // VPC and more mode
    const result = await wizardMutation.mutateAsync(data)

    if (!result.error && result.vpcId) {
      navigate({
        to: "/ec2/describe-vpcs/$id",
        params: { id: result.vpcId },
      })
    }
  }

  const mutationError = createVpcMutation.error ?? wizardMutation.error
  const isPending = createVpcMutation.isPending || wizardMutation.isPending

  return (
    <>
      <BackLink to="/ec2/describe-vpcs">Back to VPCs</BackLink>

      <PageHeading title="Create VPC" />

      {mutationError && (
        <ErrorBanner error={mutationError} msg="Failed to create VPC" />
      )}

      <WizardFailureBanner result={wizardMutation.data} />

      {wizardMutation.isPending && (
        <div className="mb-4 max-w-4xl text-sm text-muted-foreground">
          Creating VPC and resources...
        </div>
      )}

      <div className="flex max-w-6xl gap-8">
        {/* Left column: Form */}
        <form
          className="min-w-0 flex-1 space-y-6"
          onSubmit={handleSubmit(onSubmit)}
        >
          {/* Section 1: Resources to create */}
          <Field>
            <FieldTitle>Resources to create</FieldTitle>
            <div className="flex gap-4">
              <label className="flex items-center gap-2 text-xs">
                <input
                  aria-label="VPC only"
                  type="radio"
                  value="vpc-only"
                  {...register("mode")}
                />
                VPC only
              </label>
              <label className="flex items-center gap-2 text-xs">
                <input
                  aria-label="VPC and more"
                  type="radio"
                  value="vpc-and-more"
                  {...register("mode")}
                />
                VPC and more
              </label>
            </div>
          </Field>

          {/* Section 2: Name tag auto-generation */}
          <Field>
            <FieldTitle>
              <label htmlFor="namePrefix">Name tag auto-generation</label>
            </FieldTitle>
            <div className="flex items-center gap-3">
              <Input
                id="namePrefix"
                placeholder="project"
                {...register("namePrefix")}
              />
              <label className="flex shrink-0 items-center gap-2 text-xs">
                <input
                  aria-label="Auto-generate name tags"
                  type="checkbox"
                  {...register("autoGenerateNames")}
                />
                Auto-generate
              </label>
            </div>
          </Field>

          {/* Section 3: CIDR Block */}
          <Field>
            <FieldTitle>
              <label htmlFor="cidrBlock">IPv4 CIDR block</label>
            </FieldTitle>
            <Input
              aria-invalid={!!errors.cidrBlock}
              id="cidrBlock"
              placeholder="10.0.0.0/16"
              {...register("cidrBlock")}
            />
            <FieldError errors={[errors.cidrBlock]} />
          </Field>

          {/* Section 4: Tenancy */}
          <Field>
            <FieldTitle>Tenancy</FieldTitle>
            <Controller
              control={control}
              name="tenancy"
              render={({ field }) => (
                <Select onValueChange={field.onChange} value={field.value}>
                  <SelectTrigger id="tenancy">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="default">default</SelectItem>
                    <SelectItem value="dedicated">dedicated</SelectItem>
                  </SelectContent>
                </Select>
              )}
            />
          </Field>

          <TagsFieldArray control={control} name="tags" />

          {/* VPC and more sections */}
          {mode === "vpc-and-more" && (
            <>
              {/* Section 5: Public Subnets */}
              <Field>
                <FieldTitle>Number of public subnets</FieldTitle>
                <Controller
                  control={control}
                  name="publicSubnetCount"
                  render={({ field }) => (
                    <div className="flex gap-1">
                      {PUBLIC_SUBNET_COUNTS.map((n) => (
                        <Button
                          key={n}
                          onClick={() => {
                            field.onChange(n)
                          }}
                          size="sm"
                          type="button"
                          variant={field.value === n ? "default" : "outline"}
                        >
                          {n}
                        </Button>
                      ))}
                    </div>
                  )}
                />
                <SubnetCidrInputs
                  count={publicSubnetCount}
                  defaults={defaultCidrs.publicSubnets.map((s) => s.cidr)}
                  form={form}
                  type="public"
                />
              </Field>

              {/* Section 6: Private Subnets */}
              <Field>
                <FieldTitle>Number of private subnets</FieldTitle>
                <Controller
                  control={control}
                  name="privateSubnetCount"
                  render={({ field }) => (
                    <div className="flex gap-1">
                      {PRIVATE_SUBNET_COUNTS.map((n) => (
                        <Button
                          key={n}
                          onClick={() => {
                            field.onChange(n)
                          }}
                          size="sm"
                          type="button"
                          variant={field.value === n ? "default" : "outline"}
                        >
                          {n}
                        </Button>
                      ))}
                    </div>
                  )}
                />
                <SubnetCidrInputs
                  count={privateSubnetCount}
                  defaults={defaultCidrs.privateSubnets.map((s) => s.cidr)}
                  form={form}
                  type="private"
                />
              </Field>

              {/* Section 7: NAT gateway (needs both a public and private subnet) */}
              {publicSubnetCount > 0 && privateSubnetCount > 0 && (
                <Field>
                  <FieldTitle>NAT gateways</FieldTitle>
                  <Controller
                    control={control}
                    name="natGateway"
                    render={({ field }) => (
                      <Select
                        onValueChange={field.onChange}
                        value={field.value ?? "none"}
                      >
                        <SelectTrigger id="natGateway">
                          <SelectValue />
                        </SelectTrigger>
                        <SelectContent>
                          <SelectItem value="none">None</SelectItem>
                          <SelectItem value="single">In 1 AZ</SelectItem>
                        </SelectContent>
                      </Select>
                    )}
                  />
                  <p className="mt-1 text-xs text-muted-foreground">
                    Creates an Elastic IP + NAT gateway in the public subnet and
                    routes private subnet egress (0.0.0.0/0) through it.
                  </p>
                </Field>
              )}
            </>
          )}

          <CliCommandPanel
            commands={buildCreateVpcCommands(values, subnetCidrs)}
          />

          <FormActions
            isPending={isPending}
            isSubmitting={isSubmitting}
            onCancel={async () => {
              await navigate({ to: "/ec2/describe-vpcs" })
            }}
            pendingLabel="Creating…"
            submitLabel="Create VPC"
          />
        </form>

        {/* Right column: Preview */}
        <div className="hidden min-w-0 flex-1 lg:block">
          <VpcPreview
            hasIgw={mode === "vpc-and-more" && publicSubnetCount > 0}
            mode={mode}
            privateSubnets={subnetCidrs.privateSubnets}
            publicSubnets={subnetCidrs.publicSubnets}
            vpcCidr={vpcCidr}
          />
        </div>
      </div>
    </>
  )
}

function buildCreateVpcCommands(
  values: DeepPartialSkipArrayKey<CreateVpcWizardFormData>,
  subnetCidrs: {
    publicSubnets: { cidr: string }[]
    privateSubnets: { cidr: string }[]
  },
): CliCommand[] {
  const cidr = values.cidrBlock || "10.0.0.0/16"

  if (values.mode === "vpc-only") {
    const { tenancy } = values

    return [
      {
        label: "Create VPC",
        parts: [
          { type: "bin", value: "AWS_PROFILE=spinifex aws ec2 create-vpc" },
          ...commandFlag("\\\n  --cidr-block", cidr),
          ...optionalFlag(
            "\\\n  --instance-tenancy",
            tenancy !== "default" && tenancy,
          ),
        ],
      },
    ]
  }

  // VPC-and-more: pasteable bash script
  const commands: CliCommand[] = []

  const allSubnets = [
    ...subnetCidrs.publicSubnets.map((s, i) => ({
      ...s,
      name: `PUBLIC_SUBNET_${i + 1}`,
    })),
    ...subnetCidrs.privateSubnets.map((s, i) => ({
      ...s,
      name: `PRIVATE_SUBNET_${i + 1}`,
    })),
  ]

  commands.push({
    label: "Create VPC",
    parts: [
      {
        type: "comment",
        value: "# Create VPC with subnets, internet gateway, and routing\n\n",
      },
      ...captureId(
        "VPC_ID",
        "AWS_PROFILE=spinifex aws ec2 create-vpc",
        commandFlag("\\\n  --cidr-block", cidr),
        "Vpc.VpcId",
      ),
    ],
  })

  for (const subnet of allSubnets) {
    commands.push({
      label: `Create Subnet (${subnet.name})`,
      parts: captureId(
        `${subnet.name}_ID`,
        "AWS_PROFILE=spinifex aws ec2 create-subnet",
        [
          ...variableFlag("\\\n  --vpc-id", "VPC_ID"),
          ...commandFlag("\\\n  --cidr-block", subnet.cidr),
        ],
        "Subnet.SubnetId",
      ),
    })
  }

  // Internet gateway (only if public subnets)
  if (subnetCidrs.publicSubnets.length > 0) {
    commands.push(
      {
        label: "Create Internet Gateway",
        parts: captureId(
          "IGW_ID",
          "AWS_PROFILE=spinifex aws ec2 create-internet-gateway",
          [],
          "InternetGateway.InternetGatewayId",
        ),
      },
      {
        label: "Attach Internet Gateway",
        parts: [
          {
            type: "bin",
            value: "AWS_PROFILE=spinifex aws ec2 attach-internet-gateway",
          },
          ...variableFlag("\\\n  --internet-gateway-id", "IGW_ID"),
          ...variableFlag("\\\n  --vpc-id", "VPC_ID"),
        ],
      },
      ...routeTableCommands(
        "",
        "RT_ID",
        variableFlag("\\\n  --gateway-id", "IGW_ID"),
      ),
    )

    // Associate route table with each public subnet
    for (let i = 0; i < subnetCidrs.publicSubnets.length; i++) {
      commands.push(
        associateRouteTableCommand(
          `Associate Route Table (PUBLIC_SUBNET_${i + 1})`,
          "RT_ID",
          `PUBLIC_SUBNET_${i + 1}_ID`,
        ),
      )
    }
  }

  // NAT gateway for private egress (needs both a public and a private subnet)
  if (
    values.natGateway === "single" &&
    subnetCidrs.publicSubnets.length > 0 &&
    subnetCidrs.privateSubnets.length > 0
  ) {
    commands.push(
      {
        label: "Allocate Elastic IP",
        parts: captureId(
          "EIP_ALLOC_ID",
          "AWS_PROFILE=spinifex aws ec2 allocate-address",
          commandFlag("\\\n  --domain", "vpc"),
          "AllocationId",
        ),
      },
      {
        label: "Create NAT Gateway",
        parts: captureId(
          "NAT_ID",
          "AWS_PROFILE=spinifex aws ec2 create-nat-gateway",
          [
            ...variableFlag("\\\n  --subnet-id", "PUBLIC_SUBNET_1_ID"),
            ...variableFlag("\\\n  --allocation-id", "EIP_ALLOC_ID"),
          ],
          "NatGateway.NatGatewayId",
        ),
      },
      ...routeTableCommands(
        "Private ",
        "PRIV_RT_ID",
        variableFlag("\\\n  --nat-gateway-id", "NAT_ID"),
      ),
    )

    for (let i = 0; i < subnetCidrs.privateSubnets.length; i++) {
      commands.push(
        associateRouteTableCommand(
          `Associate Private Route Table (PRIVATE_SUBNET_${i + 1})`,
          "PRIV_RT_ID",
          `PRIVATE_SUBNET_${i + 1}_ID`,
        ),
      )
    }
  }

  return commands
}

// Creates a route table in $VPC_ID whose default route goes to `target`.
function routeTableCommands(
  labelPrefix: string,
  tableVar: string,
  target: CommandPart[],
): CliCommand[] {
  return [
    {
      label: `Create ${labelPrefix}Route Table`,
      parts: captureId(
        tableVar,
        "AWS_PROFILE=spinifex aws ec2 create-route-table",
        variableFlag("\\\n  --vpc-id", "VPC_ID"),
        "RouteTable.RouteTableId",
      ),
    },
    {
      label: `Create ${labelPrefix}Default Route`,
      parts: [
        { type: "bin", value: "AWS_PROFILE=spinifex aws ec2 create-route" },
        ...variableFlag("\\\n  --route-table-id", tableVar),
        ...commandFlag("\\\n  --destination-cidr-block", "0.0.0.0/0"),
        ...target,
      ],
    },
  ]
}

function associateRouteTableCommand(
  label: string,
  tableVar: string,
  subnetVar: string,
): CliCommand {
  return {
    label,
    parts: [
      {
        type: "bin",
        value: "AWS_PROFILE=spinifex aws ec2 associate-route-table",
      },
      ...variableFlag("\\\n  --route-table-id", tableVar),
      ...variableFlag("\\\n  --subnet-id", subnetVar),
    ],
  }
}

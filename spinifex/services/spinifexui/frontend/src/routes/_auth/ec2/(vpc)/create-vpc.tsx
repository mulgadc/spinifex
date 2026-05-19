import { zodResolver } from "@hookform/resolvers/zod"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import { useState } from "react"
import { Controller, useForm } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import {
  CliCommandPanel,
  type CliCommand,
  type CommandPart,
} from "@/components/cli-command-panel"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { PageHeading } from "@/components/page-heading"
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
import { calculateSubnetCidrs } from "@/lib/subnet-calculator"
import {
  type WizardResult,
  useCreateVpc,
  useCreateVpcWizard,
} from "@/mutations/ec2"
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
  const [wizardResult, setWizardResult] = useState<WizardResult | null>(null)

  const form = useForm<CreateVpcWizardFormData>({
    resolver: zodResolver(createVpcWizardSchema),
    defaultValues: {
      mode: "vpc-only",
      namePrefix: "",
      autoGenerateNames: true,
      cidrBlock: "10.0.0.0/16",
      tenancy: "default",
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
    watch,
    control,
    formState: { errors, isSubmitting },
  } = form

  const mode = watch("mode")
  const cidrBlock = watch("cidrBlock")
  const publicSubnetCount = watch("publicSubnetCount")
  const privateSubnetCount = watch("privateSubnetCount")

  // Compute preview subnet CIDRs
  const subnetCidrs = calculateSubnetCidrs(
    cidrBlock || "10.0.0.0/16",
    mode === "vpc-and-more" ? publicSubnetCount : 0,
    mode === "vpc-and-more" ? privateSubnetCount : 0,
  )

  const [progressMessage, setProgressMessage] = useState("")

  const onSubmit = async (data: CreateVpcWizardFormData) => {
    setWizardResult(null)

    if (data.mode === "vpc-only") {
      const name =
        data.autoGenerateNames && data.namePrefix
          ? `${data.namePrefix}-vpc`
          : data.namePrefix
      const result = await createVpcMutation.mutateAsync({
        cidrBlock: data.cidrBlock,
        // oxlint-disable-next-line typescript/prefer-nullish-coalescing
        name: name || undefined,
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
    setProgressMessage("Creating VPC and resources...")
    const result = await wizardMutation.mutateAsync(data)
    setWizardResult(result)

    if (!result.error && result.vpcId) {
      navigate({
        to: "/ec2/describe-vpcs/$id",
        params: { id: result.vpcId },
      })
    }
    setProgressMessage("")
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

      {wizardResult?.error && (
        <div className="mb-6 max-w-4xl rounded-md border border-destructive bg-destructive/10 p-4">
          <h2 className="text-sm font-medium text-destructive">
            Wizard failed: {wizardResult.failedStep}
          </h2>
          <p className="mt-1 text-sm text-destructive">
            {wizardResult.error.message}
          </p>
          {wizardResult.created.length > 0 && (
            <div className="mt-3">
              <p className="text-xs font-medium text-destructive">
                Successfully created resources:
              </p>
              <ul className="mt-1 list-inside list-disc text-xs text-destructive">
                {wizardResult.created.map((r, i) => (
                  // oxlint-disable-next-line react/no-array-index-key -- error list with no stable id
                  <li key={i}>
                    {r.type}: {r.id}
                  </li>
                ))}
              </ul>
            </div>
          )}
        </div>
      )}

      {progressMessage && (
        <div className="mb-4 max-w-4xl text-sm text-muted-foreground">
          {progressMessage}
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
            <Controller
              control={control}
              name="mode"
              render={({ field }) => (
                <div className="flex gap-4">
                  <label className="flex items-center gap-2 text-xs">
                    <input
                      aria-label="VPC only"
                      checked={field.value === "vpc-only"}
                      name="mode"
                      onChange={() => field.onChange("vpc-only")}
                      type="radio"
                    />
                    VPC only
                  </label>
                  <label className="flex items-center gap-2 text-xs">
                    <input
                      aria-label="VPC and more"
                      checked={field.value === "vpc-and-more"}
                      name="mode"
                      onChange={() => field.onChange("vpc-and-more")}
                      type="radio"
                    />
                    VPC and more
                  </label>
                </div>
              )}
            />
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
              <Controller
                control={control}
                name="autoGenerateNames"
                render={({ field }) => (
                  <label className="flex shrink-0 items-center gap-2 text-xs">
                    <input
                      aria-label="Auto-generate name tags"
                      checked={field.value}
                      onChange={(e) => field.onChange(e.target.checked)}
                      type="checkbox"
                    />
                    Auto-generate
                  </label>
                )}
              />
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
                          onClick={() => field.onChange(n)}
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
                  defaults={subnetCidrs.publicSubnets.map((s) => s.cidr)}
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
                          onClick={() => field.onChange(n)}
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
                  defaults={subnetCidrs.privateSubnets.map((s) => s.cidr)}
                  form={form}
                  type="private"
                />
              </Field>
            </>
          )}

          <CliCommandPanel
            commands={buildCreateVpcCommands(watch, subnetCidrs)}
          />

          <FormActions
            isPending={isPending}
            isSubmitting={isSubmitting}
            onCancel={async () => await navigate({ to: "/ec2/describe-vpcs" })}
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
            vpcCidr={cidrBlock || "10.0.0.0/16"}
          />
        </div>
      </div>
    </>
  )
}

function buildCreateVpcCommands(
  watch: (name?: string) => unknown,
  subnetCidrs: {
    publicSubnets: { cidr: string }[]
    privateSubnets: { cidr: string }[]
  },
): CliCommand[] {
  const rawMode = watch("mode")
  const mode = typeof rawMode === "string" ? rawMode : ""
  const rawCidr = watch("cidrBlock")
  const cidr = typeof rawCidr === "string" ? rawCidr : ""
  const rawTenancy = watch("tenancy")
  const tenancy = typeof rawTenancy === "string" ? rawTenancy : ""

  if (mode === "vpc-only") {
    const parts: CommandPart[] = [
      { type: "bin", value: "AWS_PROFILE=spinifex aws ec2 create-vpc" },
      { type: "flag", value: " \\\n  --cidr-block" },
      { type: "value", value: ` ${cidr || "10.0.0.0/16"}` },
    ]
    if (tenancy && tenancy !== "default") {
      parts.push(
        { type: "flag", value: " \\\n  --instance-tenancy" },
        { type: "value", value: ` ${tenancy}` },
      )
    }
    return [{ label: "Create VPC", parts }]
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

  // Comment header
  const commentParts: CommandPart[] = [
    {
      type: "comment",
      value: "# Create VPC with subnets, internet gateway, and routing\n\n",
    },
  ]

  // Create VPC
  commands.push({
    label: "Create VPC",
    parts: [
      ...commentParts,
      { type: "variable", value: "VPC_ID=" },
      { type: "bin", value: "$(AWS_PROFILE=spinifex aws ec2 create-vpc" },
      { type: "flag", value: " \\\n  --cidr-block" },
      { type: "value", value: ` ${cidr || "10.0.0.0/16"}` },
      { type: "flag", value: " \\\n  --query" },
      { type: "value", value: " 'Vpc.VpcId'" },
      { type: "flag", value: " --output" },
      { type: "value", value: " text)" },
    ],
  })

  // Create subnets
  for (const subnet of allSubnets) {
    commands.push({
      label: `Create Subnet (${subnet.name})`,
      parts: [
        { type: "variable", value: `${subnet.name}_ID=` },
        { type: "bin", value: "$(AWS_PROFILE=spinifex aws ec2 create-subnet" },
        { type: "flag", value: " \\\n  --vpc-id" },
        { type: "variable", value: ' "$VPC_ID"' },
        { type: "flag", value: " \\\n  --cidr-block" },
        { type: "value", value: ` ${subnet.cidr}` },
        { type: "flag", value: " \\\n  --query" },
        { type: "value", value: " 'Subnet.SubnetId'" },
        { type: "flag", value: " --output" },
        { type: "value", value: " text)" },
      ],
    })
  }

  // Internet gateway (only if public subnets)
  if (subnetCidrs.publicSubnets.length > 0) {
    commands.push({
      label: "Create Internet Gateway",
      parts: [
        { type: "variable", value: "IGW_ID=" },
        {
          type: "bin",
          value: "$(AWS_PROFILE=spinifex aws ec2 create-internet-gateway",
        },
        { type: "flag", value: " \\\n  --query" },
        { type: "value", value: " 'InternetGateway.InternetGatewayId'" },
        { type: "flag", value: " --output" },
        { type: "value", value: " text)" },
      ],
    })

    commands.push({
      label: "Attach Internet Gateway",
      parts: [
        {
          type: "bin",
          value: "AWS_PROFILE=spinifex aws ec2 attach-internet-gateway",
        },
        { type: "flag", value: " \\\n  --internet-gateway-id" },
        { type: "variable", value: ' "$IGW_ID"' },
        { type: "flag", value: " \\\n  --vpc-id" },
        { type: "variable", value: ' "$VPC_ID"' },
      ],
    })

    // Route table for public subnets
    commands.push({
      label: "Create Route Table",
      parts: [
        { type: "variable", value: "RT_ID=" },
        {
          type: "bin",
          value: "$(AWS_PROFILE=spinifex aws ec2 create-route-table",
        },
        { type: "flag", value: " \\\n  --vpc-id" },
        { type: "variable", value: ' "$VPC_ID"' },
        { type: "flag", value: " \\\n  --query" },
        { type: "value", value: " 'RouteTable.RouteTableId'" },
        { type: "flag", value: " --output" },
        { type: "value", value: " text)" },
      ],
    })

    commands.push({
      label: "Create Default Route",
      parts: [
        { type: "bin", value: "AWS_PROFILE=spinifex aws ec2 create-route" },
        { type: "flag", value: " \\\n  --route-table-id" },
        { type: "variable", value: ' "$RT_ID"' },
        { type: "flag", value: " \\\n  --destination-cidr-block" },
        { type: "value", value: " 0.0.0.0/0" },
        { type: "flag", value: " \\\n  --gateway-id" },
        { type: "variable", value: ' "$IGW_ID"' },
      ],
    })

    // Associate route table with each public subnet
    for (let i = 0; i < subnetCidrs.publicSubnets.length; i++) {
      commands.push({
        label: `Associate Route Table (PUBLIC_SUBNET_${i + 1})`,
        parts: [
          {
            type: "bin",
            value: "AWS_PROFILE=spinifex aws ec2 associate-route-table",
          },
          { type: "flag", value: " \\\n  --route-table-id" },
          { type: "variable", value: ' "$RT_ID"' },
          { type: "flag", value: " \\\n  --subnet-id" },
          { type: "variable", value: ` "$PUBLIC_SUBNET_${i + 1}_ID"` },
        ],
      })
    }
  }

  return commands
}

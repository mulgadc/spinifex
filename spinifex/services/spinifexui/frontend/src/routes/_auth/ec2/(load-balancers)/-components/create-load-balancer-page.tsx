import type { Subnet, Vpc } from "@aws-sdk/client-ec2"
import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import { Plus, Trash2 } from "lucide-react"
import { useState } from "react"
import { Controller, useForm } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import {
  CliCommandPanel,
  type CliCommand,
  type CommandPart,
} from "@/components/cli-command-panel"
import { TargetGroupForm } from "@/components/elbv2/target-group-form"
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
import { getNameTag } from "@/lib/utils"
import {
  type LbWizardResult,
  useCreateLoadBalancerWizard,
} from "@/mutations/elbv2"
import {
  ec2SecurityGroupsQueryOptions,
  ec2SubnetsQueryOptions,
  ec2VpcsQueryOptions,
} from "@/queries/ec2"
import { elbv2TargetGroupsQueryOptions } from "@/queries/elbv2"
import {
  type CreateLoadBalancerFormData,
  type CreateTargetGroupFormData,
  createLoadBalancerSchema,
  createTargetGroupSchema,
} from "@/types/elbv2"

function vpcLabel(vpc: Vpc): string {
  const name = getNameTag(vpc.Tags)
  if (name) {
    return `${vpc.VpcId} (${name})`
  }
  return `${vpc.VpcId} (${vpc.CidrBlock})`
}

function subnetLabel(subnet: Subnet): string {
  const name = getNameTag(subnet.Tags)
  const suffix = name ? `${subnet.SubnetId} (${name})` : subnet.SubnetId
  return `${suffix} · ${subnet.CidrBlock}`
}

interface GroupedSubnets {
  az: string
  subnets: Subnet[]
}

function groupSubnetsByAz(subnets: Subnet[]): GroupedSubnets[] {
  const byAz = new Map<string, Subnet[]>()
  for (const subnet of subnets) {
    const az = subnet.AvailabilityZone ?? "unknown"
    const list = byAz.get(az) ?? []
    list.push(subnet)
    byAz.set(az, list)
  }
  return [...byAz.entries()]
    .toSorted((a, b) => a[0].localeCompare(b[0]))
    .map(([az, list]) => ({ az, subnets: list }))
}

export function CreateLoadBalancerPage() {
  const navigate = useNavigate()
  const { data: vpcsData } = useSuspenseQuery(ec2VpcsQueryOptions)
  const { data: subnetsData } = useSuspenseQuery(ec2SubnetsQueryOptions)
  const { data: sgsData } = useSuspenseQuery(ec2SecurityGroupsQueryOptions)
  const { data: tgsData } = useSuspenseQuery(elbv2TargetGroupsQueryOptions)
  const wizardMutation = useCreateLoadBalancerWizard()

  const [wizardResult, setWizardResult] = useState<LbWizardResult | null>(null)

  const vpcs = vpcsData.Vpcs ?? []
  const allSubnets = subnetsData.Subnets ?? []
  const allSgs = sgsData.SecurityGroups ?? []
  const allTgs = tgsData.TargetGroups ?? []

  const lbForm = useForm<CreateLoadBalancerFormData>({
    resolver: zodResolver(createLoadBalancerSchema),
    defaultValues: {
      name: "",
      scheme: "internet-facing",
      vpcId: vpcs[0]?.VpcId ?? "",
      subnetIds: [],
      securityGroupIds: [],
      tags: [],
      listener: {
        protocol: "HTTP",
        port: 80,
        targetGroupMode: "new",
        existingTargetGroupArn: "",
      },
    },
  })

  const tgForm = useForm<CreateTargetGroupFormData>({
    resolver: zodResolver(createTargetGroupSchema),
    defaultValues: {
      name: "",
      protocol: "HTTP",
      port: 80,
      vpcId: vpcs[0]?.VpcId ?? "",
      healthCheck: {
        protocol: "HTTP",
        path: "/",
        port: "traffic-port",
        intervalSeconds: 30,
        timeoutSeconds: 5,
        healthyThresholdCount: 5,
        unhealthyThresholdCount: 2,
        matcher: "200",
      },
      tags: [],
    },
  })

  const {
    handleSubmit,
    register,
    watch,
    control,
    setValue,
    getValues,
    formState: { errors, isSubmitting },
  } = lbForm

  const selectedVpc = watch("vpcId")
  const selectedSubnets = watch("subnetIds")
  const selectedSgs = watch("securityGroupIds")
  const tgMode = watch("listener.targetGroupMode")
  const tags = watch("tags")

  const vpcSubnets = allSubnets.filter((s) => s.VpcId === selectedVpc)
  const vpcSgs = allSgs.filter((g) => g.VpcId === selectedVpc)
  const vpcTgs = allTgs.filter((tg) => tg.VpcId === selectedVpc)

  // When the VPC changes, any previously-selected subnets/SGs/TGs from the old
  // VPC must be cleared — they would fail backend validation on submit.
  const handleVpcChange = (newVpcId: string | null = "") => {
    const next = newVpcId ?? ""
    setValue("vpcId", next, { shouldValidate: true })
    setValue("subnetIds", [], { shouldValidate: true })
    setValue("securityGroupIds", [])
    setValue("listener.existingTargetGroupArn", "")
    tgForm.setValue("vpcId", next)
  }

  const toggleSubnet = (subnetId: string) => {
    const current = getValues("subnetIds")
    const next = current.includes(subnetId)
      ? current.filter((id) => id !== subnetId)
      : [...current, subnetId]
    setValue("subnetIds", next, { shouldValidate: true })
  }

  const toggleSg = (sgId: string) => {
    const current = getValues("securityGroupIds")
    const next = current.includes(sgId)
      ? current.filter((id) => id !== sgId)
      : [...current, sgId]
    setValue("securityGroupIds", next)
  }

  const onSubmit = async (data: CreateLoadBalancerFormData) => {
    setWizardResult(null)

    let newTargetGroup: CreateTargetGroupFormData | undefined
    if (data.listener.targetGroupMode === "new") {
      const tgValid = await tgForm.trigger()
      if (!tgValid) {
        return
      }
      newTargetGroup = tgForm.getValues()
    }

    const result = await wizardMutation.mutateAsync({
      lb: {
        name: data.name,
        scheme: data.scheme,
        vpcId: data.vpcId,
        subnetIds: data.subnetIds,
        securityGroupIds: data.securityGroupIds,
        tags: data.tags,
      },
      listener: {
        protocol: data.listener.protocol,
        port: data.listener.port,
        targetGroupMode: data.listener.targetGroupMode,
        existingTargetGroupArn: data.listener.existingTargetGroupArn,
        newTargetGroup,
      },
    })
    setWizardResult(result)

    if (!result.error && result.loadBalancerArn) {
      navigate({
        to: "/ec2/describe-load-balancers/$id",
        params: { id: encodeURIComponent(result.loadBalancerArn) },
      })
    }
  }

  const groupedSubnets = groupSubnetsByAz(vpcSubnets)

  return (
    <>
      <BackLink to="/ec2/describe-load-balancers">
        Back to load balancers
      </BackLink>

      <PageHeading title="Create load balancer" />

      {wizardMutation.error && (
        <ErrorBanner
          error={wizardMutation.error}
          msg="Failed to create load balancer"
        />
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
                Successfully created resources (clean up manually if needed):
              </p>
              <ul className="mt-1 list-inside list-disc text-xs text-destructive">
                {wizardResult.created.map((r, i) => (
                  // oxlint-disable-next-line react/no-array-index-key -- error list with no stable id
                  <li key={i}>
                    {r.type}: {r.id ?? "(created)"}
                  </li>
                ))}
              </ul>
            </div>
          )}
        </div>
      )}

      <form className="max-w-4xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
        <Field>
          <FieldTitle>
            <label htmlFor="lb-name">Name</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.name}
            id="lb-name"
            placeholder="my-load-balancer"
            {...register("name")}
          />
          <FieldError errors={[errors.name]} />
        </Field>

        <Field>
          <FieldTitle>Scheme</FieldTitle>
          <Controller
            control={control}
            name="scheme"
            render={({ field }) => (
              <div className="flex gap-4">
                <label className="flex items-center gap-2 text-xs">
                  <input
                    aria-label="Internet-facing"
                    checked={field.value === "internet-facing"}
                    name="scheme"
                    onChange={() => field.onChange("internet-facing")}
                    type="radio"
                  />
                  Internet-facing
                </label>
                <label className="flex items-center gap-2 text-xs">
                  <input
                    aria-label="Internal"
                    checked={field.value === "internal"}
                    name="scheme"
                    onChange={() => field.onChange("internal")}
                    type="radio"
                  />
                  Internal
                </label>
              </div>
            )}
          />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="lb-vpc">VPC</label>
          </FieldTitle>
          <Controller
            control={control}
            name="vpcId"
            render={({ field }) => (
              <Select
                onValueChange={(v) => handleVpcChange(v)}
                value={field.value ?? ""}
              >
                <SelectTrigger
                  aria-invalid={!!errors.vpcId}
                  className="w-full"
                  id="lb-vpc"
                >
                  <SelectValue placeholder="Select VPC" />
                </SelectTrigger>
                <SelectContent>
                  {vpcs.map((vpc) => (
                    <SelectItem key={vpc.VpcId} value={vpc.VpcId ?? ""}>
                      {vpcLabel(vpc)}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
          />
          <FieldError errors={[errors.vpcId]} />
        </Field>

        <Field>
          <FieldTitle>Subnets (select at least 2)</FieldTitle>
          {groupedSubnets.length === 0 ? (
            <p className="text-xs text-muted-foreground">
              No subnets in the selected VPC.
            </p>
          ) : (
            <div className="space-y-3">
              {groupedSubnets.map((group) => (
                <div key={group.az}>
                  <p className="mb-1 text-xs font-medium text-muted-foreground">
                    {group.az}
                  </p>
                  <div className="space-y-1">
                    {group.subnets.map((subnet) => (
                      <label
                        className="flex items-center gap-2 text-xs"
                        key={subnet.SubnetId}
                      >
                        <input
                          aria-label={`Subnet ${subnetLabel(subnet)}`}
                          checked={selectedSubnets.includes(
                            subnet.SubnetId ?? "",
                          )}
                          onChange={() => toggleSubnet(subnet.SubnetId ?? "")}
                          type="checkbox"
                        />
                        <span className="font-mono">{subnetLabel(subnet)}</span>
                      </label>
                    ))}
                  </div>
                </div>
              ))}
            </div>
          )}
          <FieldError errors={[errors.subnetIds]} />
        </Field>

        <Field>
          <FieldTitle>Security groups</FieldTitle>
          {vpcSgs.length === 0 ? (
            <p className="text-xs text-muted-foreground">
              No security groups in the selected VPC.
            </p>
          ) : (
            <div className="space-y-1">
              {vpcSgs.map((sg) => (
                <label
                  className="flex items-center gap-2 text-xs"
                  key={sg.GroupId}
                >
                  <input
                    aria-label={`Security group ${sg.GroupId} (${sg.GroupName})`}
                    checked={selectedSgs.includes(sg.GroupId ?? "")}
                    onChange={() => toggleSg(sg.GroupId ?? "")}
                    type="checkbox"
                  />
                  <span className="font-mono">
                    {sg.GroupId} ({sg.GroupName})
                  </span>
                </label>
              ))}
            </div>
          )}
        </Field>

        <Field>
          <FieldTitle>Tags</FieldTitle>
          <div className="space-y-2">
            {tags.map((_, index) => (
              // oxlint-disable-next-line react/no-array-index-key -- form array with no stable id
              <div className="flex items-center gap-2" key={index}>
                <Input placeholder="Key" {...register(`tags.${index}.key`)} />
                <Input
                  placeholder="Value"
                  {...register(`tags.${index}.value`)}
                />
                <Button
                  onClick={() =>
                    setValue(
                      "tags",
                      getValues("tags").filter((__, i) => i !== index),
                    )
                  }
                  size="icon"
                  type="button"
                  variant="ghost"
                >
                  <Trash2 className="size-3.5" />
                </Button>
              </div>
            ))}
            <Button
              onClick={() =>
                setValue("tags", [...getValues("tags"), { key: "", value: "" }])
              }
              size="sm"
              type="button"
              variant="outline"
            >
              <Plus className="size-3.5" />
              Add tag
            </Button>
          </div>
        </Field>

        <div className="space-y-4 rounded-md border bg-card p-4">
          <h2 className="text-sm font-semibold">
            First listener &amp; default target group
          </h2>

          <Field>
            <FieldTitle>
              <label htmlFor="listener-protocol">Protocol</label>
            </FieldTitle>
            <Controller
              control={control}
              name="listener.protocol"
              render={({ field }) => (
                <Select onValueChange={field.onChange} value={field.value}>
                  <SelectTrigger className="w-full" id="listener-protocol">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="HTTP">HTTP</SelectItem>
                  </SelectContent>
                </Select>
              )}
            />
          </Field>

          <Field>
            <FieldTitle>
              <label htmlFor="listener-port">Port</label>
            </FieldTitle>
            <Input
              aria-invalid={!!errors.listener?.port}
              id="listener-port"
              inputMode="numeric"
              type="number"
              {...register("listener.port", { valueAsNumber: true })}
            />
            <FieldError errors={[errors.listener?.port]} />
          </Field>

          <Field>
            <FieldTitle>Default target group</FieldTitle>
            <Controller
              control={control}
              name="listener.targetGroupMode"
              render={({ field }) => (
                <div className="flex gap-4">
                  <label className="flex items-center gap-2 text-xs">
                    <input
                      aria-label="Create new target group"
                      checked={field.value === "new"}
                      name="tg-mode"
                      onChange={() => field.onChange("new")}
                      type="radio"
                    />
                    Create new
                  </label>
                  <label className="flex items-center gap-2 text-xs">
                    <input
                      aria-label="Use existing target group"
                      checked={field.value === "existing"}
                      name="tg-mode"
                      onChange={() => field.onChange("existing")}
                      type="radio"
                    />
                    Use existing
                  </label>
                </div>
              )}
            />
          </Field>

          {tgMode === "existing" && (
            <Field>
              <FieldTitle>
                <label htmlFor="tg-existing">Target group</label>
              </FieldTitle>
              <Controller
                control={control}
                name="listener.existingTargetGroupArn"
                render={({ field }) => (
                  <Select
                    onValueChange={field.onChange}
                    value={field.value ?? ""}
                  >
                    <SelectTrigger
                      aria-invalid={!!errors.listener?.existingTargetGroupArn}
                      className="w-full"
                      id="tg-existing"
                    >
                      <SelectValue placeholder="Select target group" />
                    </SelectTrigger>
                    <SelectContent>
                      {vpcTgs.map((tg) => (
                        <SelectItem
                          key={tg.TargetGroupArn}
                          value={tg.TargetGroupArn ?? ""}
                        >
                          {tg.TargetGroupName} · {tg.Protocol}:{tg.Port}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                )}
              />
              <FieldError errors={[errors.listener?.existingTargetGroupArn]} />
              {vpcTgs.length === 0 && (
                <p className="mt-1 text-xs text-muted-foreground">
                  No target groups in this VPC. Choose &ldquo;Create new&rdquo;
                  instead.
                </p>
              )}
            </Field>
          )}

          {tgMode === "new" && (
            <div className="space-y-4 border-l-2 border-muted pl-4">
              <TargetGroupForm form={tgForm} vpcs={vpcs} />
            </div>
          )}
        </div>

        <CliCommandPanel
          commands={buildCreateLbCommands(watch, tgForm.watch)}
        />

        <FormActions
          isPending={wizardMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={async () =>
            await navigate({ to: "/ec2/describe-load-balancers" })
          }
          pendingLabel="Creating…"
          submitLabel="Create load balancer"
        />
      </form>
    </>
  )
}

// oxlint-disable-next-line complexity -- CLI preview composition
function buildCreateLbCommands(
  watch: (name?: string) => unknown,
  tgWatch: (name?: string) => unknown,
): CliCommand[] {
  const asString = (key: string): string => {
    const raw = watch(key)
    return typeof raw === "string" ? raw : ""
  }
  const asStringArray = (key: string): string[] => {
    const raw = watch(key)
    return Array.isArray(raw)
      ? raw.filter((v): v is string => typeof v === "string")
      : []
  }
  const asNumber = (key: string): number | undefined => {
    const raw = watch(key)
    return typeof raw === "number" && Number.isFinite(raw) ? raw : undefined
  }

  const name = asString("name") || "<Name>"
  const scheme = asString("scheme") || "internet-facing"
  const subnets = asStringArray("subnetIds")
  const sgs = asStringArray("securityGroupIds")
  const listenerProtocol = asString("listener.protocol") || "HTTP"
  const listenerPort = asNumber("listener.port") ?? 80
  const tgMode = asString("listener.targetGroupMode")
  const existingTgArn = asString("listener.existingTargetGroupArn")

  const commands: CliCommand[] = []
  const comment: CommandPart = {
    type: "comment",
    value: "# Create ALB with listener and default target group\n\n",
  }

  const tgAsString = (key: string): string => {
    const raw = tgWatch(key)
    return typeof raw === "string" ? raw : ""
  }
  const tgAsNumber = (key: string): number | undefined => {
    const raw = tgWatch(key)
    return typeof raw === "number" && Number.isFinite(raw) ? raw : undefined
  }

  // Either a create-target-group step (mode=new) or just a TG_ARN= assignment
  if (tgMode === "new") {
    const tgName = tgAsString("name") || "<TG-Name>"
    const tgPort = tgAsNumber("port") ?? 80
    const tgVpc = tgAsString("vpcId")
    commands.push({
      label: "Create Target Group",
      parts: [
        comment,
        { type: "variable", value: "TG_ARN=" },
        {
          type: "bin",
          value: "$(AWS_PROFILE=spinifex aws elbv2 create-target-group",
        },
        { type: "flag", value: " \\\n  --name" },
        { type: "value", value: ` ${tgName}` },
        { type: "flag", value: " \\\n  --protocol HTTP --port" },
        { type: "value", value: ` ${tgPort}` },
        { type: "flag", value: " \\\n  --target-type instance --vpc-id" },
        { type: "value", value: ` ${tgVpc || "<vpc-id>"}` },
        { type: "flag", value: " \\\n  --query" },
        { type: "value", value: " 'TargetGroups[0].TargetGroupArn'" },
        { type: "flag", value: " --output" },
        { type: "value", value: " text)" },
      ],
    })
  } else {
    commands.push({
      label: "Use Existing Target Group",
      parts: [
        comment,
        { type: "variable", value: "TG_ARN=" },
        { type: "value", value: existingTgArn || "<tg-arn>" },
      ],
    })
  }

  // Create LB
  const lbParts: CommandPart[] = [
    { type: "variable", value: "LB_ARN=" },
    {
      type: "bin",
      value: "$(AWS_PROFILE=spinifex aws elbv2 create-load-balancer",
    },
    { type: "flag", value: " \\\n  --name" },
    { type: "value", value: ` ${name}` },
    { type: "flag", value: " \\\n  --scheme" },
    { type: "value", value: ` ${scheme}` },
    { type: "flag", value: " \\\n  --type application" },
  ]
  if (subnets.length > 0) {
    lbParts.push(
      { type: "flag", value: " \\\n  --subnets" },
      { type: "value", value: ` ${subnets.join(" ")}` },
    )
  }
  if (sgs.length > 0) {
    lbParts.push(
      { type: "flag", value: " \\\n  --security-groups" },
      { type: "value", value: ` ${sgs.join(" ")}` },
    )
  }
  lbParts.push(
    { type: "flag", value: " \\\n  --query" },
    { type: "value", value: " 'LoadBalancers[0].LoadBalancerArn'" },
    { type: "flag", value: " --output" },
    { type: "value", value: " text)" },
  )
  commands.push({ label: "Create Load Balancer", parts: lbParts })

  // Create Listener
  commands.push({
    label: "Create Listener",
    parts: [
      { type: "bin", value: "AWS_PROFILE=spinifex aws elbv2 create-listener" },
      { type: "flag", value: " \\\n  --load-balancer-arn" },
      { type: "variable", value: ' "$LB_ARN"' },
      { type: "flag", value: " \\\n  --protocol" },
      { type: "value", value: ` ${listenerProtocol}` },
      { type: "flag", value: " \\\n  --port" },
      { type: "value", value: ` ${listenerPort}` },
      { type: "flag", value: " \\\n  --default-actions" },
      { type: "value", value: ' Type=forward,TargetGroupArn="$TG_ARN"' },
    ],
  })

  return commands
}

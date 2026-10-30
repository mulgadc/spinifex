import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { useState } from "react"
import {
  Controller,
  type DeepPartialSkipArrayKey,
  useForm,
  useWatch,
} from "react-hook-form"

import {
  CliCommandPanel,
  type CliCommand,
  captureId,
  commandFlag,
  optionalFlag,
  variableFlag,
  type CommandPart,
} from "@/components/cli-command-panel"
import { CertificateImportDialog } from "@/components/elbv2/certificate-import-dialog"
import { listenerProtocolDefaults } from "@/components/elbv2/listener-form"
import { subnetSwitchItems } from "@/components/elbv2/subnets-editor"
import { TargetGroupForm } from "@/components/elbv2/target-group-form"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { SecurityGroupPicker } from "@/components/security-group-picker"
import { SwitchList } from "@/components/switch-list"
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
import { VpcSelectField } from "@/components/vpc-select-field"
import { WizardFailureBanner } from "@/components/wizard-failure-banner"
import { arnResourceId } from "@/lib/utils"
import { useCreateLoadBalancerWizard } from "@/mutations/elbv2"
import { acmCertificatesQueryOptions } from "@/queries/acm"
import {
  ec2SecurityGroupsQueryOptions,
  ec2SubnetsQueryOptions,
  ec2VpcsQueryOptions,
} from "@/queries/ec2"
import {
  elbv2SslPoliciesQueryOptions,
  elbv2TargetGroupsQueryOptions,
} from "@/queries/elbv2"
import {
  type CreateLoadBalancerFormData,
  type CreateTargetGroupFormData,
  type LbType,
  type ListenerProtocolValue,
  type TargetGroupProtocol,
  createLoadBalancerSchema,
  DEFAULT_SSL_POLICY,
  createTargetGroupSchema,
  defaultProtocolForType,
  isSecureProtocol,
  protocolsForType,
} from "@/types/elbv2"

// The target-group protocol the wizard creates for a given listener protocol.
// HTTP and HTTPS both forward to an HTTP target group; secure NLB (TLS) and TCP
// terminate at a TCP one.
function targetGroupProtocolFor(
  protocol: ListenerProtocolValue,
): TargetGroupProtocol {
  if (protocol === "TLS" || protocol === "TCP") {
    return "TCP"
  }

  if (protocol === "UDP") {
    return "UDP"
  }

  if (protocol === "TCP_UDP") {
    return "TCP_UDP"
  }

  return "HTTP"
}

interface CreateLoadBalancerPageProps {
  // Called with the new load balancer's ARN once created, or with nothing on
  // cancel.
  onClose: (arn?: string) => void
}

export function CreateLoadBalancerPage({
  onClose,
}: CreateLoadBalancerPageProps) {
  const { data: vpcsData } = useSuspenseQuery(ec2VpcsQueryOptions)
  const { data: subnetsData } = useSuspenseQuery(ec2SubnetsQueryOptions)
  const { data: sgsData } = useSuspenseQuery(ec2SecurityGroupsQueryOptions)
  const { data: tgsData } = useSuspenseQuery(elbv2TargetGroupsQueryOptions)
  const { data: certsData } = useSuspenseQuery(acmCertificatesQueryOptions)

  const { data: sslPoliciesData } = useSuspenseQuery(
    elbv2SslPoliciesQueryOptions,
  )

  const wizardMutation = useCreateLoadBalancerWizard()

  const [importOpen, setImportOpen] = useState(false)

  const vpcs = vpcsData.Vpcs ?? []
  const allSubnets = subnetsData.Subnets ?? []
  const allSgs = sgsData.SecurityGroups ?? []
  const allTgs = tgsData.TargetGroups ?? []
  const certificates = certsData.CertificateSummaryList ?? []
  const sslPolicies = sslPoliciesData.SslPolicies ?? []

  const lbForm = useForm<CreateLoadBalancerFormData>({
    resolver: zodResolver(createLoadBalancerSchema),
    defaultValues: {
      name: "",
      type: "application",
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
        certificateArn: undefined,
        sslPolicy: undefined,
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
    control,
    setValue,
    getValues,
    formState: { errors, isSubmitting },
  } = lbForm

  const values = useWatch({ control })
  const tgValues = useWatch({ control: tgForm.control })
  const selectedVpc = useWatch({ control, name: "vpcId" })
  const selectedSubnets = useWatch({ control, name: "subnetIds" })
  const selectedSgs = useWatch({ control, name: "securityGroupIds" })
  const lbType = useWatch({ control, name: "type" })
  const listenerProtocol = useWatch({ control, name: "listener.protocol" })
  const tgMode = useWatch({ control, name: "listener.targetGroupMode" })

  const isNlb = lbType === "network"
  const isSecure = isSecureProtocol(listenerProtocol)
  const listenerProtocols = protocolsForType(lbType)

  // The default target group's protocol must match the listener's L4/L7 family;
  // HTTPS terminates at an HTTP target group, secure NLB (TLS) at a TCP one.
  const targetGroupProtocols = isNlb
    ? ["TCP", "UDP", "TLS", "TCP_UDP"]
    : ["HTTP", "HTTPS"]

  const vpcSubnets = allSubnets.filter((s) => s.VpcId === selectedVpc)
  const vpcSgs = allSgs.filter((g) => g.VpcId === selectedVpc)
  const vpcTgs = allTgs.filter((tg) => tg.VpcId === selectedVpc)

  // Keep the inline target-group form's protocol + health-check protocol in step
  // with the listener so the wizard submits a compatible pair.
  const syncTargetGroupProtocol = (listenerProto: ListenerProtocolValue) => {
    const tgProto = targetGroupProtocolFor(listenerProto)
    tgForm.setValue("protocol", tgProto)
    tgForm.setValue("healthCheck.protocol", tgProto === "HTTP" ? "HTTP" : "TCP")
  }

  const handleTypeChange = (next: LbType) => {
    setValue("type", next)
    const proto = defaultProtocolForType(next)
    setValue("listener.protocol", proto)
    setValue("listener.port", 80)
    setValue("listener.certificateArn", undefined)
    setValue("listener.sslPolicy", undefined)

    if (next === "network") {
      setValue("securityGroupIds", [])
    }

    syncTargetGroupProtocol(proto)
  }

  const handleProtocolChange = (next: ListenerProtocolValue) => {
    setValue("listener.protocol", next)
    const fields = listenerProtocolDefaults(next, getValues("listener"))
    setValue("listener.port", fields.port)
    setValue("listener.certificateArn", fields.certificateArn)
    setValue("listener.sslPolicy", fields.sslPolicy)
    syncTargetGroupProtocol(next)
  }

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

  const onSubmit = async (data: CreateLoadBalancerFormData) => {
    wizardMutation.reset()

    let newTargetGroup: CreateTargetGroupFormData | undefined

    if (data.listener.targetGroupMode === "new") {
      const tgValid = await tgForm.trigger()

      if (!tgValid) {
        return
      }

      newTargetGroup = tgForm.getValues()
    }

    let result

    try {
      result = await wizardMutation.mutateAsync({
        lb: {
          name: data.name,
          type: data.type,
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
          certificateArn: data.listener.certificateArn,
          sslPolicy: data.listener.sslPolicy,
          newTargetGroup,
        },
      })
    } catch {
      // Surfaced beside the submit button through the mutation's error.
      return
    }

    if (!result.error && result.loadBalancerArn) {
      onClose(result.loadBalancerArn)
    }
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={wizardMutation.isPending || isSubmitting}
      >
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
          <FieldTitle>Type</FieldTitle>
          <Controller
            control={control}
            name="type"
            render={({ field }) => (
              <div className="flex gap-4">
                <label className="flex items-center gap-2 text-xs">
                  <input
                    aria-label="Application Load Balancer"
                    checked={field.value === "application"}
                    name="lb-type"
                    onChange={() => {
                      handleTypeChange("application")
                    }}
                    type="radio"
                  />
                  Application (ALB)
                </label>
                <label className="flex items-center gap-2 text-xs">
                  <input
                    aria-label="Network Load Balancer"
                    checked={field.value === "network"}
                    name="lb-type"
                    onChange={() => {
                      handleTypeChange("network")
                    }}
                    type="radio"
                  />
                  Network (NLB)
                </label>
              </div>
            )}
          />
        </Field>

        <Field>
          <FieldTitle>Scheme</FieldTitle>
          <div className="flex gap-4">
            <label className="flex items-center gap-2 text-xs">
              <input
                aria-label="Internet-facing"
                type="radio"
                value="internet-facing"
                {...register("scheme")}
              />
              Internet-facing
            </label>
            <label className="flex items-center gap-2 text-xs">
              <input
                aria-label="Internal"
                type="radio"
                value="internal"
                {...register("scheme")}
              />
              Internal
            </label>
          </div>
        </Field>

        <VpcSelectField
          control={control}
          id="lb-vpc"
          name="vpcId"
          onValueChange={handleVpcChange}
          placeholder="Select VPC"
          vpcs={vpcs}
        />

        <Field>
          <FieldTitle>Subnets (select at least 1)</FieldTitle>
          <SwitchList
            emptyText="No subnets in the selected VPC."
            itemLabel="Subnet"
            items={subnetSwitchItems(vpcSubnets)}
            onChange={(next) => {
              setValue("subnetIds", next, { shouldValidate: true })
            }}
            selected={selectedSubnets}
          />
          <FieldError errors={[errors.subnetIds]} />
        </Field>

        {!isNlb && (
          <Field>
            <FieldTitle>Security groups</FieldTitle>
            <SecurityGroupPicker
              emptyText="No security groups in the selected VPC."
              groups={vpcSgs}
              onChange={(next) => {
                setValue("securityGroupIds", next)
              }}
              selected={selectedSgs}
            />
          </Field>
        )}

        <TagsFieldArray control={control} name="tags" />

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
                <Select
                  onValueChange={(next: ListenerProtocolValue | null) => {
                    if (next) {
                      handleProtocolChange(next)
                    }
                  }}
                  value={field.value}
                >
                  <SelectTrigger className="w-full" id="listener-protocol">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {listenerProtocols.map((p) => (
                      <SelectItem key={p} value={p}>
                        {p}
                      </SelectItem>
                    ))}
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

          {isSecure && (
            <>
              <Field>
                <FieldTitle>
                  <label htmlFor="listener-certificate">Certificate</label>
                </FieldTitle>
                <Controller
                  control={control}
                  name="listener.certificateArn"
                  render={({ field }) => (
                    <Select
                      onValueChange={field.onChange}
                      value={field.value ?? ""}
                    >
                      <SelectTrigger
                        aria-invalid={!!errors.listener?.certificateArn}
                        className="w-full"
                        id="listener-certificate"
                      >
                        <SelectValue placeholder="Select certificate" />
                      </SelectTrigger>
                      <SelectContent>
                        {certificates.map((cert) => (
                          <SelectItem
                            key={cert.CertificateArn}
                            value={cert.CertificateArn ?? ""}
                          >
                            {cert.DomainName ??
                              arnResourceId(cert.CertificateArn)}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  )}
                />
                <FieldError errors={[errors.listener?.certificateArn]} />
                <div className="mt-1 flex items-center justify-between">
                  {certificates.length === 0 && (
                    <p className="text-xs text-muted-foreground">
                      No certificates imported yet.
                    </p>
                  )}
                  <Button
                    className="ml-auto"
                    onClick={() => {
                      setImportOpen(true)
                    }}
                    size="sm"
                    type="button"
                    variant="ghost"
                  >
                    Import certificate…
                  </Button>
                </div>
              </Field>

              {listenerProtocol === "HTTPS" && (
                <Field>
                  <FieldTitle>
                    <label htmlFor="listener-ssl-policy">Security policy</label>
                  </FieldTitle>
                  <Controller
                    control={control}
                    name="listener.sslPolicy"
                    render={({ field }) => (
                      <Select
                        onValueChange={field.onChange}
                        value={field.value ?? DEFAULT_SSL_POLICY}
                      >
                        <SelectTrigger
                          className="w-full"
                          id="listener-ssl-policy"
                        >
                          <SelectValue />
                        </SelectTrigger>
                        <SelectContent>
                          {sslPolicies.map((policy) => (
                            <SelectItem
                              key={policy.Name}
                              value={policy.Name ?? ""}
                            >
                              {policy.Name}
                            </SelectItem>
                          ))}
                        </SelectContent>
                      </Select>
                    )}
                  />
                </Field>
              )}
            </>
          )}

          <Field>
            <FieldTitle>Default target group</FieldTitle>
            <div className="flex gap-4">
              <label className="flex items-center gap-2 text-xs">
                <input
                  aria-label="Create new target group"
                  type="radio"
                  value="new"
                  {...register("listener.targetGroupMode")}
                />
                Create new
              </label>
              <label className="flex items-center gap-2 text-xs">
                <input
                  aria-label="Use existing target group"
                  type="radio"
                  value="existing"
                  {...register("listener.targetGroupMode")}
                />
                Use existing
              </label>
            </div>
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
              <TargetGroupForm
                allowedProtocols={targetGroupProtocols}
                form={tgForm}
                vpcs={vpcs}
              />
            </div>
          )}
        </div>

        <CertificateImportDialog
          onImported={(arn) => {
            setValue("listener.certificateArn", arn, { shouldValidate: true })
          }}
          onOpenChange={setImportOpen}
          open={importOpen}
        />

        <CliCommandPanel commands={buildCreateLbCommands(values, tgValues)} />

        <WizardFailureBanner
          createdHeading="Successfully created resources (clean up manually if needed):"
          result={wizardMutation.data}
        />

        <FormActions
          error={wizardMutation.error}
          errorMsg="Failed to create load balancer"
          isPending={wizardMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            onClose()
          }}
          pendingLabel="Creating…"
          submitLabel="Create Load Balancer"
        />
      </FormLock>
    </form>
  )
}

function buildCreateLbCommands(
  values: DeepPartialSkipArrayKey<CreateLoadBalancerFormData>,
  tgValues: DeepPartialSkipArrayKey<CreateTargetGroupFormData>,
): CliCommand[] {
  const lbTypeValue = values.type || "application"
  const listener = values.listener ?? {}
  const listenerProtocol = listener.protocol || "HTTP"

  const commands: CliCommand[] = []

  const comment: CommandPart = {
    type: "comment",
    value:
      lbTypeValue === "network"
        ? "# Create NLB with listener and default target group\n\n"
        : "# Create ALB with listener and default target group\n\n",
  }

  // Either a create-target-group step (mode=new) or just a TG_ARN= assignment
  if (listener.targetGroupMode === "new") {
    commands.push({
      label: "Create Target Group",
      parts: [
        comment,
        ...captureId(
          "TG_ARN",
          "AWS_PROFILE=spinifex aws elbv2 create-target-group",
          [
            ...commandFlag("\\\n  --name", tgValues.name || "<TG-Name>"),
            ...commandFlag("\\\n  --protocol", tgValues.protocol || "HTTP"),
            ...commandFlag("--port", tgValues.port ?? 80),
            ...commandFlag(
              "\\\n  --target-type instance --vpc-id",
              tgValues.vpcId || "<vpc-id>",
            ),
          ],
          "TargetGroups[0].TargetGroupArn",
        ),
      ],
    })
  } else {
    commands.push({
      label: "Use Existing Target Group",
      parts: [
        comment,
        { type: "variable", value: "TG_ARN=" },
        { type: "value", value: listener.existingTargetGroupArn || "<tg-arn>" },
      ],
    })
  }

  commands.push({
    label: "Create Load Balancer",
    parts: captureId(
      "LB_ARN",
      "AWS_PROFILE=spinifex aws elbv2 create-load-balancer",
      [
        ...commandFlag("\\\n  --name", values.name || "<Name>"),
        ...commandFlag("\\\n  --scheme", values.scheme || "internet-facing"),
        ...commandFlag("\\\n  --type", lbTypeValue),
        ...optionalFlag("\\\n  --subnets", values.subnetIds?.join(" ")),
        ...optionalFlag(
          "\\\n  --security-groups",
          values.securityGroupIds?.join(" "),
        ),
      ],
      "LoadBalancers[0].LoadBalancerArn",
    ),
  })

  const listenerParts: CommandPart[] = [
    { type: "bin", value: "AWS_PROFILE=spinifex aws elbv2 create-listener" },
    ...variableFlag("\\\n  --load-balancer-arn", "LB_ARN"),
    ...commandFlag("\\\n  --protocol", listenerProtocol),
    ...commandFlag("\\\n  --port", listener.port ?? 80),
  ]

  if (isSecureProtocol(listenerProtocol) && listener.certificateArn) {
    listenerParts.push(
      ...commandFlag(
        "\\\n  --certificates",
        `CertificateArn=${listener.certificateArn}`,
      ),
      ...optionalFlag(
        "\\\n  --ssl-policy",
        listenerProtocol === "HTTPS" && listener.sslPolicy,
      ),
    )
  }

  listenerParts.push(
    ...commandFlag(
      "\\\n  --default-actions",
      'Type=forward,TargetGroupArn="$TG_ARN"',
    ),
  )
  commands.push({ label: "Create Listener", parts: listenerParts })

  return commands
}

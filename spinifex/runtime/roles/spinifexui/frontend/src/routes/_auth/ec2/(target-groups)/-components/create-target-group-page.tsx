import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import {
  type DeepPartialSkipArrayKey,
  useForm,
  useWatch,
} from "react-hook-form"

import { BackLink } from "@/components/back-link"
import {
  CliCommandPanel,
  type CliCommand,
  cliPlaceholder,
  commandFlag,
  optionalFlag,
  type CommandPart,
} from "@/components/cli-command-panel"
import { TargetGroupForm } from "@/components/elbv2/target-group-form"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { PageHeading } from "@/components/page-heading"
import { useCreateTargetGroup } from "@/mutations/elbv2"
import { ec2VpcsQueryOptions } from "@/queries/ec2"
import {
  type CreateTargetGroupFormData,
  createTargetGroupSchema,
} from "@/types/elbv2"

export function CreateTargetGroupPage() {
  const navigate = useNavigate()
  const { data: vpcsData } = useSuspenseQuery(ec2VpcsQueryOptions)
  const createMutation = useCreateTargetGroup()

  const vpcs = vpcsData.Vpcs ?? []

  const form = useForm({
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
    formState: { isSubmitting },
  } = form

  const values = useWatch({ control: form.control })

  const onSubmit = async (data: CreateTargetGroupFormData) => {
    try {
      const result = await createMutation.mutateAsync(data)
      const arn = result.TargetGroups?.[0]?.TargetGroupArn

      if (arn) {
        navigate({
          to: "/ec2/describe-target-groups/$id",
          params: { id: encodeURIComponent(arn) },
        })
      } else {
        navigate({ to: "/ec2/describe-target-groups" })
      }
    } catch {
      // Error surfaced via createMutation.error in the ErrorBanner above.
    }
  }

  return (
    <>
      <BackLink to="/ec2/describe-target-groups">
        Back to target groups
      </BackLink>

      <PageHeading title="Create Target Group" />

      {createMutation.error && (
        <ErrorBanner
          error={createMutation.error}
          msg="Failed to create target group"
        />
      )}

      <form className="max-w-4xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
        <TargetGroupForm form={form} vpcs={vpcs} />

        <CliCommandPanel commands={buildCreateTargetGroupCommands(values)} />

        <FormActions
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={async () => {
            await navigate({ to: "/ec2/describe-target-groups" })
          }}
          pendingLabel="Creating…"
          submitLabel="Create Target Group"
        />
      </form>
    </>
  )
}

function buildCreateTargetGroupCommands(
  values: DeepPartialSkipArrayKey<CreateTargetGroupFormData>,
): CliCommand[] {
  const healthCheck = values.healthCheck ?? {}
  const hcProtocol = healthCheck.protocol || "HTTP"
  const httpHealthCheck = hcProtocol === "HTTP"

  const parts: CommandPart[] = [
    {
      type: "bin",
      value: "AWS_PROFILE=spinifex aws elbv2 create-target-group",
    },
    ...commandFlag("\\\n  --name", cliPlaceholder(values.name ?? "", "Name")),
    ...commandFlag("\\\n  --protocol", values.protocol || "HTTP"),
    ...commandFlag("\\\n  --port", values.port ?? 80),
    ...commandFlag("\\\n  --target-type", "instance"),
    ...optionalFlag("\\\n  --vpc-id", values.vpcId),
    ...commandFlag("\\\n  --health-check-protocol", hcProtocol),
    ...optionalFlag(
      "\\\n  --health-check-path",
      httpHealthCheck && (healthCheck.path || "/"),
    ),
    ...commandFlag(
      "\\\n  --health-check-port",
      healthCheck.port || "traffic-port",
    ),
    ...commandFlag(
      "\\\n  --health-check-interval-seconds",
      healthCheck.intervalSeconds ?? 30,
    ),
    ...commandFlag(
      "\\\n  --health-check-timeout-seconds",
      healthCheck.timeoutSeconds ?? 5,
    ),
    ...commandFlag(
      "\\\n  --healthy-threshold-count",
      healthCheck.healthyThresholdCount ?? 5,
    ),
    ...commandFlag(
      "\\\n  --unhealthy-threshold-count",
      healthCheck.unhealthyThresholdCount ?? 2,
    ),
    ...optionalFlag(
      "\\\n  --matcher",
      httpHealthCheck && `HttpCode=${healthCheck.matcher || "200"}`,
    ),
  ]

  return [{ label: "Create Target Group", parts }]
}

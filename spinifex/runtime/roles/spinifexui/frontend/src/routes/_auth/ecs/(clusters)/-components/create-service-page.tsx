import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { Controller, useForm, useWatch } from "react-hook-form"

import {
  CliCommandPanel,
  type CliCommand,
  cliPlaceholder,
  commandFlag,
  optionalFlag,
  shellSingleQuote,
} from "@/components/cli-command-panel"
import { PanelSection } from "@/components/detail-panel"
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
import { useCreateService } from "@/mutations/ecs"
import {
  ec2SecurityGroupsQueryOptions,
  ec2SubnetsQueryOptions,
} from "@/queries/ec2"
import {
  ecsTaskDefinitionQueryOptions,
  ecsTaskDefinitionsQueryOptions,
} from "@/queries/ecs"
import { elbv2TargetGroupsQueryOptions } from "@/queries/elbv2"
import { createServiceSchema, type CreateServiceFormData } from "@/types/ecs"

import { awsvpcNetworkFlag, cliNumber, ECS_BIN } from "./ecs-cli"
import {
  AWSVPC_SUBNET_REQUIRED,
  AwsvpcNetworkFields,
  TaskDefinitionField,
} from "./task-launch-fields"

interface CreateServicePageProps {
  cluster: string
  // Called once the service is created, or on cancel.
  onClose: () => void
}

export function CreateServicePage({
  cluster,
  onClose,
}: CreateServicePageProps) {
  const createService = useCreateService()

  const { data: taskDefArns } = useSuspenseQuery(ecsTaskDefinitionsQueryOptions)
  const { data: subnetsData } = useSuspenseQuery(ec2SubnetsQueryOptions)
  const { data: sgData } = useSuspenseQuery(ec2SecurityGroupsQueryOptions)
  const { data: tgData } = useSuspenseQuery(elbv2TargetGroupsQueryOptions)
  const subnets = subnetsData.Subnets ?? []
  const securityGroups = sgData.SecurityGroups ?? []
  const targetGroups = tgData.TargetGroups ?? []

  const {
    control,
    handleSubmit,
    register: field,
    setValue,
    setError,
    formState: { errors, isSubmitting },
  } = useForm<CreateServiceFormData>({
    resolver: zodResolver(createServiceSchema),
    defaultValues: {
      cluster,
      serviceName: "",
      taskDefinition: "",
      desiredCount: 1,
      subnets: [],
      securityGroups: [],
      assignPublicIp: false,
      targetGroupArn: "",
      loadBalancerContainerName: "",
      loadBalancerContainerPort: "",
    },
  })

  const selectedTaskDef = useWatch({ control, name: "taskDefinition" })

  const { data: describedTaskDef } = useQuery(
    ecsTaskDefinitionQueryOptions(selectedTaskDef),
  )

  const awsvpc = describedTaskDef?.networkMode === "awsvpc"

  const selectedSubnets = useWatch({ control, name: "subnets" })
  const selectedSgs = useWatch({ control, name: "securityGroups" })
  const assignPublicIp = useWatch({ control, name: "assignPublicIp" })

  const [serviceName, desiredCount, targetGroupArn, lbContainer, lbPort] =
    useWatch({
      control,
      name: [
        "serviceName",
        "desiredCount",
        "targetGroupArn",
        "loadBalancerContainerName",
        "loadBalancerContainerPort",
      ],
    })

  async function onSubmit(data: CreateServiceFormData) {
    if (awsvpc && data.subnets.length === 0) {
      setError("subnets", { message: AWSVPC_SUBNET_REQUIRED })

      return
    }

    try {
      await createService.mutateAsync({ ...data, awsvpc })
      onClose()
    } catch {
      // Left open, with the refusal shown beside the submit button.
    }
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={createService.isPending || isSubmitting}
      >
        <p className="text-sm text-muted-foreground">
          In cluster <span className="font-mono">{cluster}</span>
        </p>

        <Field>
          <FieldTitle>
            <label htmlFor="serviceName">Service name</label>
          </FieldTitle>
          <Input id="serviceName" placeholder="api" {...field("serviceName")} />
          <FieldError errors={[errors.serviceName]} />
        </Field>

        <Controller
          control={control}
          name="taskDefinition"
          render={({ field: f }) => (
            <TaskDefinitionField
              arns={taskDefArns}
              error={errors.taskDefinition}
              onChange={f.onChange}
              value={f.value}
            />
          )}
        />

        <Field>
          <FieldTitle>
            <label htmlFor="desiredCount">Desired count</label>
          </FieldTitle>
          <Input
            id="desiredCount"
            type="number"
            {...field("desiredCount", { valueAsNumber: true })}
          />
          <FieldError errors={[errors.desiredCount]} />
        </Field>

        {awsvpc && (
          <AwsvpcNetworkFields
            assignPublicIp={assignPublicIp}
            onAssignPublicIpChange={(next) => {
              setValue("assignPublicIp", next)
            }}
            onSecurityGroupsChange={(next) => {
              setValue("securityGroups", next, { shouldValidate: true })
            }}
            onSubnetsChange={(next) => {
              setValue("subnets", next, { shouldValidate: true })
            }}
            securityGroups={securityGroups}
            selectedSecurityGroups={selectedSgs}
            selectedSubnets={selectedSubnets}
            subnets={subnets}
            subnetsError={errors.subnets}
          />
        )}

        <PanelSection title="Load balancer (optional)">
          <div className="space-y-4">
            <Field>
              <FieldTitle>
                <label htmlFor="targetGroupArn">Target group</label>
              </FieldTitle>
              <Controller
                control={control}
                name="targetGroupArn"
                render={({ field: f }) => (
                  <Select
                    onValueChange={(v) => {
                      f.onChange(v === "none" ? "" : v)
                    }}
                    value={f.value === "" ? "none" : f.value}
                  >
                    <SelectTrigger className="w-full" id="targetGroupArn">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="none">none</SelectItem>
                      {targetGroups.map((tg) => (
                        <SelectItem
                          key={tg.TargetGroupArn}
                          value={tg.TargetGroupArn ?? ""}
                        >
                          {tg.TargetGroupName}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                )}
              />
            </Field>
            <div className="grid grid-cols-2 gap-4">
              <Field>
                <FieldTitle>
                  <label htmlFor="lbContainerName">Container name</label>
                </FieldTitle>
                <Input
                  id="lbContainerName"
                  placeholder="app"
                  {...field("loadBalancerContainerName")}
                />
              </Field>
              <Field>
                <FieldTitle>
                  <label htmlFor="lbContainerPort">Container port</label>
                </FieldTitle>
                <Input
                  id="lbContainerPort"
                  placeholder="80"
                  type="number"
                  {...field("loadBalancerContainerPort")}
                />
              </Field>
            </div>
          </div>
        </PanelSection>

        <CliCommandPanel
          commands={buildCreateServiceCommands({
            cluster,
            serviceName,
            taskDefinition: selectedTaskDef,
            desiredCount,
            network: awsvpc
              ? {
                  subnets: selectedSubnets,
                  securityGroups: selectedSgs,
                  assignPublicIp,
                }
              : undefined,
            loadBalancer: { targetGroupArn, lbContainer, lbPort },
          })}
        />

        <FormActions
          error={createService.error}
          errorMsg="Failed to create service"
          isPending={createService.isPending}
          isSubmitting={isSubmitting}
          onCancel={onClose}
          pendingLabel="Creating…"
          submitLabel="Create service"
        />
      </FormLock>
    </form>
  )
}

interface CreateServiceCommandInput {
  cluster: string
  serviceName: string
  taskDefinition: string
  desiredCount: number
  network?: Parameters<typeof awsvpcNetworkFlag>[0]
  loadBalancer: { targetGroupArn: string; lbContainer: string; lbPort: string }
}

// The load balancer block is sent only with a target group, and its container
// name and port only when given, as the request does.
function loadBalancersValue({
  targetGroupArn,
  lbContainer,
  lbPort,
}: CreateServiceCommandInput["loadBalancer"]): string {
  const fields = [`targetGroupArn=${targetGroupArn}`]

  if (lbContainer) {
    fields.push(`containerName=${lbContainer}`)
  }

  if (lbPort) {
    fields.push(`containerPort=${lbPort}`)
  }

  return targetGroupArn ? shellSingleQuote(fields.join(",")) : ""
}

function buildCreateServiceCommands({
  cluster,
  serviceName,
  taskDefinition,
  desiredCount,
  network,
  loadBalancer,
}: CreateServiceCommandInput): CliCommand[] {
  return [
    {
      label: "Create service",
      parts: [
        { type: "bin", value: `${ECS_BIN} create-service` },
        ...commandFlag("\\\n  --cluster", cluster),
        ...commandFlag(
          "\\\n  --service-name",
          cliPlaceholder(serviceName.trim(), "name"),
        ),
        ...commandFlag(
          "\\\n  --task-definition",
          cliPlaceholder(taskDefinition, "task-definition"),
        ),
        ...commandFlag(
          "\\\n  --desired-count",
          cliNumber(desiredCount, "count"),
        ),
        ...(network ? awsvpcNetworkFlag(network) : []),
        ...optionalFlag(
          "\\\n  --load-balancers",
          loadBalancersValue(loadBalancer),
        ),
      ],
    },
  ]
}

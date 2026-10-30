import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { Controller, useForm, useWatch } from "react-hook-form"

import {
  CliCommandPanel,
  type CliCommand,
  cliPlaceholder,
  commandFlag,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { useRunTask } from "@/mutations/ecs"
import {
  ec2SecurityGroupsQueryOptions,
  ec2SubnetsQueryOptions,
} from "@/queries/ec2"
import {
  ecsTaskDefinitionQueryOptions,
  ecsTaskDefinitionsQueryOptions,
} from "@/queries/ecs"
import { runTaskSchema, type RunTaskFormData } from "@/types/ecs"

import { awsvpcNetworkFlag, cliNumber, ECS_BIN } from "./ecs-cli"
import {
  AWSVPC_SUBNET_REQUIRED,
  AwsvpcNetworkFields,
  TaskDefinitionField,
} from "./task-launch-fields"

interface RunTaskPageProps {
  cluster: string
  // Called once the task is started, or on cancel.
  onClose: () => void
}

export function RunTaskPage({ cluster, onClose }: RunTaskPageProps) {
  const runTask = useRunTask()

  const { data: taskDefArns } = useSuspenseQuery(ecsTaskDefinitionsQueryOptions)
  const { data: subnetsData } = useSuspenseQuery(ec2SubnetsQueryOptions)
  const { data: sgData } = useSuspenseQuery(ec2SecurityGroupsQueryOptions)
  const subnets = subnetsData.Subnets ?? []
  const securityGroups = sgData.SecurityGroups ?? []

  const {
    control,
    handleSubmit,
    register: field,
    setValue,
    setError,
    formState: { errors, isSubmitting },
  } = useForm<RunTaskFormData>({
    resolver: zodResolver(runTaskSchema),
    defaultValues: {
      cluster,
      taskDefinition: "",
      count: 1,
      subnets: [],
      securityGroups: [],
      assignPublicIp: false,
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
  const count = useWatch({ control, name: "count" })

  function onSubmit(data: RunTaskFormData) {
    if (awsvpc && data.subnets.length === 0) {
      setError("subnets", { message: AWSVPC_SUBNET_REQUIRED })

      return
    }

    runTask.mutate(
      {
        cluster: data.cluster,
        taskDefinition: data.taskDefinition,
        count: data.count,
        awsvpc,
        subnets: data.subnets,
        securityGroups: data.securityGroups,
        assignPublicIp: data.assignPublicIp,
      },
      { onSuccess: onClose },
    )
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={runTask.isPending || isSubmitting}
      >
        <p className="text-sm text-muted-foreground">
          In cluster <span className="font-mono">{cluster}</span>
        </p>

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
            <label htmlFor="count">Count</label>
          </FieldTitle>
          <Input
            id="count"
            type="number"
            {...field("count", { valueAsNumber: true })}
          />
          <FieldError errors={[errors.count]} />
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

        <CliCommandPanel
          commands={buildRunTaskCommands({
            cluster,
            taskDefinition: selectedTaskDef,
            count,
            network: awsvpc
              ? {
                  subnets: selectedSubnets,
                  securityGroups: selectedSgs,
                  assignPublicIp,
                }
              : undefined,
          })}
        />

        <FormActions
          error={runTask.error}
          errorMsg="Failed to run task"
          isPending={runTask.isPending}
          isSubmitting={isSubmitting}
          onCancel={onClose}
          pendingLabel="Running…"
          submitLabel="Run task"
        />
      </FormLock>
    </form>
  )
}

interface RunTaskCommandInput {
  cluster: string
  taskDefinition: string
  count: number
  network?: Parameters<typeof awsvpcNetworkFlag>[0]
}

function buildRunTaskCommands({
  cluster,
  taskDefinition,
  count,
  network,
}: RunTaskCommandInput): CliCommand[] {
  return [
    {
      label: "Run task",
      parts: [
        { type: "bin", value: `${ECS_BIN} run-task` },
        ...commandFlag("\\\n  --cluster", cluster),
        ...commandFlag(
          "\\\n  --task-definition",
          cliPlaceholder(taskDefinition, "task-definition"),
        ),
        ...commandFlag("\\\n  --count", cliNumber(count, "count")),
        ...(network ? awsvpcNetworkFlag(network) : []),
      ],
    },
  ]
}

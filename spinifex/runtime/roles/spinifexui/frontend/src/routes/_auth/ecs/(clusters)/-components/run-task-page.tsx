import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import { Controller, useForm, useWatch } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { PageHeading } from "@/components/page-heading"
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

import { AwsvpcNetworkFields, TaskDefinitionField } from "./task-launch-fields"

export function RunTaskPage({ cluster }: { cluster: string }) {
  const navigate = useNavigate()
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

  function done() {
    void navigate({
      to: "/ecs/list-clusters/$clusterName",
      params: { clusterName: cluster },
    })
  }

  function onSubmit(data: RunTaskFormData) {
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
      { onSuccess: done },
    )
  }

  const awsvpcMissingSubnet = awsvpc && selectedSubnets.length === 0

  return (
    <>
      <BackLink
        params={{ clusterName: cluster }}
        to="/ecs/list-clusters/$clusterName"
      >
        {cluster}
      </BackLink>
      <PageHeading subtitle={`Cluster: ${cluster}`} title="Run Task" />

      {runTask.isError && (
        <ErrorBanner error={runTask.error} msg="Failed to run task." />
      )}

      <form className="max-w-2xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
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
          />
        )}

        <FormActions
          isPending={runTask.isPending || awsvpcMissingSubnet}
          isSubmitting={isSubmitting}
          onCancel={done}
          pendingLabel="Running…"
          submitLabel="Run Task"
        />
      </form>
    </>
  )
}

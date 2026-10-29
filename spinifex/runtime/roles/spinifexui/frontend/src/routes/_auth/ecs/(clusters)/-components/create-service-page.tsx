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

import { AwsvpcNetworkFields, TaskDefinitionField } from "./task-launch-fields"

export function CreateServicePage({ cluster }: { cluster: string }) {
  const navigate = useNavigate()
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

  function done() {
    void navigate({
      to: "/ecs/list-clusters/$clusterName",
      params: { clusterName: cluster },
    })
  }

  async function onSubmit(data: CreateServiceFormData) {
    await createService.mutateAsync({ ...data, awsvpc })
    done()
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
      <PageHeading subtitle={`Cluster: ${cluster}`} title="Create Service" />

      {createService.isError && (
        <ErrorBanner
          error={createService.error}
          msg="Failed to create service."
        />
      )}

      <form className="max-w-2xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
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
          />
        )}

        <div className="space-y-4 rounded-lg border bg-card p-4">
          <h2 className="font-semibold">Load balancer (optional)</h2>
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

        <FormActions
          isPending={createService.isPending || awsvpcMissingSubnet}
          isSubmitting={isSubmitting}
          onCancel={done}
          pendingLabel="Creating…"
          submitLabel="Create Service"
        />
      </form>
    </>
  )
}

import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import { Controller, useForm } from "react-hook-form"

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

function familyRevision(arn: string): string {
  const idx = arn.lastIndexOf("/")
  return idx === -1 ? arn : arn.slice(idx + 1)
}

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
    watch,
    setValue,
    getValues,
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

  const selectedTaskDef = watch("taskDefinition")
  const { data: describedTaskDef } = useQuery(
    ecsTaskDefinitionQueryOptions(selectedTaskDef),
  )
  const awsvpc = describedTaskDef?.networkMode === "awsvpc"

  const selectedSubnets = watch("subnets")
  const selectedSgs = watch("securityGroups")
  const toggle = (name: "subnets" | "securityGroups", id: string) => {
    const current = getValues(name)
    setValue(
      name,
      current.includes(id) ? current.filter((x) => x !== id) : [...current, id],
      { shouldValidate: true },
    )
  }

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
          error={
            createService.error instanceof Error
              ? createService.error
              : undefined
          }
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

        <Field>
          <FieldTitle>
            <label htmlFor="taskDefinition">Task definition</label>
          </FieldTitle>
          <Controller
            control={control}
            name="taskDefinition"
            render={({ field: f }) => (
              <Select onValueChange={f.onChange} value={f.value}>
                <SelectTrigger className="w-full" id="taskDefinition">
                  <SelectValue placeholder="Select a task definition" />
                </SelectTrigger>
                <SelectContent>
                  {taskDefArns.map((arn) => {
                    const id = familyRevision(arn)
                    return (
                      <SelectItem key={arn} value={id}>
                        {id}
                      </SelectItem>
                    )
                  })}
                </SelectContent>
              </Select>
            )}
          />
          <FieldError errors={[errors.taskDefinition]} />
        </Field>

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
          <div className="space-y-4 rounded-lg border bg-card p-4">
            <p className="text-sm text-muted-foreground">
              This task definition uses awsvpc networking. Select at least one
              subnet.
            </p>
            <div>
              <span className="text-sm font-medium">Subnets</span>
              <div className="mt-2 space-y-1">
                {subnets.map((s) => (
                  <label
                    className="flex items-center gap-2 text-xs"
                    key={s.SubnetId}
                  >
                    <input
                      checked={selectedSubnets.includes(s.SubnetId ?? "")}
                      onChange={() => toggle("subnets", s.SubnetId ?? "")}
                      type="checkbox"
                    />
                    <span className="font-mono">
                      {s.SubnetId} ({s.CidrBlock})
                    </span>
                  </label>
                ))}
              </div>
            </div>
            <div>
              <span className="text-sm font-medium">Security groups</span>
              <div className="mt-2 space-y-1">
                {securityGroups.map((sg) => (
                  <label
                    className="flex items-center gap-2 text-xs"
                    key={sg.GroupId}
                  >
                    <input
                      checked={selectedSgs.includes(sg.GroupId ?? "")}
                      onChange={() =>
                        toggle("securityGroups", sg.GroupId ?? "")
                      }
                      type="checkbox"
                    />
                    <span className="font-mono">
                      {sg.GroupId} ({sg.GroupName})
                    </span>
                  </label>
                ))}
              </div>
            </div>
            <label className="flex items-center gap-2 text-sm">
              <input type="checkbox" {...field("assignPublicIp")} />
              <span>Assign public IP</span>
            </label>
          </div>
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
                  onValueChange={(v) => f.onChange(v === "none" ? "" : v)}
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

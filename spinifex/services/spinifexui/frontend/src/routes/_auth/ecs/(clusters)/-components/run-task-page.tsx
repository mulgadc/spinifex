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

// familyRevision turns a task definition ARN into its "family:revision" id.
function familyRevision(arn: string): string {
  const idx = arn.lastIndexOf("/")
  return idx === -1 ? arn : arn.slice(idx + 1)
}

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
    getValues,
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
        <ErrorBanner
          error={runTask.error instanceof Error ? runTask.error : undefined}
          msg="Failed to run task."
        />
      )}

      <form className="max-w-2xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
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

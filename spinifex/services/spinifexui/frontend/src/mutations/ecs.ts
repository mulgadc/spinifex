import {
  type ContainerDefinition,
  CreateClusterCommand,
  CreateServiceCommand,
  DeleteClusterCommand,
  DeregisterContainerInstanceCommand,
  DeregisterTaskDefinitionCommand,
  RegisterTaskDefinitionCommand,
  RunTaskCommand,
  StopTaskCommand,
  UpdateContainerInstancesStateCommand,
} from "@aws-sdk/client-ecs"
import { useMutation, useQueryClient } from "@tanstack/react-query"

import { getEcsClient } from "@/lib/awsClient"
import {
  provisionCapacity,
  type ProvisionCapacityRequest,
} from "@/lib/ecs-provision"
import { ecsContainerInstancesQueryOptions } from "@/queries/ecs"
import type {
  CreateServiceFormData,
  RegisterTaskDefinitionFormData,
} from "@/types/ecs"

export function useCreateCluster() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (clusterName: string) => {
      const command = new CreateClusterCommand({ clusterName })
      return await getEcsClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["ecs", "clusters"] })
    },
  })
}

export function useDeleteCluster() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (clusterName: string) => {
      const command = new DeleteClusterCommand({ cluster: clusterName })
      return await getEcsClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["ecs", "clusters"] })
    },
  })
}

export interface StopTaskParams {
  cluster: string
  task: string
}

export function useStopTask() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: StopTaskParams) => {
      const command = new StopTaskCommand({
        cluster: params.cluster,
        task: params.task,
      })
      return await getEcsClient().send(command)
    },
    onSuccess: (_data, variables) => {
      void queryClient.invalidateQueries({
        queryKey: ["ecs", "clusters", variables.cluster, "tasks"],
      })
    },
  })
}

export function useDeregisterTaskDefinition() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (taskDefinition: string) => {
      const command = new DeregisterTaskDefinitionCommand({ taskDefinition })
      return await getEcsClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: ["ecs", "task-definitions"],
      })
    },
  })
}

export interface UpdateContainerInstanceStateParams {
  cluster: string
  containerInstance: string
  status: "ACTIVE" | "DRAINING"
}

// useUpdateContainerInstanceState toggles an instance between ACTIVE and
// DRAINING; DRAINING force-stops the service-owned tasks placed on it.
export function useUpdateContainerInstanceState() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: UpdateContainerInstanceStateParams) => {
      const command = new UpdateContainerInstancesStateCommand({
        cluster: params.cluster,
        containerInstances: [params.containerInstance],
        status: params.status,
      })
      return await getEcsClient().send(command)
    },
    onSuccess: (_data, variables) => {
      void queryClient.invalidateQueries({
        queryKey: ["ecs", "clusters", variables.cluster, "container-instances"],
      })
    },
  })
}

export interface DeregisterContainerInstanceParams {
  cluster: string
  containerInstance: string
}

// useDeregisterContainerInstance force-removes an instance record; force is
// always set so a still-registered instance can be cleared from the console.
export function useDeregisterContainerInstance() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: DeregisterContainerInstanceParams) => {
      const command = new DeregisterContainerInstanceCommand({
        cluster: params.cluster,
        containerInstance: params.containerInstance,
        force: true,
      })
      return await getEcsClient().send(command)
    },
    onSuccess: (_data, variables) => {
      void queryClient.invalidateQueries({
        queryKey: ["ecs", "clusters", variables.cluster, "container-instances"],
      })
    },
  })
}

// useRegisterTaskDefinition registers a single-container revision. Container
// CPU/memory, GPU count, and port mappings are optional; an empty/zero value
// is omitted so the gateway applies its defaults.
export function useRegisterTaskDefinition() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: RegisterTaskDefinitionFormData) => {
      const container: ContainerDefinition = {
        name: params.containerName,
        image: params.image,
        essential: params.essential,
        cpu: params.containerCpu ? Number(params.containerCpu) : undefined,
        memory: params.containerMemory
          ? Number(params.containerMemory)
          : undefined,
        portMappings:
          params.portMappings.length > 0
            ? params.portMappings.map((pm) => ({
                containerPort: pm.containerPort,
                hostPort: pm.containerPort,
                protocol: pm.protocol,
              }))
            : undefined,
        resourceRequirements:
          params.containerGpuCount > 0
            ? [{ type: "GPU", value: String(params.containerGpuCount) }]
            : undefined,
      }
      const command = new RegisterTaskDefinitionCommand({
        family: params.family,
        networkMode: params.networkMode,
        cpu: params.cpu || undefined,
        memory: params.memory || undefined,
        containerDefinitions: [container],
      })
      return await getEcsClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: ["ecs", "task-definitions"],
      })
    },
  })
}

// useProvisionCapacity launches container instances into a cluster via the
// custom ProvisionCapacity gateway action, then invalidates the cluster's
// container-instance list so it re-polls until they register.
export function useProvisionCapacity() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (req: ProvisionCapacityRequest) =>
      await provisionCapacity(req),
    onSuccess: (_data, variables) => {
      void queryClient.invalidateQueries({
        queryKey: ecsContainerInstancesQueryOptions(variables.Cluster).queryKey,
      })
    },
  })
}

export interface RunTaskParams {
  cluster: string
  taskDefinition: string
  count: number
  awsvpc: boolean
  subnets: string[]
  securityGroups: string[]
  assignPublicIp: boolean
}

// useRunTask launches one-off tasks. An awsvpc task definition requires a
// network configuration (subnets + security groups); other network modes omit
// it entirely.
export function useRunTask() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: RunTaskParams) => {
      const command = new RunTaskCommand({
        cluster: params.cluster,
        taskDefinition: params.taskDefinition,
        count: params.count,
        networkConfiguration: params.awsvpc
          ? {
              awsvpcConfiguration: {
                subnets: params.subnets,
                securityGroups:
                  params.securityGroups.length > 0
                    ? params.securityGroups
                    : undefined,
                assignPublicIp: params.assignPublicIp ? "ENABLED" : "DISABLED",
              },
            }
          : undefined,
      })
      const output = await getEcsClient().send(command)
      // RunTask returns HTTP 200 with a failures array when no task is placed
      // (e.g. no instance has capacity); surface it as an error.
      if ((output.tasks?.length ?? 0) === 0 && output.failures?.length) {
        const f = output.failures[0]
        throw new Error(
          `Task not placed: ${f?.reason ?? "unknown"}${
            f?.detail ? ` (${f.detail})` : ""
          }`,
        )
      }
      return output
    },
    onSuccess: (_data, variables) => {
      void queryClient.invalidateQueries({
        queryKey: ["ecs", "clusters", variables.cluster, "tasks"],
      })
    },
  })
}

export interface CreateServiceParams extends CreateServiceFormData {
  awsvpc: boolean
}

// useCreateService creates a REPLICA service. The load balancer block is sent
// only when a target group is chosen; the awsvpc network config mirrors RunTask.
export function useCreateService() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: CreateServiceParams) => {
      const command = new CreateServiceCommand({
        cluster: params.cluster,
        serviceName: params.serviceName,
        taskDefinition: params.taskDefinition,
        desiredCount: params.desiredCount,
        networkConfiguration: params.awsvpc
          ? {
              awsvpcConfiguration: {
                subnets: params.subnets,
                securityGroups:
                  params.securityGroups.length > 0
                    ? params.securityGroups
                    : undefined,
                assignPublicIp: params.assignPublicIp ? "ENABLED" : "DISABLED",
              },
            }
          : undefined,
        loadBalancers: params.targetGroupArn
          ? [
              {
                targetGroupArn: params.targetGroupArn,
                containerName: params.loadBalancerContainerName || undefined,
                containerPort: params.loadBalancerContainerPort
                  ? Number(params.loadBalancerContainerPort)
                  : undefined,
              },
            ]
          : undefined,
      })
      return await getEcsClient().send(command)
    },
    onSuccess: (_data, variables) => {
      void queryClient.invalidateQueries({
        queryKey: ["ecs", "clusters", variables.cluster, "services"],
      })
    },
  })
}

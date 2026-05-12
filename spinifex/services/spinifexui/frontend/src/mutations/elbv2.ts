import {
  type Action,
  type Tag,
  type TargetDescription,
  CreateListenerCommand,
  CreateLoadBalancerCommand,
  CreateTargetGroupCommand,
  DeleteListenerCommand,
  DeleteLoadBalancerCommand,
  DeleteTargetGroupCommand,
  DeregisterTargetsCommand,
  ModifyLoadBalancerAttributesCommand,
  ModifyTargetGroupAttributesCommand,
  RegisterTargetsCommand,
} from "@aws-sdk/client-elastic-load-balancing-v2"
import { useMutation, useQueryClient } from "@tanstack/react-query"

import { getElbv2Client } from "@/lib/awsClient"
import type {
  CreateLoadBalancerFormData,
  CreateTargetGroupFormData,
} from "@/types/elbv2"

export interface CreateLoadBalancerParams {
  name: string
  scheme: "internet-facing" | "internal"
  subnetIds: string[]
  securityGroupIds: string[]
  tags: { key: string; value: string }[]
}

export function useCreateLoadBalancer() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: CreateLoadBalancerParams) => {
      const tags: Tag[] = params.tags
        .filter((t) => t.key.length > 0)
        .map((t) => ({ Key: t.key, Value: t.value }))
      const command = new CreateLoadBalancerCommand({
        Name: params.name,
        Scheme: params.scheme,
        Type: "application",
        IpAddressType: "ipv4",
        Subnets: params.subnetIds,
        SecurityGroups:
          params.securityGroupIds.length > 0
            ? params.securityGroupIds
            : undefined,
        Tags: tags.length > 0 ? tags : undefined,
      })
      return await getElbv2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: ["elbv2", "loadBalancers"],
      })
    },
  })
}

export function useDeleteLoadBalancer() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (loadBalancerArn: string) => {
      const command = new DeleteLoadBalancerCommand({
        LoadBalancerArn: loadBalancerArn,
      })
      return await getElbv2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: ["elbv2", "loadBalancers"],
      })
    },
  })
}

export interface ModifyLoadBalancerAttributesParams {
  loadBalancerArn: string
  attributes: { key: string; value: string }[]
}

export function useModifyLoadBalancerAttributes() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: ModifyLoadBalancerAttributesParams) => {
      const command = new ModifyLoadBalancerAttributesCommand({
        LoadBalancerArn: params.loadBalancerArn,
        Attributes: params.attributes.map((a) => ({
          Key: a.key,
          Value: a.value,
        })),
      })
      return await getElbv2Client().send(command)
    },
    onSuccess: (_data, variables) => {
      void queryClient.invalidateQueries({
        queryKey: [
          "elbv2",
          "loadBalancers",
          variables.loadBalancerArn,
          "attributes",
        ],
      })
    },
  })
}

export function useCreateTargetGroup() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: CreateTargetGroupFormData) => {
      const tags: Tag[] = params.tags
        .filter((t) => t.key.length > 0)
        .map((t) => ({ Key: t.key, Value: t.value }))
      const command = new CreateTargetGroupCommand({
        Name: params.name,
        Protocol: params.protocol,
        Port: params.port,
        VpcId: params.vpcId,
        TargetType: "instance",
        HealthCheckProtocol: params.healthCheck.protocol,
        HealthCheckPath: params.healthCheck.path,
        HealthCheckPort: params.healthCheck.port,
        HealthCheckIntervalSeconds: params.healthCheck.intervalSeconds,
        HealthCheckTimeoutSeconds: params.healthCheck.timeoutSeconds,
        HealthyThresholdCount: params.healthCheck.healthyThresholdCount,
        UnhealthyThresholdCount: params.healthCheck.unhealthyThresholdCount,
        Matcher: { HttpCode: params.healthCheck.matcher },
        Tags: tags.length > 0 ? tags : undefined,
      })
      return await getElbv2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: ["elbv2", "targetGroups"],
      })
    },
  })
}

export function useDeleteTargetGroup() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (targetGroupArn: string) => {
      const command = new DeleteTargetGroupCommand({
        TargetGroupArn: targetGroupArn,
      })
      return await getElbv2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: ["elbv2", "targetGroups"],
      })
    },
  })
}

export interface ModifyTargetGroupAttributesParams {
  targetGroupArn: string
  attributes: { key: string; value: string }[]
}

export function useModifyTargetGroupAttributes() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: ModifyTargetGroupAttributesParams) => {
      const command = new ModifyTargetGroupAttributesCommand({
        TargetGroupArn: params.targetGroupArn,
        Attributes: params.attributes.map((a) => ({
          Key: a.key,
          Value: a.value,
        })),
      })
      return await getElbv2Client().send(command)
    },
    onSuccess: (_data, variables) => {
      void queryClient.invalidateQueries({
        queryKey: [
          "elbv2",
          "targetGroups",
          variables.targetGroupArn,
          "attributes",
        ],
      })
    },
  })
}

export interface CreateListenerParams {
  loadBalancerArn: string
  protocol: "HTTP"
  port: number
  defaultTargetGroupArn: string
}

export function useCreateListener() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: CreateListenerParams) => {
      const defaultActions: Action[] = [
        {
          Type: "forward",
          TargetGroupArn: params.defaultTargetGroupArn,
        },
      ]
      const command = new CreateListenerCommand({
        LoadBalancerArn: params.loadBalancerArn,
        Protocol: params.protocol,
        Port: params.port,
        DefaultActions: defaultActions,
      })
      return await getElbv2Client().send(command)
    },
    onSuccess: (_data, variables) => {
      void queryClient.invalidateQueries({
        queryKey: ["elbv2", "listeners", variables.loadBalancerArn],
      })
    },
  })
}

export function useDeleteListener() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (listenerArn: string) => {
      const command = new DeleteListenerCommand({ ListenerArn: listenerArn })
      return await getElbv2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["elbv2", "listeners"] })
    },
  })
}

export interface LbWizardCreatedResource {
  type: string
  id: string | undefined
}

export interface LbWizardResult {
  loadBalancerArn: string | undefined
  created: LbWizardCreatedResource[]
  error?: Error
  failedStep?: string
}

export interface CreateLoadBalancerWizardParams {
  lb: Omit<CreateLoadBalancerFormData, "listener">
  listener: {
    protocol: "HTTP"
    port: number
    targetGroupMode: "new" | "existing"
    existingTargetGroupArn?: string
    newTargetGroup?: CreateTargetGroupFormData
  }
}

export function useCreateLoadBalancerWizard() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (
      params: CreateLoadBalancerWizardParams,
    ): Promise<LbWizardResult> => {
      const client = getElbv2Client()
      const created: LbWizardCreatedResource[] = []
      let currentStep = ""

      try {
        // Step 1: create new TG if requested
        let targetGroupArn = params.listener.existingTargetGroupArn
        if (params.listener.targetGroupMode === "new") {
          const tg = params.listener.newTargetGroup
          if (!tg) {
            throw new Error(
              "internal error: new target group requested but no data supplied",
            )
          }
          currentStep = "creating target group"
          const tgTags: Tag[] = tg.tags
            .filter((t) => t.key.length > 0)
            .map((t) => ({ Key: t.key, Value: t.value }))
          const tgResult = await client.send(
            new CreateTargetGroupCommand({
              Name: tg.name,
              Protocol: tg.protocol,
              Port: tg.port,
              VpcId: tg.vpcId,
              TargetType: "instance",
              HealthCheckProtocol: tg.healthCheck.protocol,
              HealthCheckPath: tg.healthCheck.path,
              HealthCheckPort: tg.healthCheck.port,
              HealthCheckIntervalSeconds: tg.healthCheck.intervalSeconds,
              HealthCheckTimeoutSeconds: tg.healthCheck.timeoutSeconds,
              HealthyThresholdCount: tg.healthCheck.healthyThresholdCount,
              UnhealthyThresholdCount: tg.healthCheck.unhealthyThresholdCount,
              Matcher: { HttpCode: tg.healthCheck.matcher },
              Tags: tgTags.length > 0 ? tgTags : undefined,
            }),
          )
          targetGroupArn = tgResult.TargetGroups?.[0]?.TargetGroupArn
          if (!targetGroupArn) {
            throw new Error(
              "Target group was created but no ARN was returned by the API",
            )
          }
          created.push({ type: "Target Group", id: targetGroupArn })
        }

        if (!targetGroupArn) {
          throw new Error("Target group selection is required")
        }

        // Step 2: create LB
        currentStep = "creating load balancer"
        const lbTags: Tag[] = params.lb.tags
          .filter((t) => t.key.length > 0)
          .map((t) => ({ Key: t.key, Value: t.value }))
        const lbResult = await client.send(
          new CreateLoadBalancerCommand({
            Name: params.lb.name,
            Scheme: params.lb.scheme,
            Type: "application",
            IpAddressType: "ipv4",
            Subnets: params.lb.subnetIds,
            SecurityGroups:
              params.lb.securityGroupIds.length > 0
                ? params.lb.securityGroupIds
                : undefined,
            Tags: lbTags.length > 0 ? lbTags : undefined,
          }),
        )
        const loadBalancerArn = lbResult.LoadBalancers?.[0]?.LoadBalancerArn
        if (!loadBalancerArn) {
          throw new Error(
            "Load balancer was created but no ARN was returned by the API",
          )
        }
        created.push({ type: "Load Balancer", id: loadBalancerArn })

        // Step 3: create listener
        currentStep = "creating listener"
        await client.send(
          new CreateListenerCommand({
            LoadBalancerArn: loadBalancerArn,
            Protocol: params.listener.protocol,
            Port: params.listener.port,
            DefaultActions: [
              { Type: "forward", TargetGroupArn: targetGroupArn },
            ],
          }),
        )
        created.push({ type: "Listener", id: undefined })

        return { loadBalancerArn, created }
      } catch (error) {
        return {
          loadBalancerArn: undefined,
          created,
          error: error instanceof Error ? error : new Error(String(error)),
          failedStep: currentStep,
        }
      }
    },
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: ["elbv2"] })
    },
  })
}

export interface TargetInput {
  id: string
  port?: number
}

export interface RegisterTargetsParams {
  targetGroupArn: string
  targets: TargetInput[]
}

function toTargetDescriptions(targets: TargetInput[]): TargetDescription[] {
  return targets.map((t) => ({
    Id: t.id,
    Port: t.port,
  }))
}

export function useRegisterTargets() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: RegisterTargetsParams) => {
      const command = new RegisterTargetsCommand({
        TargetGroupArn: params.targetGroupArn,
        Targets: toTargetDescriptions(params.targets),
      })
      return await getElbv2Client().send(command)
    },
    onSuccess: (_data, variables) => {
      void queryClient.invalidateQueries({
        queryKey: ["elbv2", "targetGroups", variables.targetGroupArn, "health"],
      })
    },
  })
}

export function useDeregisterTargets() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: RegisterTargetsParams) => {
      const command = new DeregisterTargetsCommand({
        TargetGroupArn: params.targetGroupArn,
        Targets: toTargetDescriptions(params.targets),
      })
      return await getElbv2Client().send(command)
    },
    onSuccess: (_data, variables) => {
      void queryClient.invalidateQueries({
        queryKey: ["elbv2", "targetGroups", variables.targetGroupArn, "health"],
      })
    },
  })
}

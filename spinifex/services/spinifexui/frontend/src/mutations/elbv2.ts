import {
  type Action,
  type RuleCondition,
  type RulePriorityPair,
  type Tag,
  type TargetDescription,
  AddTagsCommand,
  CreateListenerCommand,
  CreateLoadBalancerCommand,
  CreateRuleCommand,
  CreateTargetGroupCommand,
  DeleteListenerCommand,
  DeleteLoadBalancerCommand,
  DeleteRuleCommand,
  DeleteTargetGroupCommand,
  DeregisterTargetsCommand,
  ModifyListenerCommand,
  ModifyLoadBalancerAttributesCommand,
  ModifyRuleCommand,
  ModifyTargetGroupAttributesCommand,
  RegisterTargetsCommand,
  RemoveTagsCommand,
  SetRulePrioritiesCommand,
  SetSecurityGroupsCommand,
} from "@aws-sdk/client-elastic-load-balancing-v2"
import { useMutation, useQueryClient } from "@tanstack/react-query"

import { getElbv2Client } from "@/lib/awsClient"
import type {
  CreateLoadBalancerFormData,
  CreateTargetGroupFormData,
} from "@/types/elbv2"

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

export interface UpdateTagsParams {
  // ARN of any taggable ELBv2 resource (load balancer, target group, listener,
  // listener rule).
  resourceArn: string
  // Desired final tag set after the edit.
  tags: { key: string; value: string }[]
  // Tag keys present before the edit, used to compute removals.
  initialKeys: string[]
}

// useUpdateTags reconciles a resource's tags to the desired set by diffing
// against the prior keys: it overwrites the final tags via AddTags and deletes
// any dropped keys via RemoveTags. Either call is skipped when it has no work.
export function useUpdateTags() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: UpdateTagsParams) => {
      const finalKeys = new Set(params.tags.map((t) => t.key))
      const toRemove = params.initialKeys.filter((k) => !finalKeys.has(k))
      const client = getElbv2Client()

      if (params.tags.length > 0) {
        await client.send(
          new AddTagsCommand({
            ResourceArns: [params.resourceArn],
            Tags: params.tags.map((t) => ({ Key: t.key, Value: t.value })),
          }),
        )
      }
      if (toRemove.length > 0) {
        await client.send(
          new RemoveTagsCommand({
            ResourceArns: [params.resourceArn],
            TagKeys: toRemove,
          }),
        )
      }
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["elbv2", "tags"] })
    },
  })
}

export interface SetSecurityGroupsParams {
  loadBalancerArn: string
  securityGroupIds: string[]
}

// useSetSecurityGroups replaces the security groups associated with a load
// balancer (SetSecurityGroups). The new set takes effect on the live data plane
// server-side; the LB query is invalidated so the detail page reflects it.
export function useSetSecurityGroups() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: SetSecurityGroupsParams) => {
      const command = new SetSecurityGroupsCommand({
        LoadBalancerArn: params.loadBalancerArn,
        SecurityGroups: params.securityGroupIds,
      })
      return await getElbv2Client().send(command)
    },
    onSuccess: (_data, variables) => {
      void queryClient.invalidateQueries({
        queryKey: ["elbv2", "loadBalancers", variables.loadBalancerArn],
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

export interface ModifyListenerParams {
  listenerArn: string
  loadBalancerArn: string
  protocol: "HTTP"
  port: number
  defaultTargetGroupArn: string
}

export function useModifyListener() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: ModifyListenerParams) => {
      const defaultActions: Action[] = [
        {
          Type: "forward",
          TargetGroupArn: params.defaultTargetGroupArn,
        },
      ]
      const command = new ModifyListenerCommand({
        ListenerArn: params.listenerArn,
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

export interface RuleCondInput {
  field: string
  values: string[]
  httpHeaderName?: string
  queryStringPairs?: { key?: string; value: string }[]
}

export interface CreateRuleParams {
  listenerArn: string
  priority: number
  conditions: RuleCondInput[]
  forwardTargetGroupArn: string
}

function toSDKConditions(conds: RuleCondInput[]): RuleCondition[] {
  return conds.map((c): RuleCondition => {
    switch (c.field) {
      case "host-header": {
        return {
          Field: c.field,
          HostHeaderConfig: { Values: c.values },
        }
      }
      case "path-pattern": {
        return {
          Field: c.field,
          PathPatternConfig: { Values: c.values },
        }
      }
      case "http-header": {
        return {
          Field: c.field,
          HttpHeaderConfig: {
            HttpHeaderName: c.httpHeaderName ?? "",
            Values: c.values,
          },
        }
      }
      case "http-request-method": {
        return {
          Field: c.field,
          HttpRequestMethodConfig: { Values: c.values },
        }
      }
      case "source-ip": {
        return {
          Field: c.field,
          SourceIpConfig: { Values: c.values },
        }
      }
      case "query-string": {
        return {
          Field: c.field,
          QueryStringConfig: {
            Values: (c.queryStringPairs ?? []).map((p) => ({
              Key: p.key && p.key.length > 0 ? p.key : undefined,
              Value: p.value,
            })),
          },
        }
      }
      default: {
        return { Field: c.field, Values: c.values }
      }
    }
  })
}

export function useCreateRule() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: CreateRuleParams) => {
      const actions: Action[] = [
        { Type: "forward", TargetGroupArn: params.forwardTargetGroupArn },
      ]
      const command = new CreateRuleCommand({
        ListenerArn: params.listenerArn,
        Priority: params.priority,
        Conditions: toSDKConditions(params.conditions),
        Actions: actions,
      })
      return await getElbv2Client().send(command)
    },
    onSuccess: (_data, variables) => {
      void queryClient.invalidateQueries({
        queryKey: ["elbv2", "listeners", variables.listenerArn, "rules"],
      })
    },
  })
}

export interface ModifyRuleParams {
  ruleArn: string
  listenerArn: string
  conditions?: RuleCondInput[]
  forwardTargetGroupArn?: string
}

export function useModifyRule() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: ModifyRuleParams) => {
      const command = new ModifyRuleCommand({
        RuleArn: params.ruleArn,
        Conditions: params.conditions
          ? toSDKConditions(params.conditions)
          : undefined,
        Actions: params.forwardTargetGroupArn
          ? [
              {
                Type: "forward",
                TargetGroupArn: params.forwardTargetGroupArn,
              },
            ]
          : undefined,
      })
      return await getElbv2Client().send(command)
    },
    onSuccess: (_data, variables) => {
      void queryClient.invalidateQueries({
        queryKey: ["elbv2", "listeners", variables.listenerArn, "rules"],
      })
    },
  })
}

export interface DeleteRuleParams {
  ruleArn: string
  listenerArn: string
}

export function useDeleteRule() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: DeleteRuleParams) => {
      const command = new DeleteRuleCommand({ RuleArn: params.ruleArn })
      return await getElbv2Client().send(command)
    },
    onSuccess: (_data, variables) => {
      void queryClient.invalidateQueries({
        queryKey: ["elbv2", "listeners", variables.listenerArn, "rules"],
      })
    },
  })
}

export interface SetRulePrioritiesParams {
  listenerArn: string
  priorities: { ruleArn: string; priority: number }[]
}

export function useSetRulePriorities() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: SetRulePrioritiesParams) => {
      const pairs: RulePriorityPair[] = params.priorities.map((p) => ({
        RuleArn: p.ruleArn,
        Priority: p.priority,
      }))
      const command = new SetRulePrioritiesCommand({ RulePriorities: pairs })
      return await getElbv2Client().send(command)
    },
    onSuccess: (_data, variables) => {
      void queryClient.invalidateQueries({
        queryKey: ["elbv2", "listeners", variables.listenerArn, "rules"],
      })
    },
  })
}

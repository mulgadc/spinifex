import { z } from "zod"

export const NETWORK_MODES = ["bridge", "awsvpc", "host", "none"] as const

export const portMappingSchema = z.object({
  containerPort: z.number().int().min(1).max(65_535),
  protocol: z.enum(["tcp", "udp"]),
})

export const registerTaskDefinitionSchema = z.object({
  family: z.string().min(1, "Family is required"),
  networkMode: z.enum(NETWORK_MODES),
  cpu: z.string(),
  memory: z.string(),
  containerName: z.string().min(1, "Container name is required"),
  image: z.string().min(1, "Image is required"),
  containerCpu: z.string(),
  containerMemory: z.string(),
  essential: z.boolean(),
  portMappings: z.array(portMappingSchema),
})

export type RegisterTaskDefinitionFormData = z.infer<
  typeof registerTaskDefinitionSchema
>

export const runTaskSchema = z.object({
  cluster: z.string().min(1, "Cluster is required"),
  taskDefinition: z.string().min(1, "Task definition is required"),
  count: z.number().int().min(1).max(10),
  subnets: z.array(z.string()),
  securityGroups: z.array(z.string()),
  assignPublicIp: z.boolean(),
})

export type RunTaskFormData = z.infer<typeof runTaskSchema>

export const createServiceSchema = z.object({
  cluster: z.string().min(1, "Cluster is required"),
  serviceName: z.string().min(1, "Service name is required"),
  taskDefinition: z.string().min(1, "Task definition is required"),
  desiredCount: z.number().int().min(0).max(10),
  subnets: z.array(z.string()),
  securityGroups: z.array(z.string()),
  assignPublicIp: z.boolean(),
  targetGroupArn: z.string(),
  loadBalancerContainerName: z.string(),
  loadBalancerContainerPort: z.string(),
})

export type CreateServiceFormData = z.infer<typeof createServiceSchema>

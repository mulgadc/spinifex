import { z } from "zod"

const LB_TG_NAME_REGEX = /^(?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9])$/

const lbNameField = z
  .string()
  .min(1, "Name is required")
  .max(32, "Name must be 32 characters or less")
  .regex(
    LB_TG_NAME_REGEX,
    "Name may contain only letters, digits, and hyphens; must start and end with alphanumeric",
  )
  .refine(
    (value) => !value.startsWith("internal-"),
    "Name cannot start with 'internal-'",
  )

const tgNameField = z
  .string()
  .min(1, "Name is required")
  .max(32, "Name must be 32 characters or less")
  .regex(
    LB_TG_NAME_REGEX,
    "Name may contain only letters, digits, and hyphens; must start and end with alphanumeric",
  )

const portField = z
  .number()
  .int("Port must be a whole number")
  .min(1, "Port must be at least 1")
  .max(65_535, "Port must be at most 65535")

export const tagSchema = z.object({
  key: z.string().min(1, "Tag key is required").max(128),
  value: z.string().max(256),
})

export type TagFormData = z.infer<typeof tagSchema>

// ALB health-check config. NLB variant lands with slice 9.
export const healthCheckSchema = z.object({
  protocol: z.enum(["HTTP"]),
  path: z.string().min(1, "Path is required").max(1024),
  port: z.string().min(1),
  intervalSeconds: z.number().int().min(5).max(300),
  timeoutSeconds: z.number().int().min(2).max(120),
  healthyThresholdCount: z.number().int().min(2).max(10),
  unhealthyThresholdCount: z.number().int().min(2).max(10),
  matcher: z
    .string()
    .regex(
      /^\d{3}(?:[-,]\d{3})*$/,
      "Matcher must be HTTP codes like 200 or 200-299 or 200,201",
    ),
})

export type HealthCheckFormData = z.infer<typeof healthCheckSchema>

export const createTargetGroupSchema = z.object({
  name: tgNameField,
  protocol: z.enum(["HTTP"]),
  port: portField,
  vpcId: z.string().min(1, "VPC is required"),
  healthCheck: healthCheckSchema,
  tags: z.array(tagSchema),
})

export type CreateTargetGroupFormData = z.infer<typeof createTargetGroupSchema>

// Default SSL policy applied when an HTTPS listener leaves the policy unset;
// mirrors the server's DefaultSslPolicy constant.
export const DEFAULT_SSL_POLICY = "ELBSecurityPolicy-2016-08"

export const createListenerSchema = z
  .object({
    protocol: z.enum(["HTTP", "HTTPS"]),
    port: portField,
    defaultTargetGroupArn: z.string().min(1, "Target group is required"),
    certificateArn: z.string().optional(),
    sslPolicy: z.string().optional(),
  })
  .superRefine((data, ctx) => {
    if (data.protocol === "HTTPS" && !data.certificateArn) {
      ctx.addIssue({
        code: "custom",
        path: ["certificateArn"],
        message: "A certificate is required for HTTPS",
      })
    }
  })

export type CreateListenerFormData = z.infer<typeof createListenerSchema>

// printable non-whitespace ASCII
const RULE_VALUE_REGEX = /^[!-~]+$/

export const ruleConditionSchema = z
  .object({
    field: z.enum([
      "host-header",
      "path-pattern",
      "http-header",
      "http-request-method",
      "source-ip",
      "query-string",
    ]),
    httpHeaderName: z.string().max(40).optional(),
    rawValues: z
      .string()
      .min(1, "At least one value is required")
      .max(640, "Too many values"),
  })
  .superRefine((data, ctx) => {
    if (data.field === "http-header" && !data.httpHeaderName) {
      ctx.addIssue({
        code: "custom",
        path: ["httpHeaderName"],
        message: "Header name is required",
      })
    }
    const values = data.rawValues
      .split(/[\n,]/)
      .map((v) => v.trim())
      .filter((v) => v.length > 0)
    if (values.length === 0 || values.length > 5) {
      ctx.addIssue({
        code: "custom",
        path: ["rawValues"],
        message: "Enter 1 to 5 values, comma or newline separated",
      })
      return
    }
    for (const v of values) {
      if (!RULE_VALUE_REGEX.test(v)) {
        ctx.addIssue({
          code: "custom",
          path: ["rawValues"],
          message: `Value "${v}" contains whitespace or non-ASCII characters`,
        })
        return
      }
    }
  })

export type RuleConditionFormData = z.infer<typeof ruleConditionSchema>

export const createRuleSchema = z.object({
  priority: z
    .number()
    .int("Priority must be a whole number")
    .min(1, "Priority must be at least 1")
    .max(50_000, "Priority must be at most 50000"),
  conditions: z
    .array(ruleConditionSchema)
    .min(1, "At least one condition is required")
    .max(5, "Up to 5 conditions per rule"),
  forwardTargetGroupArn: z.string().min(1, "Target group is required"),
})

export type CreateRuleFormData = z.infer<typeof createRuleSchema>

// LB wizard form. The inline "new target group" option is driven by a separate
// `useForm<CreateTargetGroupFormData>` instance at the route level, so this
// schema only validates the existing-TG branch. See create-load-balancer.tsx.
export const createLoadBalancerSchema = z.object({
  name: lbNameField,
  scheme: z.enum(["internet-facing", "internal"]),
  vpcId: z.string().min(1, "VPC is required"),
  subnetIds: z
    .array(z.string())
    .min(2, "At least 2 subnets are required for an ALB"),
  securityGroupIds: z.array(z.string()),
  tags: z.array(tagSchema),
  listener: z
    .object({
      protocol: z.enum(["HTTP"]),
      port: portField,
      targetGroupMode: z.enum(["new", "existing"]),
      existingTargetGroupArn: z.string().optional(),
    })
    .refine(
      (value) =>
        value.targetGroupMode === "existing"
          ? !!value.existingTargetGroupArn
          : true,
      {
        error: "Target group is required",
        path: ["existingTargetGroupArn"],
      },
    ),
})

export type CreateLoadBalancerFormData = z.infer<
  typeof createLoadBalancerSchema
>

export const registerTargetsSchema = z.object({
  targets: z
    .array(
      z.object({
        instanceId: z.string().min(1, "Instance is required"),
        port: portField.optional(),
      }),
    )
    .min(1, "At least one target is required"),
})

export type RegisterTargetsFormData = z.infer<typeof registerTargetsSchema>

// Attributes editor state — free-form key/value pairs. Slice 7 narrows keys to
// DefaultLoadBalancerAttributes / DefaultTargetGroupAttributes.
export const attributesSchema = z.record(z.string(), z.string())

export type AttributesFormData = z.infer<typeof attributesSchema>

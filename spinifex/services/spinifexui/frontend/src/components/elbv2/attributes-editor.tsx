import { useMemo, useState } from "react"

import { Button } from "@/components/ui/button"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"

export type AttributeSpec =
  | {
      key: string
      label: string
      description?: string
      type: "bool"
    }
  | {
      key: string
      label: string
      description?: string
      type: "int"
      min?: number
      max?: number
    }
  | {
      key: string
      label: string
      description?: string
      type: "text"
      placeholder?: string
    }
  | {
      key: string
      label: string
      description?: string
      type: "select"
      options: readonly string[]
    }

export interface AttributesEditorAttribute {
  Key?: string
  Value?: string
}

interface AttributesEditorProps {
  specs: AttributeSpec[]
  attributes: AttributesEditorAttribute[]
  onSubmit: (changed: { key: string; value: string }[]) => void
  isPending: boolean
  error?: Error | null
  isSuccess?: boolean
}

function buildInitial(
  specs: AttributeSpec[],
  attributes: AttributesEditorAttribute[],
): Record<string, string> {
  const byKey = new Map<string, string>()
  for (const attr of attributes) {
    if (attr.Key !== undefined) {
      byKey.set(attr.Key, attr.Value ?? "")
    }
  }
  const out: Record<string, string> = {}
  for (const spec of specs) {
    out[spec.key] = byKey.get(spec.key) ?? ""
  }
  return out
}

function validate(spec: AttributeSpec, value: string): string | undefined {
  if (spec.type === "int") {
    if (value.trim() === "") {
      return "Value is required"
    }
    if (!/^-?\d+$/.test(value)) {
      return "Must be a whole number"
    }
    const n = Number(value)
    if (spec.min !== undefined && n < spec.min) {
      return `Must be at least ${spec.min}`
    }
    if (spec.max !== undefined && n > spec.max) {
      return `Must be at most ${spec.max}`
    }
  }
  return undefined
}

export function AttributesEditor({
  specs,
  attributes,
  onSubmit,
  isPending,
  error,
  isSuccess,
}: AttributesEditorProps) {
  const initial = useMemo(
    () => buildInitial(specs, attributes),
    [specs, attributes],
  )
  const [values, setValues] = useState(initial)

  const setValue = (key: string, value: string) => {
    setValues((prev) => ({ ...prev, [key]: value }))
  }

  const errors: Record<string, string | undefined> = {}
  for (const spec of specs) {
    errors[spec.key] = validate(spec, values[spec.key] ?? "")
  }
  const hasError = Object.values(errors).some(Boolean)

  const changed = specs
    .filter((s) => (values[s.key] ?? "") !== (initial[s.key] ?? ""))
    .map((s) => ({ key: s.key, value: values[s.key] ?? "" }))

  const handleReset = () => {
    setValues(initial)
  }

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault()
        if (hasError || changed.length === 0) {
          return
        }
        onSubmit(changed)
      }}
    >
      {error && (
        <div
          className="mb-4 rounded-md border border-destructive bg-destructive/10 p-3 text-sm text-destructive"
          role="alert"
        >
          {error.message || "Failed to save attributes"}
        </div>
      )}
      {isSuccess && changed.length === 0 && (
        <div
          className="mb-4 rounded-md border border-emerald-500 bg-emerald-500/10 p-3 text-sm text-emerald-700 dark:text-emerald-400"
          role="status"
        >
          Attributes saved.
        </div>
      )}
      <div className="rounded-lg border bg-card">
        <div className="grid gap-4 p-4 sm:grid-cols-2">
          {specs.map((spec) => {
            const id = `attr-${spec.key}`
            const value = values[spec.key] ?? ""
            const fieldError = errors[spec.key]
            return (
              <Field key={spec.key}>
                <FieldTitle>
                  <label htmlFor={id}>{spec.label}</label>
                </FieldTitle>
                {spec.type === "bool" && (
                  <input
                    aria-label={spec.label}
                    checked={value === "true"}
                    id={id}
                    onChange={(e) =>
                      setValue(spec.key, e.target.checked ? "true" : "false")
                    }
                    type="checkbox"
                  />
                )}
                {spec.type === "int" && (
                  <Input
                    aria-invalid={!!fieldError}
                    id={id}
                    inputMode="numeric"
                    onChange={(e) => setValue(spec.key, e.target.value)}
                    value={value}
                  />
                )}
                {spec.type === "text" && (
                  <Input
                    id={id}
                    onChange={(e) => setValue(spec.key, e.target.value)}
                    placeholder={spec.placeholder}
                    value={value}
                  />
                )}
                {spec.type === "select" && (
                  <Select
                    onValueChange={(v) => setValue(spec.key, v ?? "")}
                    value={value}
                  >
                    <SelectTrigger className="w-full" id={id}>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {spec.options.map((opt) => (
                        <SelectItem key={opt} value={opt}>
                          {opt}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                )}
                {spec.description && (
                  <p className="text-xs text-muted-foreground">
                    {spec.description}
                  </p>
                )}
                <FieldError
                  errors={[fieldError ? { message: fieldError } : undefined]}
                />
              </Field>
            )
          })}
        </div>
        <div className="flex items-center justify-end gap-2 border-t p-4">
          <span className="mr-auto text-xs text-muted-foreground">
            {changed.length === 0
              ? "No changes"
              : `${changed.length} change${changed.length === 1 ? "" : "s"} pending`}
          </span>
          <Button
            disabled={isPending || changed.length === 0}
            onClick={handleReset}
            size="sm"
            type="button"
            variant="secondary"
          >
            Reset
          </Button>
          <Button
            disabled={isPending || hasError || changed.length === 0}
            size="sm"
            type="submit"
          >
            {isPending ? "Saving…" : "Save changes"}
          </Button>
        </div>
      </div>
    </form>
  )
}

// Spec: DefaultLoadBalancerAttributes ALB subset. Keys must match
// handlers/elbv2/types.go:DefaultLoadBalancerAttributes or handler returns
// ValidationError.
export const albAttributeSpecs: AttributeSpec[] = [
  {
    key: "deletion_protection.enabled",
    label: "Deletion protection",
    type: "bool",
  },
  {
    key: "load_balancing.cross_zone.enabled",
    label: "Cross-zone load balancing",
    type: "bool",
  },
  {
    key: "idle_timeout.timeout_seconds",
    label: "Idle timeout (s)",
    type: "int",
    min: 1,
    max: 4000,
  },
  {
    key: "client_keep_alive.seconds",
    label: "Client keep-alive (s)",
    type: "int",
    min: 60,
    max: 604_800,
  },
  { key: "routing.http2.enabled", label: "HTTP/2", type: "bool" },
  {
    key: "routing.http.drop_invalid_header_fields.enabled",
    label: "Drop invalid header fields",
    type: "bool",
  },
  {
    key: "routing.http.preserve_host_header.enabled",
    label: "Preserve host header",
    type: "bool",
  },
  {
    key: "routing.http.xff_client_port.enabled",
    label: "XFF client port",
    type: "bool",
  },
  {
    key: "routing.http.xff_header_processing.mode",
    label: "XFF header processing",
    type: "select",
    options: ["append", "preserve", "remove"] as const,
  },
  {
    key: "routing.http.desync_mitigation_mode",
    label: "Desync mitigation mode",
    type: "select",
    options: ["defensive", "strictest", "monitor"] as const,
  },
  {
    key: "routing.http.x_amzn_tls_version_and_cipher_suite.enabled",
    label: "Add TLS version & cipher headers",
    type: "bool",
  },
  { key: "waf.fail_open.enabled", label: "WAF fail open", type: "bool" },
  {
    key: "zonal_shift.config.enabled",
    label: "Zonal shift",
    type: "bool",
  },
  {
    key: "access_logs.s3.enabled",
    label: "Access logs: enabled",
    type: "bool",
  },
  {
    key: "access_logs.s3.bucket",
    label: "Access logs: S3 bucket",
    type: "text",
  },
  {
    key: "access_logs.s3.prefix",
    label: "Access logs: S3 prefix",
    type: "text",
  },
  {
    key: "connection_logs.s3.enabled",
    label: "Connection logs: enabled",
    type: "bool",
  },
  {
    key: "connection_logs.s3.bucket",
    label: "Connection logs: S3 bucket",
    type: "text",
  },
  {
    key: "connection_logs.s3.prefix",
    label: "Connection logs: S3 prefix",
    type: "text",
  },
]

// Spec: DefaultTargetGroupAttributes. Keys must match
// handlers/elbv2/types.go:DefaultTargetGroupAttributes.
export const targetGroupAttributeSpecs: AttributeSpec[] = [
  {
    key: "deregistration_delay.timeout_seconds",
    label: "Deregistration delay (s)",
    type: "int",
    min: 0,
    max: 3600,
  },
  { key: "stickiness.enabled", label: "Stickiness", type: "bool" },
  {
    key: "stickiness.type",
    label: "Stickiness type",
    type: "select",
    options: ["lb_cookie", "source_ip"] as const,
  },
  {
    key: "stickiness.lb_cookie.duration_seconds",
    label: "Stickiness lb_cookie duration (s)",
    type: "int",
    min: 1,
    max: 604_800,
  },
  {
    key: "load_balancing.cross_zone.enabled",
    label: "Cross-zone load balancing",
    type: "select",
    options: ["true", "false", "use_load_balancer_configuration"] as const,
  },
  {
    key: "load_balancing.algorithm.type",
    label: "Load balancing algorithm",
    type: "select",
    options: ["round_robin", "least_outstanding_requests"] as const,
  },
  {
    key: "slow_start.duration_seconds",
    label: "Slow start duration (s)",
    type: "int",
    min: 0,
    max: 900,
  },
]

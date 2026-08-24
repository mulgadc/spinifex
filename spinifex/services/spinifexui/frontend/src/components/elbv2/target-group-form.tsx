import type { Vpc } from "@aws-sdk/client-ec2"
import type { UseFormReturn } from "react-hook-form"
import { Controller, useWatch } from "react-hook-form"

import { TagsFieldArray } from "@/components/tags-field-array"
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { getNameTag } from "@/lib/utils"
import type { CreateTargetGroupFormData } from "@/types/elbv2"

interface TargetGroupFormProps {
  form: UseFormReturn<CreateTargetGroupFormData>
  vpcs: Vpc[]
  allowedProtocols?: readonly string[]
}

const DEFAULT_ALLOWED_PROTOCOLS = [
  "HTTP",
  "HTTPS",
  "TCP",
  "UDP",
  "TLS",
  "TCP_UDP",
] as const

export function TargetGroupForm({
  form,
  vpcs,
  allowedProtocols = DEFAULT_ALLOWED_PROTOCOLS,
}: TargetGroupFormProps) {
  const {
    control,
    register,
    setValue,
    formState: { errors },
  } = form
  const protocol = useWatch({ control, name: "protocol" })
  // Path + Matcher only apply to HTTP(S) health checks; L4 target groups (TCP/
  // UDP/TLS) use a TCP health check that has neither.
  const httpHealthCheck = protocol === "HTTP" || protocol === "HTTPS"

  // Keep the health-check protocol consistent with the target-group protocol so
  // L4 groups submit a TCP health check and L7 groups submit an HTTP one.
  const handleProtocolChange = (
    next: CreateTargetGroupFormData["protocol"] | null,
  ) => {
    if (!next) {
      return
    }
    setValue("protocol", next)
    const layer7 = next === "HTTP" || next === "HTTPS"
    setValue("healthCheck.protocol", layer7 ? "HTTP" : "TCP")
  }

  return (
    <>
      <Field>
        <FieldTitle>
          <label htmlFor="tg-name">Name</label>
        </FieldTitle>
        <Input
          aria-invalid={!!errors.name}
          id="tg-name"
          placeholder="my-target-group"
          {...register("name")}
        />
        <FieldError errors={[errors.name]} />
      </Field>

      <Field>
        <FieldTitle>
          <label htmlFor="tg-protocol">Protocol</label>
        </FieldTitle>
        <Controller
          control={control}
          name="protocol"
          render={({ field }) => (
            <Select onValueChange={handleProtocolChange} value={field.value}>
              <SelectTrigger className="w-full" id="tg-protocol">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {allowedProtocols.map((p) => (
                  <SelectItem key={p} value={p}>
                    {p}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          )}
        />
      </Field>

      <Field>
        <FieldTitle>
          <label htmlFor="tg-port">Port</label>
        </FieldTitle>
        <Input
          aria-invalid={!!errors.port}
          id="tg-port"
          inputMode="numeric"
          placeholder="80"
          type="number"
          {...register("port", { valueAsNumber: true })}
        />
        <FieldError errors={[errors.port]} />
      </Field>

      <Field>
        <FieldTitle>
          <label htmlFor="tg-vpc">VPC</label>
        </FieldTitle>
        <Controller
          control={control}
          name="vpcId"
          render={({ field }) => (
            <Select
              onValueChange={(value) => {
                field.onChange(value)
              }}
              value={field.value ?? ""}
            >
              <SelectTrigger
                aria-invalid={!!errors.vpcId}
                className="w-full"
                id="tg-vpc"
              >
                <SelectValue placeholder="Select VPC" />
              </SelectTrigger>
              <SelectContent>
                {vpcs.map((vpc) => {
                  const name = getNameTag(vpc.Tags)
                  return (
                    <SelectItem key={vpc.VpcId} value={vpc.VpcId ?? ""}>
                      {name
                        ? `${vpc.VpcId} (${name})`
                        : `${vpc.VpcId} (${vpc.CidrBlock})`}
                    </SelectItem>
                  )
                })}
              </SelectContent>
            </Select>
          )}
        />
        <FieldError errors={[errors.vpcId]} />
      </Field>

      <Collapsible>
        <CollapsibleTrigger
          className="text-left text-sm font-medium text-muted-foreground hover:text-foreground"
          render={<button aria-label="Health check settings" type="button" />}
        >
          Health check settings
        </CollapsibleTrigger>
        <CollapsibleContent className="mt-3 space-y-4 border-l-2 border-muted pl-4">
          {httpHealthCheck && (
            <Field>
              <FieldTitle>
                <label htmlFor="hc-path">Path</label>
              </FieldTitle>
              <Input
                aria-invalid={!!errors.healthCheck?.path}
                id="hc-path"
                placeholder="/"
                {...register("healthCheck.path")}
              />
              <FieldError errors={[errors.healthCheck?.path]} />
            </Field>
          )}

          <Field>
            <FieldTitle>
              <label htmlFor="hc-port">Port</label>
            </FieldTitle>
            <Input
              aria-invalid={!!errors.healthCheck?.port}
              id="hc-port"
              placeholder="traffic-port or numeric"
              {...register("healthCheck.port")}
            />
            <FieldError errors={[errors.healthCheck?.port]} />
          </Field>

          <Field>
            <FieldTitle>
              <label htmlFor="hc-interval">Interval (seconds)</label>
            </FieldTitle>
            <Input
              aria-invalid={!!errors.healthCheck?.intervalSeconds}
              id="hc-interval"
              inputMode="numeric"
              type="number"
              {...register("healthCheck.intervalSeconds", {
                valueAsNumber: true,
              })}
            />
            <FieldError errors={[errors.healthCheck?.intervalSeconds]} />
          </Field>

          <Field>
            <FieldTitle>
              <label htmlFor="hc-timeout">Timeout (seconds)</label>
            </FieldTitle>
            <Input
              aria-invalid={!!errors.healthCheck?.timeoutSeconds}
              id="hc-timeout"
              inputMode="numeric"
              type="number"
              {...register("healthCheck.timeoutSeconds", {
                valueAsNumber: true,
              })}
            />
            <FieldError errors={[errors.healthCheck?.timeoutSeconds]} />
          </Field>

          <Field>
            <FieldTitle>
              <label htmlFor="hc-healthy">Healthy threshold</label>
            </FieldTitle>
            <Input
              aria-invalid={!!errors.healthCheck?.healthyThresholdCount}
              id="hc-healthy"
              inputMode="numeric"
              type="number"
              {...register("healthCheck.healthyThresholdCount", {
                valueAsNumber: true,
              })}
            />
            <FieldError errors={[errors.healthCheck?.healthyThresholdCount]} />
          </Field>

          <Field>
            <FieldTitle>
              <label htmlFor="hc-unhealthy">Unhealthy threshold</label>
            </FieldTitle>
            <Input
              aria-invalid={!!errors.healthCheck?.unhealthyThresholdCount}
              id="hc-unhealthy"
              inputMode="numeric"
              type="number"
              {...register("healthCheck.unhealthyThresholdCount", {
                valueAsNumber: true,
              })}
            />
            <FieldError
              errors={[errors.healthCheck?.unhealthyThresholdCount]}
            />
          </Field>

          {httpHealthCheck && (
            <Field>
              <FieldTitle>
                <label htmlFor="hc-matcher">Matcher (HTTP codes)</label>
              </FieldTitle>
              <Input
                aria-invalid={!!errors.healthCheck?.matcher}
                id="hc-matcher"
                placeholder="200 or 200-299 or 200,201"
                {...register("healthCheck.matcher")}
              />
              <FieldError errors={[errors.healthCheck?.matcher]} />
            </Field>
          )}
        </CollapsibleContent>
      </Collapsible>

      <TagsFieldArray control={control} name="tags" />
    </>
  )
}

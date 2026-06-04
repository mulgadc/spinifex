import type { Vpc } from "@aws-sdk/client-ec2"
import { Plus, Trash2 } from "lucide-react"
import type { UseFormReturn } from "react-hook-form"
import { Controller, useWatch } from "react-hook-form"

import { Button } from "@/components/ui/button"
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

export function TargetGroupForm({
  form,
  vpcs,
  allowedProtocols = ["HTTP"],
}: TargetGroupFormProps) {
  const {
    control,
    register,
    setValue,
    getValues,
    formState: { errors },
  } = form
  const tags = useWatch({ control, name: "tags" })
  const protocol = useWatch({ control, name: "protocol" })
  // Path + Matcher only apply to HTTP(S) health checks; L4 target groups (TCP/
  // UDP/TLS) use a TCP health check that has neither.
  const httpHealthCheck = protocol === "HTTP" || protocol === "HTTPS"

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
            <Select onValueChange={field.onChange} value={field.value}>
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
              onValueChange={(value) => field.onChange(value)}
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

      <Field>
        <FieldTitle>Tags</FieldTitle>
        <div className="space-y-2">
          {tags.map((_, index) => (
            // oxlint-disable-next-line react/no-array-index-key -- form array with no stable id
            <div className="flex items-center gap-2" key={index}>
              <Input placeholder="Key" {...register(`tags.${index}.key`)} />
              <Input placeholder="Value" {...register(`tags.${index}.value`)} />
              <Button
                onClick={() =>
                  setValue(
                    "tags",
                    getValues("tags").filter((__, i) => i !== index),
                  )
                }
                size="icon"
                type="button"
                variant="ghost"
              >
                <Trash2 className="size-3.5" />
              </Button>
            </div>
          ))}
          <Button
            onClick={() =>
              setValue("tags", [...getValues("tags"), { key: "", value: "" }])
            }
            size="sm"
            type="button"
            variant="outline"
          >
            <Plus className="size-3.5" />
            Add tag
          </Button>
        </div>
      </Field>
    </>
  )
}

import type { CertificateSummary } from "@aws-sdk/client-acm"
import type { Vpc } from "@aws-sdk/client-ec2"
import type {
  SslPolicy,
  TargetGroup,
} from "@aws-sdk/client-elastic-load-balancing-v2"
import { Controller, type UseFormReturn } from "react-hook-form"

import { TargetGroupForm } from "@/components/elbv2/target-group-form"
import { Button } from "@/components/ui/button"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { arnResourceId } from "@/lib/utils"
import {
  type CreateLoadBalancerFormData,
  type CreateTargetGroupFormData,
  type ListenerProtocolValue,
  DEFAULT_SSL_POLICY,
} from "@/types/elbv2"

type LbForm = UseFormReturn<CreateLoadBalancerFormData>

interface SecureListenerFieldsProps {
  form: LbForm
  listenerProtocol: ListenerProtocolValue
  certificates: CertificateSummary[]
  sslPolicies: SslPolicy[]
  onImportCertificate: () => void
}

// Certificate and, for HTTPS, security-policy fields of a secure listener.
function SecureListenerFields({
  form,
  listenerProtocol,
  certificates,
  sslPolicies,
  onImportCertificate,
}: SecureListenerFieldsProps) {
  const {
    control,
    formState: { errors },
  } = form

  return (
    <>
      <Field>
        <FieldTitle>
          <label htmlFor="listener-certificate">Certificate</label>
        </FieldTitle>
        <Controller
          control={control}
          name="listener.certificateArn"
          render={({ field }) => (
            <Select onValueChange={field.onChange} value={field.value ?? ""}>
              <SelectTrigger
                aria-invalid={!!errors.listener?.certificateArn}
                className="w-full"
                id="listener-certificate"
              >
                <SelectValue placeholder="Select certificate" />
              </SelectTrigger>
              <SelectContent>
                {certificates.map((cert) => (
                  <SelectItem
                    key={cert.CertificateArn}
                    value={cert.CertificateArn ?? ""}
                  >
                    {cert.DomainName ?? arnResourceId(cert.CertificateArn)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          )}
        />
        <FieldError errors={[errors.listener?.certificateArn]} />
        <div className="mt-1 flex items-center justify-between">
          {certificates.length === 0 && (
            <p className="text-xs text-muted-foreground">
              No certificates imported yet.
            </p>
          )}
          <Button
            className="ml-auto"
            onClick={onImportCertificate}
            size="sm"
            type="button"
            variant="ghost"
          >
            Import certificate…
          </Button>
        </div>
      </Field>

      {listenerProtocol === "HTTPS" && (
        <Field>
          <FieldTitle>
            <label htmlFor="listener-ssl-policy">Security policy</label>
          </FieldTitle>
          <Controller
            control={control}
            name="listener.sslPolicy"
            render={({ field }) => (
              <Select
                onValueChange={field.onChange}
                value={field.value ?? DEFAULT_SSL_POLICY}
              >
                <SelectTrigger className="w-full" id="listener-ssl-policy">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {sslPolicies.map((policy) => (
                    <SelectItem key={policy.Name} value={policy.Name ?? ""}>
                      {policy.Name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
          />
        </Field>
      )}
    </>
  )
}

// Picker for an existing target group in the selected VPC.
function ExistingTargetGroupField({
  form,
  vpcTgs,
}: {
  form: LbForm
  vpcTgs: TargetGroup[]
}) {
  const {
    control,
    formState: { errors },
  } = form

  return (
    <Field>
      <FieldTitle>
        <label htmlFor="tg-existing">Target group</label>
      </FieldTitle>
      <Controller
        control={control}
        name="listener.existingTargetGroupArn"
        render={({ field }) => (
          <Select onValueChange={field.onChange} value={field.value ?? ""}>
            <SelectTrigger
              aria-invalid={!!errors.listener?.existingTargetGroupArn}
              className="w-full"
              id="tg-existing"
            >
              <SelectValue placeholder="Select target group" />
            </SelectTrigger>
            <SelectContent>
              {vpcTgs.map((tg) => (
                <SelectItem
                  key={tg.TargetGroupArn}
                  value={tg.TargetGroupArn ?? ""}
                >
                  {tg.TargetGroupName} · {tg.Protocol}:{tg.Port}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        )}
      />
      <FieldError errors={[errors.listener?.existingTargetGroupArn]} />
      {vpcTgs.length === 0 && (
        <p className="mt-1 text-xs text-muted-foreground">
          No target groups in this VPC. Choose &ldquo;Create new&rdquo; instead.
        </p>
      )}
    </Field>
  )
}

interface CreateLoadBalancerListenerSectionProps {
  form: LbForm
  tgForm: UseFormReturn<CreateTargetGroupFormData>
  listenerProtocol: ListenerProtocolValue
  listenerProtocols: readonly ListenerProtocolValue[]
  isSecure: boolean
  tgMode: CreateLoadBalancerFormData["listener"]["targetGroupMode"]
  targetGroupProtocols: readonly string[]
  certificates: CertificateSummary[]
  sslPolicies: SslPolicy[]
  vpcTgs: TargetGroup[]
  vpcs: Vpc[]
  onProtocolChange: (next: ListenerProtocolValue) => void
  onImportCertificate: () => void
}

// The wizard's first listener and its default target group, new or existing.
export function CreateLoadBalancerListenerSection({
  form,
  tgForm,
  listenerProtocol,
  listenerProtocols,
  isSecure,
  tgMode,
  targetGroupProtocols,
  certificates,
  sslPolicies,
  vpcTgs,
  vpcs,
  onProtocolChange,
  onImportCertificate,
}: CreateLoadBalancerListenerSectionProps) {
  const {
    control,
    register,
    formState: { errors },
  } = form

  return (
    <div className="space-y-4 rounded-md border bg-card p-4">
      <h2 className="text-sm font-semibold">
        First listener &amp; default target group
      </h2>

      <Field>
        <FieldTitle>
          <label htmlFor="listener-protocol">Protocol</label>
        </FieldTitle>
        <Controller
          control={control}
          name="listener.protocol"
          render={({ field }) => (
            <Select
              onValueChange={(next: ListenerProtocolValue | null) => {
                if (next) {
                  onProtocolChange(next)
                }
              }}
              value={field.value}
            >
              <SelectTrigger className="w-full" id="listener-protocol">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {listenerProtocols.map((p) => (
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
          <label htmlFor="listener-port">Port</label>
        </FieldTitle>
        <Input
          aria-invalid={!!errors.listener?.port}
          id="listener-port"
          inputMode="numeric"
          type="number"
          {...register("listener.port", { valueAsNumber: true })}
        />
        <FieldError errors={[errors.listener?.port]} />
      </Field>

      {isSecure && (
        <SecureListenerFields
          certificates={certificates}
          form={form}
          listenerProtocol={listenerProtocol}
          onImportCertificate={onImportCertificate}
          sslPolicies={sslPolicies}
        />
      )}

      <Field>
        <FieldTitle>Default target group</FieldTitle>
        <Controller
          control={control}
          name="listener.targetGroupMode"
          render={({ field }) => (
            <RadioGroup
              aria-label="Default target group"
              className="flex w-auto"
              onValueChange={field.onChange}
              value={field.value}
            >
              <Label>
                <RadioGroupItem
                  aria-label="Create new target group"
                  value="new"
                />
                Create new
              </Label>
              <Label>
                <RadioGroupItem
                  aria-label="Use existing target group"
                  value="existing"
                />
                Use existing
              </Label>
            </RadioGroup>
          )}
        />
      </Field>

      {tgMode === "existing" && (
        <ExistingTargetGroupField form={form} vpcTgs={vpcTgs} />
      )}

      {tgMode === "new" && (
        <div className="space-y-4 border-l-2 border-muted pl-4">
          <TargetGroupForm
            allowedProtocols={targetGroupProtocols}
            form={tgForm}
            vpcs={vpcs}
          />
        </div>
      )}
    </div>
  )
}

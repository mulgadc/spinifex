import type { CertificateSummary } from "@aws-sdk/client-acm"
import type {
  SslPolicy,
  TargetGroup,
} from "@aws-sdk/client-elastic-load-balancing-v2"
import { type Control, Controller, type FieldErrors } from "react-hook-form"

import { Button } from "@/components/ui/button"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
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
  DEFAULT_SSL_POLICY,
} from "@/types/elbv2"

interface SecureListenerFieldsProps {
  certificates: CertificateSummary[]
  control: Control<CreateLoadBalancerFormData>
  errors: FieldErrors<CreateLoadBalancerFormData>
  onImportClick: () => void
  showSslPolicy: boolean
  sslPolicies: SslPolicy[]
}

export function SecureListenerFields({
  certificates,
  control,
  errors,
  onImportClick,
  showSslPolicy,
  sslPolicies,
}: SecureListenerFieldsProps) {
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
            onClick={onImportClick}
            size="sm"
            type="button"
            variant="ghost"
          >
            Import certificate…
          </Button>
        </div>
      </Field>

      {showSslPolicy && (
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

interface ExistingTargetGroupFieldProps {
  control: Control<CreateLoadBalancerFormData>
  errors: FieldErrors<CreateLoadBalancerFormData>
  targetGroups: TargetGroup[]
}

export function ExistingTargetGroupField({
  control,
  errors,
  targetGroups,
}: ExistingTargetGroupFieldProps) {
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
              {targetGroups.map((tg) => (
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
      {targetGroups.length === 0 && (
        <p className="mt-1 text-xs text-muted-foreground">
          No target groups in this VPC. Choose &ldquo;Create new&rdquo; instead.
        </p>
      )}
    </Field>
  )
}

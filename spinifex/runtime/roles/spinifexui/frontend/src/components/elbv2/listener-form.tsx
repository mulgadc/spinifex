import type { CertificateSummary } from "@aws-sdk/client-acm"
import type {
  SslPolicy,
  TargetGroup,
} from "@aws-sdk/client-elastic-load-balancing-v2"
import { useState } from "react"
import type { UseFormReturn } from "react-hook-form"
import { Controller, useFormState, useWatch } from "react-hook-form"

import { CertificateImportForm } from "@/components/elbv2/certificate-import-form"
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
import { arnResourceId } from "@/lib/utils"
import {
  type CreateListenerFormData,
  DEFAULT_SSL_POLICY,
  isSecureProtocol,
  type ListenerProtocolValue,
} from "@/types/elbv2"

interface ListenerFormProps {
  form: UseFormReturn<CreateListenerFormData>
  protocols: readonly ListenerProtocolValue[]
  targetGroups: TargetGroup[]
  certificates?: CertificateSummary[]
  sslPolicies?: SslPolicy[]
}

const HTTP_PORT = 80
const HTTPS_PORT = 443
const EMPTY_CERTIFICATES: CertificateSummary[] = []
const EMPTY_SSL_POLICIES: SslPolicy[] = []

function certLabel(cert: CertificateSummary): string {
  const id = arnResourceId(cert.CertificateArn)

  return cert.DomainName ? `${cert.DomainName} · ${id}` : id
}

export interface ListenerProtocolFields {
  port: number
  certificateArn?: string
  sslPolicy?: string
}

// The port and TLS fields a protocol switch implies. The port moves only off
// the other protocol's default; an SSL policy is HTTPS-only, and an insecure
// protocol drops cert + policy so the server doesn't reject them.
export function listenerProtocolDefaults(
  next: ListenerProtocolValue,
  current: ListenerProtocolFields,
): ListenerProtocolFields {
  if (!isSecureProtocol(next)) {
    return {
      port: current.port === HTTPS_PORT ? HTTP_PORT : current.port,
      certificateArn: undefined,
      sslPolicy: undefined,
    }
  }

  const sslPolicy =
    next === "HTTPS" ? current.sslPolicy || DEFAULT_SSL_POLICY : undefined

  return {
    port: current.port === HTTP_PORT ? HTTPS_PORT : current.port,
    certificateArn: current.certificateArn,
    sslPolicy,
  }
}

export function ListenerForm({
  form,
  protocols,
  targetGroups,
  certificates = EMPTY_CERTIFICATES,
  sslPolicies = EMPTY_SSL_POLICIES,
}: ListenerFormProps) {
  const { control, register, getValues, setValue } = form
  const { errors } = useFormState({ control })

  const [importOpen, setImportOpen] = useState(false)

  const protocol = useWatch({ control, name: "protocol" })
  const isHttps = protocol === "HTTPS"
  const isSecure = isSecureProtocol(protocol)

  const applyProtocolDefaults = (next: ListenerProtocolValue | null) => {
    if (!next) {
      return
    }

    const fields = listenerProtocolDefaults(next, getValues())
    setValue("port", fields.port)
    setValue("certificateArn", fields.certificateArn)
    setValue("sslPolicy", fields.sslPolicy)
  }

  return (
    <>
      <Field>
        <FieldTitle>
          <label htmlFor="listener-protocol">Protocol</label>
        </FieldTitle>
        <Controller
          control={control}
          name="protocol"
          render={({ field }) => (
            <Select
              onValueChange={(next: ListenerProtocolValue | null) => {
                field.onChange(next)
                applyProtocolDefaults(next)
              }}
              value={field.value}
            >
              <SelectTrigger className="w-full" id="listener-protocol">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {protocols.map((p) => (
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
          aria-invalid={!!errors.port}
          id="listener-port"
          inputMode="numeric"
          placeholder={isSecure ? "443" : "80"}
          type="number"
          {...register("port", { valueAsNumber: true })}
        />
        <FieldError errors={[errors.port]} />
      </Field>

      {isSecure && (
        <>
          <Field>
            <FieldTitle>
              <label htmlFor="listener-certificate">Certificate</label>
            </FieldTitle>
            <Controller
              control={control}
              name="certificateArn"
              render={({ field }) => (
                <Select
                  onValueChange={field.onChange}
                  value={field.value ?? ""}
                >
                  <SelectTrigger
                    aria-invalid={!!errors.certificateArn}
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
                        {certLabel(cert)}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              )}
            />
            <FieldError errors={[errors.certificateArn]} />
            <div className="mt-1 flex items-center justify-between">
              {certificates.length === 0 && (
                <p className="text-xs text-muted-foreground">
                  No certificates imported yet.
                </p>
              )}
              <Button
                className="ml-auto"
                onClick={() => {
                  setImportOpen(true)
                }}
                size="sm"
                type="button"
                variant="ghost"
              >
                Import certificate…
              </Button>
            </div>
          </Field>

          {isHttps && (
            <Field>
              <FieldTitle>
                <label htmlFor="listener-ssl-policy">Security policy</label>
              </FieldTitle>
              <Controller
                control={control}
                name="sslPolicy"
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

          {importOpen && (
            <CertificateImportForm
              onClose={() => {
                setImportOpen(false)
              }}
              onImported={(arn) => {
                setValue("certificateArn", arn, { shouldValidate: true })
              }}
            />
          )}
        </>
      )}

      <Field>
        <FieldTitle>
          <label htmlFor="listener-default-tg">Default target group</label>
        </FieldTitle>
        <Controller
          control={control}
          name="defaultTargetGroupArn"
          render={({ field }) => (
            <Select onValueChange={field.onChange} value={field.value ?? ""}>
              <SelectTrigger
                aria-invalid={!!errors.defaultTargetGroupArn}
                className="w-full"
                id="listener-default-tg"
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
        <FieldError errors={[errors.defaultTargetGroupArn]} />
        {targetGroups.length === 0 && (
          <p className="mt-1 text-xs text-muted-foreground">
            No target groups available in this VPC. Create one first.
          </p>
        )}
      </Field>
    </>
  )
}

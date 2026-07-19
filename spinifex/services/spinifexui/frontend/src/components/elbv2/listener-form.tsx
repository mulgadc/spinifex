import type { CertificateSummary } from "@aws-sdk/client-acm"
import type {
  SslPolicy,
  TargetGroup,
} from "@aws-sdk/client-elastic-load-balancing-v2"
import { useState } from "react"
import type { UseFormReturn } from "react-hook-form"
import { Controller, useWatch } from "react-hook-form"

import { CertificateImportDialog } from "@/components/elbv2/certificate-import-dialog"
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
  const id = cert.CertificateArn?.split("/").pop() ?? cert.CertificateArn ?? ""
  return cert.DomainName ? `${cert.DomainName} · ${id}` : id
}

export function ListenerForm({
  form,
  protocols,
  targetGroups,
  certificates = EMPTY_CERTIFICATES,
  sslPolicies = EMPTY_SSL_POLICIES,
}: ListenerFormProps) {
  const {
    control,
    register,
    getValues,
    setValue,
    formState: { errors },
  } = form

  const [importOpen, setImportOpen] = useState(false)

  const protocol = useWatch({ control, name: "protocol" })
  const isHttps = protocol === "HTTPS"
  const isSecure = isSecureProtocol(protocol)

  // Side-effects of a protocol switch, applied after the field's own onChange:
  // bump the default port (only when still on the other protocol's default) and
  // toggle the TLS fields. A secure protocol (HTTPS/TLS) keeps the certificate;
  // an SSL policy is HTTPS-only. Moving to an insecure protocol clears cert +
  // policy so the server doesn't reject them.
  const applyProtocolDefaults = (next: ListenerProtocolValue | null) => {
    if (!next) {
      return
    }
    const current = getValues("port")
    if (isSecureProtocol(next)) {
      if (current === HTTP_PORT) {
        setValue("port", HTTPS_PORT)
      }
      if (next === "HTTPS") {
        if (!getValues("sslPolicy")) {
          setValue("sslPolicy", DEFAULT_SSL_POLICY)
        }
      } else {
        setValue("sslPolicy", undefined)
      }
    } else {
      if (current === HTTPS_PORT) {
        setValue("port", HTTP_PORT)
      }
      setValue("certificateArn", undefined)
      setValue("sslPolicy", undefined)
    }
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
                onClick={() => setImportOpen(true)}
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

          <CertificateImportDialog
            onImported={(arn) =>
              setValue("certificateArn", arn, { shouldValidate: true })
            }
            onOpenChange={setImportOpen}
            open={importOpen}
          />
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

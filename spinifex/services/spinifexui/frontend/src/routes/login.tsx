import { zodResolver } from "@hookform/resolvers/zod"
import {
  createFileRoute,
  redirect,
  type SearchSchemaInput,
} from "@tanstack/react-router"
import { useState } from "react"
import { useForm } from "react-hook-form"

import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible"
import {
  Field,
  FieldError,
  FieldGroup,
  FieldTitle,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  type AwsCredentialsInput,
  awsCredentialsSchema,
  getCredentials,
  setSessionCredentials,
} from "@/lib/auth"
import { clearClients } from "@/lib/awsClient"
import { exchangeForSession } from "@/lib/sts"

export const Route = createFileRoute("/login")({
  validateSearch: (search: { reason?: string } & SearchSchemaInput) => ({
    reason: search.reason === "expired" ? ("expired" as const) : undefined,
  }),
  beforeLoad: () => {
    if (getCredentials()) {
      throw redirect({ to: "/" })
    }
  },
  component: LoginPage,
})

function LoginPage() {
  const { reason } = Route.useSearch()
  const [authError, setAuthError] = useState<string | null>(null)
  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm({
    resolver: zodResolver(awsCredentialsSchema),
  })

  async function onSubmit(data: AwsCredentialsInput) {
    setAuthError(null)
    try {
      // Exchange the long-lived creds for short-lived session creds; this both
      // validates them and yields the session token. The long-lived secret is
      // never persisted.
      const session = await exchangeForSession(data)
      setSessionCredentials(session)
      // Drop cached clients so they rebuild with the new session creds.
      clearClients()
      window.location.assign("/")
    } catch {
      setAuthError(
        "Invalid credentials. Please check your Access Key ID and Secret Access Key.",
      )
    }
  }

  return (
    <div className="flex flex-1 items-center justify-center">
      <div className="flex w-full max-w-sm flex-col gap-4">
        <Card>
          <CardHeader>
            <CardTitle>AWS Credentials</CardTitle>
          </CardHeader>
          <CardContent>
            {reason === "expired" && !authError && (
              <p className="mb-4 rounded-md bg-muted p-3 text-sm text-muted-foreground">
                Your session is no longer valid — please sign in again.
              </p>
            )}
            {authError && (
              <p className="mb-4 rounded-md bg-destructive/10 p-3 text-sm text-destructive">
                {authError}
              </p>
            )}
            <form onSubmit={handleSubmit(onSubmit)}>
              <FieldGroup>
                <Field>
                  <FieldTitle>
                    <label htmlFor="accessKeyId">Access Key ID</label>
                  </FieldTitle>
                  <Input
                    aria-invalid={!!errors.accessKeyId}
                    autoComplete="username"
                    id="accessKeyId"
                    placeholder="AKIAIOSFODNN7EXAMPLE"
                    {...register("accessKeyId")}
                  />
                  <FieldError errors={[errors.accessKeyId]} />
                </Field>
                <Field>
                  <FieldTitle>
                    <label htmlFor="secretAccessKey">Secret Access Key</label>
                  </FieldTitle>
                  <Input
                    aria-invalid={!!errors.secretAccessKey}
                    autoComplete="current-password"
                    id="secretAccessKey"
                    placeholder="wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
                    type="password"
                    {...register("secretAccessKey")}
                  />
                  <FieldError errors={[errors.secretAccessKey]} />
                </Field>
                <Button
                  className="w-full"
                  disabled={isSubmitting}
                  type="submit"
                >
                  {isSubmitting ? "Signing in..." : "Sign In"}
                </Button>
              </FieldGroup>
            </form>
          </CardContent>
        </Card>

        <CertificateInstallBanner />
      </div>
    </div>
  )
}

function CertificateInstallBanner() {
  return (
    <Card className="border-muted bg-muted/30">
      <CardContent className="pt-4 pb-4">
        <div className="flex flex-col gap-3">
          <div>
            <p className="text-sm font-medium">
              Install Local Certificate Authority
            </p>
            <p className="text-xs text-muted-foreground">
              To avoid browser certificate warnings, download and install the
              Spinifex CA as a trusted root on your OS.
            </p>
          </div>
          <a
            className="inline-flex h-8 w-full items-center justify-center rounded-md border border-input bg-background px-3 text-sm font-medium hover:bg-accent hover:text-accent-foreground"
            download="spinifex-ca.pem"
            href="/api/ca.pem"
          >
            Download CA Certificate
          </a>
          <Collapsible>
            <CollapsibleTrigger className="cursor-pointer text-xs text-muted-foreground underline-offset-4 hover:underline">
              Installation instructions
            </CollapsibleTrigger>
            <CollapsibleContent>
              <div className="mt-2 flex flex-col gap-2 text-xs text-muted-foreground">
                <div>
                  <p className="font-medium text-foreground">macOS</p>
                  <code className="mt-1 block rounded bg-muted p-2 text-[11px] break-all">
                    sudo security add-trusted-cert -d -r trustRoot -k
                    /Library/Keychains/System.keychain spinifex-ca.pem
                  </code>
                </div>
                <div>
                  <p className="font-medium text-foreground">Windows</p>
                  <p>
                    Double-click the file &rarr; Install Certificate &rarr;
                    Local Machine &rarr; Trusted Root Certification Authorities
                  </p>
                </div>
                <div>
                  <p className="font-medium text-foreground">Linux</p>
                  <code className="mt-1 block rounded bg-muted p-2 text-[11px] break-all">
                    sudo cp spinifex-ca.pem
                    /usr/local/share/ca-certificates/spinifex-ca.crt && sudo
                    update-ca-certificates
                  </code>
                </div>
              </div>
            </CollapsibleContent>
          </Collapsible>
        </div>
      </CardContent>
    </Card>
  )
}

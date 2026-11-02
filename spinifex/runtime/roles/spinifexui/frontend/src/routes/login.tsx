import { zodResolver } from "@hookform/resolvers/zod"
import { useQueryClient } from "@tanstack/react-query"
import {
  createFileRoute,
  redirect,
  useNavigate,
  useRouter,
} from "@tanstack/react-router"
import type { ReactNode } from "react"
import { useEffect, useState } from "react"
import { useForm } from "react-hook-form"
import { z } from "zod"

import { Button } from "@/components/ui/button"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
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
import { startConsoleHandoff } from "@/lib/console-handoff"
import { isDirectHostAccess } from "@/lib/host-access"
import { exchangeForSession } from "@/lib/sts"
import { cn } from "@/lib/utils"

// How long the sign-in card and its veil take to fade as the console comes in.
const LEAVE_MS = 300

// Any reason other than session expiry or sign-out is dropped,
// so a hand-typed value cannot put copy on the login page.
/* oxlint-disable promise/prefer-await-to-then, unicorn/no-useless-undefined -- zod's .catch() supplies a schema fallback, not a promise handler */
const searchSchema = z.object({
  reason: z.enum(["expired", "signed-out"]).optional().catch(undefined),
  // The router's search parser coerces `handoff=1` to the number 1, so accept
  // either and normalise. Matching only the string strips the param from the
  // URL and the handoff then silently never runs.
  handoff: z
    .union([z.literal("1"), z.literal(1)])
    .optional()
    .transform((v) => (v === undefined ? undefined : ("1" as const)))
    .catch(undefined),
})
/* oxlint-enable promise/prefer-await-to-then, unicorn/no-useless-undefined */

export const Route = createFileRoute("/login")({
  validateSearch: searchSchema,
  beforeLoad: () => {
    if (getCredentials()) {
      throw redirect({ to: "/" })
    }
  },
  component: LoginPage,
})

function LoginPage() {
  const { reason, handoff } = Route.useSearch()
  const [authError, setAuthError] = useState<string | null>(null)
  // When mulgadc.com/signup opens us with ?handoff=1 it will postMessage the
  // new account's credentials; receive them and sign in without the form. If
  // nothing arrives (opened directly, or the exchange fails) we fall back to
  // the form, so this is invisible when unused.
  const [handingOff, setHandingOff] = useState(handoff === "1")
  const [leaving, setLeaving] = useState(false)
  const navigate = useNavigate()
  const router = useRouter()
  const queryClient = useQueryClient()
  useEffect(
    () =>
      handoff === "1"
        ? startConsoleHandoff({
            onFailure: () => {
              setHandingOff(false)
              setAuthError(
                "We couldn't sign you in automatically. Please enter your credentials.",
              )
            },
          })
        : undefined,
    [handoff],
  )

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
      // Drop cached clients and queries, so nothing from before sign-in
      // survives into the session.
      clearClients()
      queryClient.clear()
    } catch {
      setAuthError(
        "Invalid credentials. Please check your Access Key ID and Secret Access Key.",
      )

      return
    }

    // Fade the card and its veil while the dashboard loads, then step into it
    // in place rather than reloading the page.
    setLeaving(true)
    void router.preloadRoute({ to: "/" })

    const reduced = window.matchMedia("(prefers-reduced-motion: reduce)")
    window.setTimeout(
      () => {
        void navigate({ to: "/" })
      },
      reduced.matches ? 0 : LEAVE_MS,
    )
  }

  if (handingOff) {
    return (
      <Veil leaving={false}>
        <Card>
          <CardHeader>
            <CardTitle>Signing you in…</CardTitle>
            <CardDescription>
              Connecting your new Spinifex account to the console.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <p className="text-sm text-muted-foreground">
              One moment — no need to copy your keys.
            </p>
          </CardContent>
        </Card>
      </Veil>
    )
  }

  return (
    <Veil leaving={leaving}>
      <Card>
        <CardHeader>
          <CardTitle>Spinifex Credentials</CardTitle>
          <CardDescription>
            Use the credentials provided with your Spinifex account.
          </CardDescription>
        </CardHeader>
        <CardContent>
          {reason === "signed-out" && !authError && (
            <output className="mb-4 block rounded-md border border-warning-border bg-warning-subtle p-3 text-sm">
              Signed out.
            </output>
          )}
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
                disabled={isSubmitting || leaving}
                type="submit"
              >
                {isSubmitting ? "Signing in..." : "Sign In"}
              </Button>
            </FieldGroup>
          </form>
        </CardContent>
      </Card>

      {isDirectHostAccess(window.location.hostname) && (
        <CertificateInstallBanner />
      )}
    </Veil>
  )
}

// Dims and lightly blurs the shell behind the sign-in card. Both fade in on
// arrival, and leaving fades them away so the console comes up through them.
function Veil({
  leaving,
  children,
}: {
  leaving: boolean
  children: ReactNode
}) {
  return (
    <div className="flex flex-1 items-center justify-center">
      <div
        aria-hidden="true"
        className={cn(
          "fixed inset-0 -z-10 bg-background/40 backdrop-blur-veil transition-opacity duration-300 motion-reduce:transition-none starting:opacity-0",
          leaving && "opacity-0",
        )}
      />
      <div
        className={cn(
          "flex w-full max-w-sm flex-col gap-4 transition duration-300 ease-out motion-reduce:transition-none starting:scale-95 starting:opacity-0",
          leaving && "scale-95 opacity-0",
        )}
      >
        {children}
      </div>
    </div>
  )
}

function CertificateInstallBanner() {
  return (
    <Card variant="muted">
      <CardContent className="my-4">
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
            <CollapsibleTrigger variant="link">
              Installation instructions
            </CollapsibleTrigger>
            <CollapsibleContent>
              <div className="mt-2 flex flex-col gap-2 text-xs text-muted-foreground">
                <div>
                  <p className="font-medium text-foreground">macOS</p>
                  <code className="mt-1 block rounded bg-muted p-2 text-code-block break-all">
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
                  <code className="mt-1 block rounded bg-muted p-2 text-code-block break-all">
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

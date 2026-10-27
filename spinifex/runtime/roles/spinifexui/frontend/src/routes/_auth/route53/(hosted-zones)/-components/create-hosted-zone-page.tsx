import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery } from "@tanstack/react-query"
import { Link, useNavigate } from "@tanstack/react-router"
import { CircleCheck } from "lucide-react"
import { useState } from "react"
import { useForm } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { TextField } from "@/components/form-fields"
import { PageHeading } from "@/components/page-heading"
import { ActivationPanel } from "@/components/route53/activation-panel"
import { NameServerList } from "@/components/route53/name-server-list"
import { Button } from "@/components/ui/button"
import { bareZoneId } from "@/lib/route53-activation"
import { cn } from "@/lib/utils"
import { useCreateHostedZone } from "@/mutations/route53"
import { route53ActivationPollingQueryOptions } from "@/queries/route53"
import {
  type CreateHostedZoneFormData,
  createHostedZoneSchema,
} from "@/types/route53"

const STEPS = ["Domain", "Name servers", "Verification"] as const

type Step = 1 | 2 | 3

interface CreatedZone {
  id: string
  name: string
  nameServers: string[]
}

function StepIndicator({ current }: { current: Step }) {
  return (
    <ol className="mb-6 flex gap-4 text-sm" aria-label="Steps">
      {STEPS.map((label, index) => {
        const step = index + 1

        return (
          <li
            aria-current={step === current ? "step" : undefined}
            className={cn(
              "flex items-center gap-2",
              step === current ? "font-medium" : "text-muted-foreground",
            )}
            key={label}
          >
            <span
              className={cn(
                "flex size-6 items-center justify-center rounded-full border text-xs",
                step <= current &&
                  "border-primary bg-primary text-primary-foreground",
              )}
            >
              {step}
            </span>
            {label}
          </li>
        )
      })}
    </ol>
  )
}

function DomainStep({ onCreated }: { onCreated: (zone: CreatedZone) => void }) {
  const navigate = useNavigate()
  const createMutation = useCreateHostedZone()

  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm<CreateHostedZoneFormData>({
    resolver: zodResolver(createHostedZoneSchema),
    defaultValues: { name: "", comment: "" },
  })

  const onSubmit = async (data: CreateHostedZoneFormData) => {
    try {
      const response = await createMutation.mutateAsync(data)
      const zone = response.HostedZone

      onCreated({
        id: bareZoneId(zone?.Id ?? ""),
        name: zone?.Name ?? data.name,
        nameServers: response.DelegationSet?.NameServers ?? [],
      })
    } catch {
      // surfaced via mutation.error
    }
  }

  return (
    <form className="max-w-2xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
      {createMutation.error && (
        <ErrorBanner
          error={createMutation.error}
          msg="The domain could not be added"
        />
      )}
      <TextField
        description="The domain you own, such as example.com. Its DNS will be served by this cluster once the registrar points at it."
        error={errors.name}
        id="zone-name"
        label="Domain name"
        placeholder="example.com"
        {...register("name")}
      />
      <TextField
        error={errors.comment}
        id="zone-comment"
        label="Comment (optional)"
        {...register("comment")}
      />
      <FormActions
        isPending={createMutation.isPending}
        isSubmitting={isSubmitting}
        onCancel={async () => {
          await navigate({ to: "/route53/list-hosted-zones" })
        }}
        pendingLabel="Adding…"
        submitLabel="Add domain"
      />
    </form>
  )
}

function NameServersStep({
  zone,
  onContinue,
}: {
  zone: CreatedZone
  onContinue: () => void
}) {
  return (
    <div className="max-w-2xl space-y-4">
      <p className="text-sm">
        Set these as the name servers for <strong>{zone.name}</strong> at your
        registrar.
      </p>
      <NameServerList nameServers={zone.nameServers} />
      <p className="text-sm text-muted-foreground">
        Load your records into the zone first, so nothing stops resolving when
        the registrar switches over. Records are not served until the delegation
        is verified.
      </p>
      <div className="flex items-center gap-4">
        <Button onClick={onContinue} type="button">
          I have updated my name servers
        </Button>
        <Link
          className="text-sm text-primary hover:underline"
          params={{ id: zone.id }}
          to="/route53/list-hosted-zones/$id"
        >
          Skip and add records now
        </Link>
      </div>
    </div>
  )
}

function VerificationStep({
  zone,
  onBack,
}: {
  zone: CreatedZone
  onBack: () => void
}) {
  const { data: activation } = useQuery(
    route53ActivationPollingQueryOptions(zone.id),
  )

  const isActive = activation?.state === "ACTIVE"

  return (
    <div className="max-w-4xl space-y-4">
      {isActive ? (
        <div className="flex items-center gap-3 rounded-md border border-success bg-success/10 p-4 text-success-foreground">
          <CircleCheck className="size-5" />
          <div>
            <p className="font-medium">Registered</p>
            <p className="text-sm">
              {zone.name} is delegated to this cluster and its records are
              served.{" "}
              <Link
                className="underline"
                params={{ id: zone.id }}
                to="/route53/list-hosted-zones/$id"
              >
                Go to the zone
              </Link>
            </p>
          </div>
        </div>
      ) : (
        <p className="text-sm text-muted-foreground">
          Waiting for the parent zone to delegate {zone.name} to the name
          servers above. Registrars can take from minutes to a day to publish
          the change; this page checks every few seconds.
        </p>
      )}
      <ActivationPanel zoneId={zone.id} />
      {!isActive && (
        <Button onClick={onBack} type="button" variant="outline">
          Back to name servers
        </Button>
      )}
    </div>
  )
}

export function CreateHostedZonePage() {
  const [step, setStep] = useState<Step>(1)
  const [zone, setZone] = useState<CreatedZone | undefined>()

  return (
    <>
      <BackLink to="/route53/list-hosted-zones">Back to hosted zones</BackLink>
      <PageHeading title="Add domain" />
      <StepIndicator current={step} />

      {step === 1 && (
        <DomainStep
          onCreated={(created) => {
            setZone(created)
            setStep(2)
          }}
        />
      )}
      {step === 2 && zone && (
        <NameServersStep
          onContinue={() => {
            setStep(3)
          }}
          zone={zone}
        />
      )}
      {step === 3 && zone && (
        <VerificationStep
          onBack={() => {
            setStep(2)
          }}
          zone={zone}
        />
      )}
    </>
  )
}

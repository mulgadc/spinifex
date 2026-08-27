import { useQuery } from "@tanstack/react-query"

import { PageHeading } from "@/components/page-heading"
import { Badge } from "@/components/ui/badge"
import { Card, CardContent } from "@/components/ui/card"
import { formatVRAMMiB } from "@/lib/utils"
import {
  adminOchreCatalogQueryOptions,
  type AdminCatalogEntry,
  type CatalogAvailability,
} from "@/queries/admin"

// Self-host entries always carry an instance type; the provider-direct tier
// (unused in v1, kept for a later phase) leaves it empty.
function isSelfHost(entry: AdminCatalogEntry): boolean {
  return entry.instanceType !== ""
}

function modalityBadges(entry: AdminCatalogEntry) {
  const modalities = [
    ...new Set([...entry.inputModalities, ...entry.outputModalities]),
  ]
  return modalities.map((modality) => (
    <Badge className="text-[0.625rem]" key={modality} variant="outline">
      {modality}
    </Badge>
  ))
}

function formatMicroUsdPerMillion(microUsd: number): string {
  return `$${(microUsd / 1_000_000).toFixed(2)}`
}

function PriceCell({ entry }: { entry: AdminCatalogEntry }) {
  if (isSelfHost(entry)) {
    return (
      <Badge className="text-[0.625rem]" variant="secondary">
        Included
      </Badge>
    )
  }
  if (!entry.priceKnown) {
    return <span className="text-muted-foreground">Price unknown</span>
  }
  return (
    <span className="font-mono">
      {formatMicroUsdPerMillion(entry.inputPriceMicroUsdPerMillion)} in /{" "}
      {formatMicroUsdPerMillion(entry.outputPriceMicroUsdPerMillion)} out per
      MTok
    </span>
  )
}

const AVAILABILITY_LABEL = {
  available: "Available",
  ungranted: "Ungranted",
  "no-weights-staged": "No weights staged",
  "no-credential": "No credential",
} satisfies Record<CatalogAvailability, string>

// AVAILABILITY_HINT is the fix affordance an operator can act on for each
// unavailable reason; "available" has none.
const AVAILABILITY_HINT = {
  available: undefined,
  ungranted: "Grant access to enable this model",
  "no-weights-staged": "Stage weights to enable this model",
  "no-credential": "Add a provider credential to enable this model",
} satisfies Record<CatalogAvailability, string | undefined>

function availabilityVariant(
  availability: CatalogAvailability,
): "default" | "secondary" | "destructive" {
  if (availability === "available") {
    return "default"
  }
  if (availability === "ungranted") {
    return "secondary"
  }
  return "destructive"
}

function AvailabilityCell({
  availability,
}: {
  availability: CatalogAvailability
}) {
  const hint = AVAILABILITY_HINT[availability]
  return (
    <div>
      <Badge
        className="text-[0.625rem]"
        variant={availabilityVariant(availability)}
      >
        {AVAILABILITY_LABEL[availability]}
      </Badge>
      {hint && (
        <p className="mt-1 text-[0.625rem] text-muted-foreground">{hint}</p>
      )}
    </div>
  )
}

function CatalogTable({ entries }: { entries: AdminCatalogEntry[] }) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-xs">
        <thead>
          <tr className="border-b text-left text-muted-foreground">
            <th className="pr-4 pb-1 font-medium">Model</th>
            <th className="pr-4 pb-1 font-medium">Modality</th>
            <th className="pr-4 pb-1 font-medium">Family</th>
            <th className="pr-4 pb-1 font-medium">Streaming</th>
            <th className="pr-4 pb-1 font-medium">Price / MTok</th>
            <th className="pr-4 pb-1 font-medium">VRAM / Instance</th>
            <th className="pr-4 pb-1 font-medium">Co-serve group</th>
            <th className="pb-1 font-medium">Availability</th>
          </tr>
        </thead>
        <tbody>
          {entries.map((entry) => (
            <tr className="border-b last:border-0" key={entry.modelId}>
              <td className="py-1.5 pr-4">
                <div className="font-medium">{entry.modelName}</div>
                <div className="font-mono text-muted-foreground">
                  {entry.modelId}
                </div>
              </td>
              <td className="py-1.5 pr-4">
                <div className="flex flex-wrap gap-1">
                  {modalityBadges(entry)}
                </div>
              </td>
              <td className="py-1.5 pr-4 font-mono text-muted-foreground">
                {entry.family}
              </td>
              <td className="py-1.5 pr-4">
                {entry.responseStreamingSupported ? (
                  <Badge className="text-[0.625rem]" variant="outline">
                    Streaming
                  </Badge>
                ) : (
                  <span className="text-muted-foreground">-</span>
                )}
              </td>
              <td className="py-1.5 pr-4">
                <PriceCell entry={entry} />
              </td>
              <td className="py-1.5 pr-4">
                {isSelfHost(entry) ? (
                  <span className="font-mono">
                    {formatVRAMMiB(entry.minVramMib)} · {entry.instanceType}
                  </span>
                ) : (
                  <span className="text-muted-foreground">-</span>
                )}
              </td>
              <td className="py-1.5 pr-4 font-mono text-muted-foreground">
                {entry.coServeGroup || "-"}
              </td>
              <td className="py-1.5">
                <AvailabilityCell availability={entry.availability} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

export function ModelCatalogPage() {
  const { data } = useQuery(adminOchreCatalogQueryOptions)

  const entries = (data?.entries ?? []).toSorted((a, b) =>
    a.modelName.localeCompare(b.modelName),
  )

  return (
    <>
      <PageHeading title="Model Catalog" />
      <Card>
        <CardContent>
          {entries.length > 0 ? (
            <CatalogTable entries={entries} />
          ) : (
            <p className="text-xs text-muted-foreground">No models found.</p>
          )}
        </CardContent>
      </Card>
    </>
  )
}

import { useQuery } from "@tanstack/react-query"
import { useState } from "react"

import {
  type DataColumn,
  DataTable,
  type SortState,
} from "@/components/data-table"
import { PageHeading } from "@/components/page-heading"
import { Badge } from "@/components/ui/badge"
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
    <Badge key={modality} variant="outline">
      {modality}
    </Badge>
  ))
}

function formatMicroUsdPerMillion(microUsd: number): string {
  return `$${(microUsd / 1_000_000).toFixed(2)}`
}

function PriceCell({ entry }: { entry: AdminCatalogEntry }) {
  if (isSelfHost(entry)) {
    return <Badge variant="secondary">Included</Badge>
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
      <Badge variant={availabilityVariant(availability)}>
        {AVAILABILITY_LABEL[availability]}
      </Badge>
      {hint && <p className="mt-1 text-2xs text-muted-foreground">{hint}</p>}
    </div>
  )
}

const COLUMNS: DataColumn<AdminCatalogEntry>[] = [
  {
    id: "model",
    header: "Model",
    sortValue: (entry) => entry.modelName,
    cell: (entry) => (
      <div className="min-w-0">
        <div className="truncate font-medium">{entry.modelName}</div>
        <div className="truncate font-mono text-xs text-muted-foreground">
          {entry.modelId}
        </div>
      </div>
    ),
  },
  {
    id: "modality",
    header: "Modality",
    hideBelow: "md",
    cell: (entry) => (
      <div className="flex flex-wrap gap-1">{modalityBadges(entry)}</div>
    ),
  },
  {
    id: "family",
    header: "Family",
    sortValue: (entry) => entry.family,
    hideBelow: "lg",
    cell: (entry) => (
      <span className="font-mono text-xs text-muted-foreground">
        {entry.family}
      </span>
    ),
  },
  {
    id: "streaming",
    header: "Streaming",
    hideBelow: "lg",
    cell: (entry) =>
      entry.responseStreamingSupported ? (
        <Badge variant="outline">Streaming</Badge>
      ) : (
        <span className="text-muted-foreground">-</span>
      ),
  },
  {
    id: "price",
    header: "Price / MTok",
    hideBelow: "md",
    cell: (entry) => <PriceCell entry={entry} />,
  },
  {
    id: "vram",
    header: "VRAM / Instance",
    hideBelow: "lg",
    cell: (entry) =>
      isSelfHost(entry) ? (
        <span className="font-mono text-xs">
          {formatVRAMMiB(entry.minVramMib)} · {entry.instanceType}
        </span>
      ) : (
        <span className="text-muted-foreground">-</span>
      ),
  },
  {
    id: "coServeGroup",
    header: "Co-serve group",
    sortValue: (entry) => entry.coServeGroup,
    hideBelow: "lg",
    cell: (entry) => (
      <span className="font-mono text-xs text-muted-foreground">
        {entry.coServeGroup || "-"}
      </span>
    ),
  },
  {
    id: "availability",
    header: "Availability",
    sortValue: (entry) => entry.availability,
    cell: (entry) => <AvailabilityCell availability={entry.availability} />,
  },
]

export function ModelCatalogPage() {
  const { data } = useQuery(adminOchreCatalogQueryOptions)
  const [sort, setSort] = useState<SortState>({ column: "model", desc: false })

  return (
    <>
      <PageHeading title="Model Catalog" />
      <DataTable
        caption="Model catalog"
        columns={COLUMNS}
        empty="No models found."
        getRowId={(entry) => entry.modelId}
        onSortChange={setSort}
        rows={data?.entries ?? []}
        sort={sort}
      />
    </>
  )
}

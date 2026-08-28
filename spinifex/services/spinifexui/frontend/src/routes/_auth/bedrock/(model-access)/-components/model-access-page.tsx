import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"

import { PageHeading } from "@/components/page-heading"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import {
  adminModelAccessQueryOptions,
  adminOchreCatalogQueryOptions,
  grantModelAccess,
  revokeModelAccess,
  type AdminCatalogEntry,
} from "@/queries/admin"

function ModelAccessRow({
  entry,
  accountId,
  granted,
}: {
  entry: AdminCatalogEntry
  accountId: string
  granted: boolean
}) {
  const queryClient = useQueryClient()
  const modelAccessQueryKey = adminModelAccessQueryOptions(accountId).queryKey

  const grant = useMutation({
    mutationFn: grantModelAccess,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: modelAccessQueryKey })
    },
  })

  const revoke = useMutation({
    mutationFn: revokeModelAccess,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: modelAccessQueryKey })
    },
  })

  const pending = grant.isPending || revoke.isPending

  return (
    <tr className="border-b last:border-0" key={entry.modelId}>
      <td className="py-1.5 pr-4">
        <div className="font-medium">{entry.modelName}</div>
        <div className="font-mono text-muted-foreground">{entry.modelId}</div>
      </td>
      <td className="py-1.5 pr-4">
        <Badge
          className="text-[0.625rem]"
          variant={granted ? "default" : "secondary"}
        >
          {granted ? "Granted" : "Ungranted"}
        </Badge>
      </td>
      <td className="py-1.5">
        {granted ? (
          <Button
            disabled={pending}
            onClick={() => {
              revoke.mutate({ accountId, modelId: entry.modelId })
            }}
            size="sm"
            variant="destructive"
          >
            Revoke
          </Button>
        ) : (
          <Button
            disabled={pending}
            onClick={() => {
              grant.mutate({ accountId, modelId: entry.modelId })
            }}
            size="sm"
            variant="outline"
          >
            Grant
          </Button>
        )}
      </td>
    </tr>
  )
}

function ModelAccessTable({
  entries,
  accountId,
  grantedModelIds,
}: {
  entries: AdminCatalogEntry[]
  accountId: string
  grantedModelIds: Set<string>
}) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-xs">
        <thead>
          <tr className="border-b text-left text-muted-foreground">
            <th className="pr-4 pb-1 font-medium">Model</th>
            <th className="pr-4 pb-1 font-medium">Access</th>
            <th className="pb-1 font-medium">Action</th>
          </tr>
        </thead>
        <tbody>
          {entries.map((entry) => (
            <ModelAccessRow
              accountId={accountId}
              entry={entry}
              granted={grantedModelIds.has(entry.modelId)}
              key={entry.modelId}
            />
          ))}
        </tbody>
      </table>
    </div>
  )
}

function ModelAccessBody({
  accountId,
  entries,
  grantedModelIds,
}: {
  accountId: string
  entries: AdminCatalogEntry[]
  grantedModelIds: Set<string>
}) {
  if (accountId === "") {
    return (
      <p className="text-xs text-muted-foreground">
        Enter an account ID to view and manage its model access.
      </p>
    )
  }
  if (entries.length === 0) {
    return <p className="text-xs text-muted-foreground">No models found.</p>
  }
  return (
    <ModelAccessTable
      accountId={accountId}
      entries={entries}
      grantedModelIds={grantedModelIds}
    />
  )
}

export function ModelAccessPage() {
  const [accountId, setAccountId] = useState("")

  const { data: catalogData } = useQuery(adminOchreCatalogQueryOptions)
  const { data: accessData } = useQuery(adminModelAccessQueryOptions(accountId))

  const entries = (catalogData?.entries ?? []).toSorted((a, b) =>
    a.modelName.localeCompare(b.modelName),
  )
  const grantedModelIds = new Set(accessData?.ModelIds)

  return (
    <>
      <PageHeading title="Model Access" />
      <Card>
        <CardContent>
          <div className="mb-3 max-w-sm">
            <Input
              onChange={(event) => {
                setAccountId(event.target.value)
              }}
              placeholder="Account ID"
              value={accountId}
            />
          </div>
          <ModelAccessBody
            accountId={accountId}
            entries={entries}
            grantedModelIds={grantedModelIds}
          />
        </CardContent>
      </Card>
    </>
  )
}

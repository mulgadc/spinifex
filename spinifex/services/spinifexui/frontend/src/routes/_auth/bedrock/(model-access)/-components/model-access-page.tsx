import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"

import { PageHeading } from "@/components/page-heading"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Field, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import {
  adminListAccountsQueryOptions,
  adminModelAccessQueryOptions,
  adminOchreCatalogQueryOptions,
  grantModelAccess,
  revokeModelAccess,
  type AccountSummary,
  type AdminCatalogEntry,
} from "@/queries/admin"

// GlobalAccountID is granted every model unconditionally, so it is not a
// meaningful target for this page and is excluded from the picker.
const GLOBAL_ACCOUNT_ID = "000000000000"

const ACCOUNT_ID_RE = /^[0-9]{12}$/

function accountLabel(account: AccountSummary): string {
  return `${account.accountName} (${account.accountId}) — ${account.status}`
}

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

function ModelAccessAccountPicker({
  accounts,
  accountId,
  onAccountIdChange,
}: {
  accounts: AccountSummary[]
  accountId: string
  onAccountIdChange: (accountId: string) => void
}) {
  const sorted = accounts.toSorted((a, b) =>
    a.accountName.localeCompare(b.accountName),
  )
  return (
    <Field>
      <FieldTitle>
        <label htmlFor="model-access-account">Account</label>
      </FieldTitle>
      <Select
        onValueChange={(value) => {
          onAccountIdChange(value ?? "")
        }}
        value={accountId}
      >
        <SelectTrigger className="w-full" id="model-access-account">
          <SelectValue placeholder="Select an account" />
        </SelectTrigger>
        <SelectContent>
          {sorted.map((account) => (
            <SelectItem key={account.accountId} value={account.accountId}>
              {accountLabel(account)}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      <p className="mt-1 text-[0.625rem] text-muted-foreground">
        Select an account.
      </p>
    </Field>
  )
}

function ModelAccessAccountFallback({
  accountId,
  onAccountIdChange,
}: {
  accountId: string
  onAccountIdChange: (accountId: string) => void
}) {
  const invalid = accountId !== "" && !ACCOUNT_ID_RE.test(accountId)
  return (
    <Field>
      <FieldTitle>
        <label htmlFor="model-access-account">Account ID</label>
      </FieldTitle>
      <Input
        aria-invalid={invalid}
        id="model-access-account"
        onChange={(event) => {
          onAccountIdChange(event.target.value)
        }}
        placeholder="123456789012"
        value={accountId}
      />
      <p className="mt-1 text-[0.625rem] text-muted-foreground">
        {invalid
          ? "Enter a 12-digit numeric account ID."
          : "Enter a 12-digit account ID — account list unavailable."}
      </p>
    </Field>
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

  const accountsQuery = useQuery(adminListAccountsQueryOptions)
  const accounts = (accountsQuery.data?.accounts ?? []).filter(
    (account) => account.accountId !== GLOBAL_ACCOUNT_ID,
  )
  const pickerAvailable = accountsQuery.isSuccess && accounts.length > 0

  const validAccountId = pickerAvailable
    ? accountId !== ""
    : ACCOUNT_ID_RE.test(accountId)
  const effectiveAccountId = validAccountId ? accountId : ""

  const { data: catalogData } = useQuery(adminOchreCatalogQueryOptions)
  const { data: accessData } = useQuery(
    adminModelAccessQueryOptions(effectiveAccountId),
  )

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
            {pickerAvailable ? (
              <ModelAccessAccountPicker
                accountId={accountId}
                accounts={accounts}
                onAccountIdChange={setAccountId}
              />
            ) : (
              <ModelAccessAccountFallback
                accountId={accountId}
                onAccountIdChange={setAccountId}
              />
            )}
          </div>
          <ModelAccessBody
            accountId={effectiveAccountId}
            entries={entries}
            grantedModelIds={grantedModelIds}
          />
        </CardContent>
      </Card>
    </>
  )
}

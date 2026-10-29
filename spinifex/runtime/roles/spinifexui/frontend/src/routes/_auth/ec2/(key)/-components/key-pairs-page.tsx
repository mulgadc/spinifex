import type { KeyPairInfo } from "@aws-sdk/client-ec2"
import { Link } from "@tanstack/react-router"
import { Plus, Upload } from "lucide-react"
import { type ReactNode, useState } from "react"

import { BulkDelete } from "@/components/bulk-delete"
import {
  type DataColumn,
  DataTable,
  type SortState,
} from "@/components/data-table"
import { PageHeading } from "@/components/page-heading"
import { ResourceView } from "@/components/resource-view"
import { SelectionBar } from "@/components/selection-bar"
import { buttonVariants } from "@/components/ui/button"
import { useRowSelection } from "@/hooks/use-row-selection"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"
import { useDeleteKeyPair } from "@/mutations/ec2"

import { KeyPairDetails } from "./key-pair-details"

const COLUMNS: DataColumn<KeyPairInfo>[] = [
  {
    id: "name",
    header: "Key pair",
    sortValue: (keyPair) => keyPair.KeyName,
    cell: (keyPair) => {
      const id = keyPair.KeyPairId ?? ""

      return (
        <Link
          className="group block max-w-72 min-w-0 rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          from="/ec2/describe-key-pairs/"
          search={(previous) => ({
            ...previous,
            ...CLOSED_PANEL,
            selected: id,
          })}
          to="."
        >
          <span className="block truncate font-medium text-primary group-hover:underline">
            {keyPair.KeyName ?? id}
          </span>
          <span className="block truncate font-mono text-xs text-muted-foreground">
            {id}
          </span>
        </Link>
      )
    },
  },
  {
    id: "type",
    header: "Type",
    sortValue: (keyPair) => keyPair.KeyType,
    cell: (keyPair) => keyPair.KeyType,
  },
  {
    id: "fingerprint",
    header: "Fingerprint",
    sortValue: (keyPair) => keyPair.KeyFingerprint,
    hideBelow: "lg",
    cell: (keyPair) => (
      <span
        className="block max-w-80 truncate font-mono text-xs"
        title={keyPair.KeyFingerprint}
      >
        {keyPair.KeyFingerprint}
      </span>
    ),
  },
  {
    id: "created",
    header: "Created",
    sortValue: (keyPair) => keyPair.CreateTime?.getTime(),
    hideBelow: "md",
    cell: (keyPair) => (
      <span className="tabular-nums">{formatDateTime(keyPair.CreateTime)}</span>
    ),
  },
]

interface KeyPairsPageProps {
  keyPairs: readonly KeyPairInfo[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function KeyPairsPage({
  keyPairs,
  search,
  onSearchChange,
}: KeyPairsPageProps) {
  const [sort, setSort] = useState<SortState>({ column: "name", desc: false })

  const open = keyPairs.find(
    (keyPair) =>
      search.selected !== "" && keyPair.KeyPairId === search.selected,
  )

  const selection = useRowSelection(
    keyPairs.map((keyPair) => keyPair.KeyPairId ?? ""),
    open !== undefined,
    () => {
      onSearchChange(CLOSED_PANEL)
    },
  )

  const deleteKeyPair = useDeleteKeyPair()

  const names = new Map(
    keyPairs.map((keyPair) => [keyPair.KeyPairId, keyPair.KeyName]),
  )

  let panel: ReactNode = null

  if (open?.KeyPairId) {
    panel = (
      <KeyPairDetails
        key={open.KeyPairId}
        keyPair={open}
        onClose={() => {
          onSearchChange(CLOSED_PANEL)
        }}
        onTabChange={(tab) => {
          onSearchChange({ tab })
        }}
        tab={search.tab}
      />
    )
  }

  return (
    <ResourceView panel={panel}>
      <PageHeading
        actions={
          <div className="flex gap-2">
            <Link
              className={buttonVariants({ variant: "outline" })}
              to="/ec2/import-key-pair"
            >
              <Upload data-icon="inline-start" />
              Import key pair
            </Link>
            <Link className={buttonVariants()} to="/ec2/create-key-pair">
              <Plus data-icon="inline-start" />
              Create key pair
            </Link>
          </div>
        }
        subtitle="SSH keys that instances accept at launch."
        title="Key pairs"
      />
      {selection.ids.length > 0 && (
        <SelectionBar count={selection.ids.length} onClear={selection.clear}>
          <BulkDelete
            deleteOne={deleteKeyPair.mutateAsync}
            description="Instances already launched with them keep the public key, but they can no longer be chosen at launch. This cannot be undone."
            ids={selection.ids}
            label={(id) => `${names.get(id) ?? id} (${id})`}
            noun={["key pair", "key pairs"]}
            onDeleted={selection.drop}
          />
        </SelectionBar>
      )}
      <DataTable
        activeId={open?.KeyPairId}
        caption="Key pairs"
        columns={COLUMNS}
        empty="No key pairs yet."
        getRowId={(keyPair) => keyPair.KeyPairId ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSelectedChange={selection.onSelectedChange}
        onSortChange={setSort}
        rows={keyPairs}
        selected={selection.selected}
        sort={sort}
      />
    </ResourceView>
  )
}

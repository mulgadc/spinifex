import type { KeyPairInfo } from "@aws-sdk/client-ec2"
import { Link } from "@tanstack/react-router"
import { Plus, Upload } from "lucide-react"
import { type ReactNode, useState } from "react"

import { BulkDelete } from "@/components/bulk-delete"
import { CreatePanel } from "@/components/create-panel"
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

import { CreateKeyPairPage } from "./create-key-pair-page"
import { ImportKeyPairPage } from "./import-key-pair-page"
import { KeyPairDetails } from "./key-pair-details"

export interface KeyPairsSearch extends PanelSearch {
  import: boolean
}

export const CLOSED_KEY_PAIRS_PANEL = {
  ...CLOSED_PANEL,
  import: false,
} satisfies KeyPairsSearch

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
            ...CLOSED_KEY_PAIRS_PANEL,
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
  search: KeyPairsSearch
  onSearchChange: (search: Partial<KeyPairsSearch>) => void
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

  function closePanel() {
    onSearchChange(CLOSED_KEY_PAIRS_PANEL)
  }

  function showKeyPair(keyPairId?: string) {
    onSearchChange({ ...CLOSED_KEY_PAIRS_PANEL, selected: keyPairId ?? "" })
  }

  const selection = useRowSelection(
    keyPairs.map((keyPair) => keyPair.KeyPairId ?? ""),
    open !== undefined,
    closePanel,
  )

  const deleteKeyPair = useDeleteKeyPair()

  const names = new Map(
    keyPairs.map((keyPair) => [keyPair.KeyPairId, keyPair.KeyName]),
  )

  let panel: ReactNode = null

  if (search.create) {
    panel = (
      <CreatePanel onClose={closePanel} title="Create key pair">
        <CreateKeyPairPage onClose={showKeyPair} />
      </CreatePanel>
    )
  } else if (search.import) {
    panel = (
      <CreatePanel onClose={closePanel} title="Import key pair">
        <ImportKeyPairPage onClose={showKeyPair} />
      </CreatePanel>
    )
  } else if (open?.KeyPairId) {
    panel = (
      <KeyPairDetails
        key={open.KeyPairId}
        keyPair={open}
        onClose={closePanel}
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
              from="/ec2/describe-key-pairs/"
              search={(previous) => ({
                ...previous,
                ...CLOSED_KEY_PAIRS_PANEL,
                import: true,
              })}
              to="."
            >
              <Upload data-icon="inline-start" />
              Import key pair
            </Link>
            <Link
              className={buttonVariants()}
              from="/ec2/describe-key-pairs/"
              search={(previous) => ({
                ...previous,
                ...CLOSED_KEY_PAIRS_PANEL,
                create: true,
              })}
              to="."
            >
              <Plus data-icon="inline-start" />
              Create key pair
            </Link>
          </div>
        }
        subtitle="SSH keys that instances accept at launch."
        title="Key Pairs"
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
          showKeyPair(id)
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

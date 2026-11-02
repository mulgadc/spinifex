import type { ListObjectsV2CommandOutput } from "@aws-sdk/client-s3"
import { keepPreviousData, useQuery } from "@tanstack/react-query"
import { Link, useNavigate } from "@tanstack/react-router"
import {
  ChevronLeft,
  ChevronRight,
  Download,
  File,
  Folder,
  Search,
} from "lucide-react"
import { type ReactNode, useEffect, useState } from "react"

import { BulkDelete } from "@/components/bulk-delete"
import {
  type DataColumn,
  DataTable,
  type SortState,
} from "@/components/data-table"
import { PanelSpinner } from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
import { ResourceView } from "@/components/resource-view"
import { SelectionBar } from "@/components/selection-bar"
import { Button } from "@/components/ui/button"
import {
  InputGroup,
  InputGroupAddon,
  InputGroupInput,
} from "@/components/ui/input-group"
import { useRowSelection } from "@/hooks/use-row-selection"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { formatDateTime, formatSize } from "@/lib/utils"
import { downloadObject, useDeleteObjects } from "@/mutations/s3"
import { s3BucketObjectsQueryOptions } from "@/queries/s3"

import { ObjectDetails } from "./object-details"
import { extension, storageClassLabel } from "./object-labels"
import { UploadButton } from "./upload-button"

// Each keystroke would otherwise be its own ListObjectsV2.
const FIND_DELAY_MS = 250

interface BrowserRow {
  bucket: string
  // The full key, or for a folder the full prefix ending in "/".
  id: string
  name: string
  folder: boolean
  size?: number
  modified?: Date
  storageClass?: string
}

// Folders first, as S3 lists them separately, then the page's objects. The
// folder itself can appear as a zero-byte key and is left out.
function browserRows(
  page: ListObjectsV2CommandOutput | undefined,
  bucket: string,
  folder: string,
): BrowserRow[] {
  const folders = (page?.CommonPrefixes ?? []).flatMap(({ Prefix }) =>
    Prefix
      ? [
          {
            bucket,
            id: Prefix,
            name: Prefix.slice(folder.length),
            folder: true,
          },
        ]
      : [],
  )

  const objects = (page?.Contents ?? []).flatMap((object) =>
    object.Key && object.Key !== folder
      ? [
          {
            bucket,
            id: object.Key,
            name: object.Key.slice(folder.length),
            folder: false,
            size: object.Size,
            modified: object.LastModified,
            storageClass: object.StorageClass,
          },
        ]
      : [],
  )

  return [...folders, ...objects]
}

const COLUMNS: DataColumn<BrowserRow>[] = [
  {
    id: "name",
    header: "Name",
    sortValue: (row) => row.name,
    cell: (row) => <NameCell row={row} />,
  },
  {
    id: "type",
    header: "Type",
    sortValue: (row) => (row.folder ? "" : extension(row.name)),
    hideBelow: "md",
    cell: (row) => (row.folder ? "Folder" : extension(row.name) || "—"),
  },
  {
    id: "modified",
    header: "Last modified",
    sortValue: (row) => row.modified?.getTime(),
    hideBelow: "sm",
    cell: (row) => (row.modified ? formatDateTime(row.modified) : "—"),
  },
  {
    id: "size",
    header: "Size",
    align: "end",
    sortValue: (row) => row.size,
    cell: (row) => (row.size === undefined ? "—" : formatSize(row.size)),
  },
  {
    id: "class",
    header: "Storage class",
    hideBelow: "lg",
    cell: (row) =>
      row.folder ? "—" : storageClassLabel(row.storageClass ?? "STANDARD"),
  },
]

function useDebounced(value: string, delay: number) {
  const [settled, setSettled] = useState(value)

  useEffect(() => {
    const timer = setTimeout(() => {
      setSettled(value)
    }, delay)

    return () => {
      clearTimeout(timer)
    }
  }, [value, delay])

  return settled
}

// Continuation tokens only run forward, so the pages already visited are
// kept as a stack of the tokens that opened them.
function usePages(query: string) {
  const [tokens, setTokens] = useState<(string | undefined)[]>([undefined])
  const [forQuery, setForQuery] = useState(query)

  if (query !== forQuery) {
    setForQuery(query)
    setTokens([undefined])
  }

  return {
    index: tokens.length - 1,
    token: tokens.at(-1),
    next: (token: string) => {
      setTokens([...tokens, token])
    },
    previous: () => {
      setTokens(tokens.slice(0, -1))
    },
  }
}

interface ObjectBrowserProps {
  bucket: string
  // Empty at the bucket root, otherwise ends in "/".
  folder: string
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function ObjectBrowser({
  bucket,
  folder,
  search,
  onSearchChange,
}: ObjectBrowserProps) {
  const navigate = useNavigate()
  const [find, setFind] = useState("")
  const query = folder + useDebounced(find, FIND_DELAY_MS)
  const pages = usePages(query)
  const [sort, setSort] = useState<SortState>()
  const deleteObjects = useDeleteObjects(bucket)
  const [downloadError, setDownloadError] = useState<Error | null>(null)

  async function download(key: string) {
    setDownloadError(null)

    try {
      await downloadObject(bucket, key)
    } catch (error) {
      setDownloadError(new Error("Failed to download object", { cause: error }))
    }
  }

  const listing = useQuery({
    ...s3BucketObjectsQueryOptions(bucket, query, pages.token),
    placeholderData: keepPreviousData,
  })

  const rows = browserRows(listing.data, bucket, folder)
  const open = search.selected

  function closePanel() {
    onSearchChange(CLOSED_PANEL)
  }

  const selection = useRowSelection(
    rows.map((row) => row.id),
    open !== "",
    closePanel,
  )

  function openFolder(prefix: string) {
    void navigate({
      to: "/s3/ls/$bucket/$",
      params: { bucket, _splat: prefix },
    })
  }

  const objectIds = selection.ids.filter((id) => !id.endsWith("/"))
  const [single] = objectIds

  let table: ReactNode = <PanelSpinner />

  if (listing.error) {
    table = <ErrorBanner error={listing.error} msg="Failed to list objects" />
  } else if (listing.data) {
    let empty = `No objects in this ${folder ? "folder" : "bucket"} yet.`

    if (query !== folder) {
      empty = `No keys here start with “${query.slice(folder.length)}”.`
    }

    table = (
      <DataTable
        activeId={open || undefined}
        caption={`Objects in ${bucket}/${folder}`}
        columns={COLUMNS}
        empty={empty}
        getRowId={(row) => row.id}
        onRowClick={(id) => {
          if (id.endsWith("/")) {
            openFolder(id)
          } else {
            onSearchChange({ ...CLOSED_PANEL, selected: id })
          }
        }}
        onSelectedChange={selection.onSelectedChange}
        onSortChange={setSort}
        rows={rows}
        selected={selection.selected}
        sort={sort}
      />
    )
  }

  let panel: ReactNode = null

  if (open) {
    panel = (
      <ObjectDetails
        bucket={bucket}
        key={open}
        objectKey={open}
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
        actions={<UploadButton bucket={bucket} prefix={folder} />}
        title={bucket}
      />

      <PathCrumbs bucket={bucket} folder={folder} />

      <InputGroup className="mb-3 w-full sm:w-96">
        <InputGroupAddon>
          <Search />
        </InputGroupAddon>
        <InputGroupInput
          aria-label="Find objects by prefix"
          onChange={(event) => {
            setFind(event.target.value)
          }}
          placeholder={`Find by prefix in ${folder || `${bucket}/`}`}
          type="search"
          value={find}
        />
      </InputGroup>

      {selection.ids.length > 0 && (
        <SelectionBar
          count={selection.ids.length}
          footer={
            downloadError && (
              <ErrorBanner
                error={downloadError}
                msg="Failed to download object"
              />
            )
          }
          onClear={selection.clear}
        >
          {single !== undefined && objectIds.length === 1 && (
            <Button
              onClick={() => {
                void download(single)
              }}
              size="sm"
              variant="outline"
            >
              <Download data-icon="inline-start" />
              Download
            </Button>
          )}
          <BulkDelete
            confirmPhrase="delete"
            deleteMany={deleteObjects}
            description="Deleting a folder deletes every object under it. This cannot be undone."
            ids={selection.ids}
            label={(id) => id.slice(folder.length)}
            noun={["item", "items"]}
            onDeleted={(ids) => {
              selection.drop(ids)

              if (ids.includes(open)) {
                closePanel()
              }
            }}
          />
        </SelectionBar>
      )}

      {table}

      <nav
        aria-label="Pages"
        className="mt-3 flex items-center justify-end gap-2 text-sm text-muted-foreground"
      >
        <span aria-live="polite" className="mr-auto">
          {listing.isPlaceholderData
            ? `Loading page ${pages.index + 1}…`
            : listing.data &&
              `Page ${pages.index + 1} · ${rows.length} ${rows.length === 1 ? "item" : "items"}`}
        </span>
        {/* A listing that fits on one page has nowhere to go. */}
        {(pages.index > 0 || listing.data?.IsTruncated) && (
          <>
            <Button
              disabled={pages.index === 0}
              onClick={() => {
                selection.clear()
                pages.previous()
              }}
              size="sm"
              variant="outline"
            >
              <ChevronLeft data-icon="inline-start" />
              Previous
            </Button>
            <Button
              disabled={!listing.data?.IsTruncated || listing.isPlaceholderData}
              onClick={() => {
                const token = listing.data?.NextContinuationToken

                if (token) {
                  selection.clear()
                  pages.next(token)
                }
              }}
              size="sm"
              variant="outline"
            >
              Next
              <ChevronRight data-icon="inline-end" />
            </Button>
          </>
        )}
      </nav>
    </ResourceView>
  )
}

const NAME_LINK =
  "inline-flex max-w-xl min-w-0 items-center gap-2 rounded-sm font-medium text-primary hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"

// A folder links into itself; an object opens its panel beside the list.
function NameCell({ row }: { row: BrowserRow }) {
  const name = (
    <span className="truncate" title={row.id}>
      {row.name}
    </span>
  )

  if (row.folder) {
    return (
      <Link
        className={NAME_LINK}
        params={{ bucket: row.bucket, _splat: row.id }}
        to="/s3/ls/$bucket/$"
      >
        <Folder aria-hidden className="size-4 shrink-0" />
        {name}
      </Link>
    )
  }

  return (
    <Link
      className={NAME_LINK}
      search={(previous) => ({
        ...previous,
        ...CLOSED_PANEL,
        selected: row.id,
      })}
      to="."
    >
      <File aria-hidden className="size-4 shrink-0 text-muted-foreground" />
      {name}
    </Link>
  )
}

function PathCrumbs({ bucket, folder }: { bucket: string; folder: string }) {
  const parts = folder.split("/").filter(Boolean)

  return (
    <nav aria-label="Folder path" className="mb-4">
      <ol className="flex flex-wrap items-center font-mono text-sm text-muted-foreground">
        <li>
          <Link
            className="rounded-sm hover:text-foreground hover:underline"
            to="/s3/ls"
          >
            s3://
          </Link>
        </li>
        <li>
          {parts.length === 0 ? (
            <span aria-current="page" className="text-foreground">
              {bucket}
            </span>
          ) : (
            <Link
              className="rounded-sm hover:text-foreground hover:underline"
              params={{ bucket }}
              to="/s3/ls/$bucket"
            >
              {bucket}
            </Link>
          )}
        </li>
        {parts.map((part, index) => {
          const prefix = `${parts.slice(0, index + 1).join("/")}/`
          const last = index === parts.length - 1

          return (
            <li className="flex items-center" key={prefix}>
              <span aria-hidden className="px-1">
                /
              </span>
              {last ? (
                <span aria-current="page" className="text-foreground">
                  {part}
                </span>
              ) : (
                <Link
                  className="rounded-sm hover:text-foreground hover:underline"
                  params={{ bucket, _splat: prefix }}
                  to="/s3/ls/$bucket/$"
                >
                  {part}
                </Link>
              )}
            </li>
          )
        })}
        <li aria-hidden className="pl-1">
          /
        </li>
      </ol>
    </nav>
  )
}

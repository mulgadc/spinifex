import type { Repository } from "@aws-sdk/client-ecr"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Link, useNavigate } from "@tanstack/react-router"
import { Plus } from "lucide-react"
import { useState } from "react"

import { CreatePanel } from "@/components/create-panel"
import {
  type DataColumn,
  DataTable,
  type SortState,
} from "@/components/data-table"
import { PageHeading } from "@/components/page-heading"
import { ResourceDeleteDialog } from "@/components/resource-delete-dialog"
import { ResourceView } from "@/components/resource-view"
import { Button, buttonVariants } from "@/components/ui/button"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"
import { useDeleteRepository } from "@/mutations/ecr"
import { ecrRepositoriesQueryOptions } from "@/queries/ecr"

import { CreateRepositoryPage } from "./create-repository-page"

function repositoryColumns(
  onDelete: (name: string) => void,
): DataColumn<Repository>[] {
  return [
    {
      id: "name",
      header: "Name",
      sortValue: (repo) => repo.repositoryName,
      cell: (repo) => {
        const name = repo.repositoryName ?? ""

        return (
          <Link
            className="font-medium text-primary hover:underline"
            params={{ id: encodeURIComponent(name) }}
            to="/ecr/list-repositories/$id"
          >
            {name}
          </Link>
        )
      },
    },
    {
      id: "uri",
      header: "URI",
      sortValue: (repo) => repo.repositoryUri,
      hideBelow: "lg",
      cell: (repo) => (
        <span className="font-mono text-xs">{repo.repositoryUri ?? ""}</span>
      ),
    },
    {
      id: "mutability",
      header: "Tag Mutability",
      sortValue: (repo) => repo.imageTagMutability,
      hideBelow: "md",
      cell: (repo) => repo.imageTagMutability ?? "",
    },
    {
      id: "created",
      header: "Created",
      sortValue: (repo) => repo.createdAt?.getTime(),
      hideBelow: "xl",
      cell: (repo) => (
        <span className="tabular-nums">{formatDateTime(repo.createdAt)}</span>
      ),
    },
    {
      id: "actions",
      header: "Actions",
      align: "end",
      cell: (repo) => (
        <Button
          onClick={() => {
            onDelete(repo.repositoryName ?? "")
          }}
          size="sm"
          variant="destructive"
        >
          Delete
        </Button>
      ),
    },
  ]
}

interface RepositoriesPageProps {
  search: PanelSearch
  onSearchChange: (next: Partial<PanelSearch>) => void
}

export function RepositoriesPage({
  search,
  onSearchChange,
}: RepositoriesPageProps) {
  const navigate = useNavigate()
  const { data } = useSuspenseQuery(ecrRepositoriesQueryOptions)
  const deleteRepository = useDeleteRepository()
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null)

  const [sort, setSort] = useState<SortState>()
  const columns = repositoryColumns(setDeleteTarget)

  const repositories = (data.repositories ?? []).filter(
    (repo) => repo.repositoryName,
  )

  function closePanel() {
    onSearchChange(CLOSED_PANEL)
  }

  const panel = search.create ? (
    <CreatePanel onClose={closePanel} title="Create repository">
      <CreateRepositoryPage onClose={closePanel} />
    </CreatePanel>
  ) : null

  return (
    <ResourceView panel={panel}>
      <PageHeading
        actions={
          <Link
            className={buttonVariants()}
            from="/ecr/list-repositories/"
            search={(previous) => ({
              ...previous,
              ...CLOSED_PANEL,
              create: true,
            })}
            to="."
          >
            <Plus data-icon="inline-start" />
            Create repository
          </Link>
        }
        title="Repositories"
      />

      <DataTable
        caption="Repositories"
        columns={columns}
        empty="No repositories found."
        getRowId={(repo) => repo.repositoryName ?? ""}
        onRowClick={(name) => {
          void navigate({
            to: "/ecr/list-repositories/$id",
            params: { id: encodeURIComponent(name) },
          })
        }}
        onSortChange={setSort}
        rows={repositories}
        sort={sort}
      />

      <ResourceDeleteDialog
        description={
          <>
            This permanently deletes {deleteTarget} and every image it holds.
            This cannot be undone.
          </>
        }
        identifier={deleteTarget ?? ""}
        mutation={deleteRepository}
        onOpenChange={(open) => {
          if (!open) {
            setDeleteTarget(null)
          }
        }}
        open={deleteTarget !== null}
        title="Delete repository?"
      />
    </ResourceView>
  )
}

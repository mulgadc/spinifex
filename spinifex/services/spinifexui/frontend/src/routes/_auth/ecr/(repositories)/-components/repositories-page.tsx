import type { Repository } from "@aws-sdk/client-ecr"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Link, useNavigate } from "@tanstack/react-router"
import { useState } from "react"

import { PageHeading } from "@/components/page-heading"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import { formatDateTime } from "@/lib/utils"
import { useDeleteRepository } from "@/mutations/ecr"
import { ecrRepositoriesQueryOptions } from "@/queries/ecr"

import { CreateRepositoryDialog } from "./create-repository-dialog"

export function RepositoriesPage() {
  const navigate = useNavigate()
  const { data } = useSuspenseQuery(ecrRepositoriesQueryOptions)
  const deleteRepository = useDeleteRepository()
  const [createOpen, setCreateOpen] = useState(false)
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null)

  const repositories = data.repositories ?? []

  async function handleDelete() {
    if (!deleteTarget) {
      return
    }
    try {
      await deleteRepository.mutateAsync(deleteTarget)
      setDeleteTarget(null)
    } catch {
      // error shown via deleteRepository.error
    }
  }

  return (
    <>
      <PageHeading
        actions={
          <Button onClick={() => setCreateOpen(true)}>Create Repository</Button>
        }
        title="Repositories"
      />

      {repositories.length > 0 ? (
        <div className="overflow-x-auto rounded-lg border bg-card">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b text-left text-muted-foreground">
                <th className="px-4 py-2 font-medium">Name</th>
                <th className="px-4 py-2 font-medium">URI</th>
                <th className="px-4 py-2 font-medium">Tag Mutability</th>
                <th className="px-4 py-2 font-medium">Created</th>
                <th className="px-4 py-2 font-medium">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {repositories.map((repo: Repository) => {
                if (!repo.repositoryName) {
                  return null
                }
                const name = repo.repositoryName
                return (
                  <tr
                    className="cursor-pointer border-b transition-colors last:border-0 hover:bg-accent"
                    key={name}
                    onClick={async () =>
                      await navigate({
                        to: "/ecr/list-repositories/$id",
                        params: { id: encodeURIComponent(name) },
                      })
                    }
                  >
                    <td className="px-4 py-2 font-medium">
                      <Link
                        className="text-primary hover:underline"
                        onClick={(e) => e.stopPropagation()}
                        params={{ id: encodeURIComponent(name) }}
                        to="/ecr/list-repositories/$id"
                      >
                        {name}
                      </Link>
                    </td>
                    <td className="px-4 py-2 font-mono text-xs">
                      {repo.repositoryUri ?? ""}
                    </td>
                    <td className="px-4 py-2">
                      {repo.imageTagMutability ?? ""}
                    </td>
                    <td className="px-4 py-2 text-xs">
                      {formatDateTime(repo.createdAt)}
                    </td>
                    <td className="px-4 py-2 text-right">
                      <Button
                        onClick={(e) => {
                          e.stopPropagation()
                          setDeleteTarget(name)
                        }}
                        size="sm"
                        variant="destructive"
                      >
                        Delete
                      </Button>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      ) : (
        <p className="text-muted-foreground">No repositories found.</p>
      )}

      <CreateRepositoryDialog onOpenChange={setCreateOpen} open={createOpen} />

      <AlertDialog
        onOpenChange={(open) => !open && setDeleteTarget(null)}
        open={deleteTarget !== null}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete repository?</AlertDialogTitle>
            <AlertDialogDescription>
              This permanently deletes {deleteTarget} and every image it holds.
              This cannot be undone.
            </AlertDialogDescription>
          </AlertDialogHeader>
          {deleteRepository.error && (
            <p className="text-sm text-destructive">
              {deleteRepository.error.message}
            </p>
          )}
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              disabled={deleteRepository.isPending}
              onClick={(e) => {
                e.preventDefault()
                void handleDelete()
              }}
            >
              {deleteRepository.isPending ? "Deleting…" : "Delete"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}

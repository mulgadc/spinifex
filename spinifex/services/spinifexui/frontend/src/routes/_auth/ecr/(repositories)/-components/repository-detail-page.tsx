import type { ImageDetail } from "@aws-sdk/client-ecr"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
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
import { Tabs, TabsList, TabsPanel, TabsTab } from "@/components/ui/tabs"
import { formatDateTime, formatSize } from "@/lib/utils"
import { useBatchDeleteImage } from "@/mutations/ecr"
import {
  ecrRepositoriesQueryOptions,
  ecrRepositoryImagesQueryOptions,
} from "@/queries/ecr"

import { LifecyclePolicyEditor } from "./lifecycle-policy-editor"
import { RepositoryPolicyEditor } from "./repository-policy-editor"
import { RepositorySummary } from "./repository-summary"

interface RepositoryDetailPageProps {
  repositoryName: string
}

export function RepositoryDetailPage({
  repositoryName,
}: RepositoryDetailPageProps) {
  const { data: reposData } = useSuspenseQuery(ecrRepositoriesQueryOptions)
  const { data: imagesData } = useSuspenseQuery(
    ecrRepositoryImagesQueryOptions(repositoryName),
  )
  const deleteImage = useBatchDeleteImage()
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null)

  const repository = (reposData.repositories ?? []).find(
    (r) => r.repositoryName === repositoryName,
  )
  const images = imagesData.imageDetails ?? []

  async function handleDeleteImage() {
    if (!deleteTarget) {
      return
    }
    try {
      await deleteImage.mutateAsync({
        repositoryName,
        imageIds: [{ imageDigest: deleteTarget }],
      })
      setDeleteTarget(null)
    } catch {
      // error shown via deleteImage.error
    }
  }

  return (
    <>
      <PageHeading
        subtitle={repository?.repositoryUri}
        title={repositoryName}
      />

      <div className="mb-4">
        <Link
          className="text-sm text-primary hover:underline"
          to="/ecr/list-repositories"
        >
          ← Back to repositories
        </Link>
      </div>

      <Tabs defaultValue="summary">
        <TabsList>
          <TabsTab value="summary">Summary</TabsTab>
          <TabsTab value="images">Images</TabsTab>
          <TabsTab value="lifecycle">Lifecycle</TabsTab>
          <TabsTab value="permissions">Permissions</TabsTab>
        </TabsList>

        <TabsPanel value="summary">
          <RepositorySummary
            imageTagMutability={repository?.imageTagMutability}
            repositoryName={repositoryName}
            repositoryUri={repository?.repositoryUri}
          />
        </TabsPanel>

        <TabsPanel value="images">
          {images.length > 0 ? (
            <div className="overflow-x-auto rounded-lg border bg-card">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b text-left text-muted-foreground">
                    <th className="px-4 py-2 font-medium">Tags</th>
                    <th className="px-4 py-2 font-medium">Digest</th>
                    <th className="px-4 py-2 font-medium">Size</th>
                    <th className="px-4 py-2 font-medium">Pushed</th>
                    <th className="px-4 py-2 font-medium">
                      <span className="sr-only">Actions</span>
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {images.map((image: ImageDetail) => {
                    if (!image.imageDigest) {
                      return null
                    }
                    const digest = image.imageDigest
                    return (
                      <tr className="border-b last:border-0" key={digest}>
                        <td className="px-4 py-2">
                          {image.imageTags && image.imageTags.length > 0
                            ? image.imageTags.join(", ")
                            : "<untagged>"}
                        </td>
                        <td className="px-4 py-2 font-mono text-xs">
                          {digest.slice(0, 19)}…
                        </td>
                        <td className="px-4 py-2">
                          {formatSize(image.imageSizeInBytes ?? 0)}
                        </td>
                        <td className="px-4 py-2 text-xs">
                          {formatDateTime(image.imagePushedAt)}
                        </td>
                        <td className="px-4 py-2 text-right">
                          <Button
                            onClick={() => setDeleteTarget(digest)}
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
            <p className="text-muted-foreground">No images pushed yet.</p>
          )}
        </TabsPanel>

        <TabsPanel value="lifecycle">
          <LifecyclePolicyEditor repositoryName={repositoryName} />
        </TabsPanel>

        <TabsPanel value="permissions">
          <RepositoryPolicyEditor repositoryName={repositoryName} />
        </TabsPanel>
      </Tabs>

      <AlertDialog
        onOpenChange={(open) => !open && setDeleteTarget(null)}
        open={deleteTarget !== null}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete image?</AlertDialogTitle>
            <AlertDialogDescription>
              This permanently deletes the image manifest and any tags pointing
              at it. This cannot be undone.
            </AlertDialogDescription>
          </AlertDialogHeader>
          {deleteImage.error && (
            <p className="text-sm text-destructive">
              {deleteImage.error.message}
            </p>
          )}
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              disabled={deleteImage.isPending}
              onClick={(e) => {
                e.preventDefault()
                void handleDeleteImage()
              }}
            >
              {deleteImage.isPending ? "Deleting…" : "Delete"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}

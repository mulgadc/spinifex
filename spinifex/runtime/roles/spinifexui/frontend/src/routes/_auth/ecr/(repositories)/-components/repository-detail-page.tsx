import type { ImageDetail } from "@aws-sdk/client-ecr"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import { useState } from "react"

import {
  type DataColumn,
  DataTable,
  type SortState,
} from "@/components/data-table"
import { PageHeading } from "@/components/page-heading"
import { ResourceDeleteDialog } from "@/components/resource-delete-dialog"
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
import { ScanFindings } from "./scan-findings"

function imageTagsLabel(tags: string[] | undefined): string {
  return tags && tags.length > 0 ? tags.join(", ") : "<untagged>"
}

function imageColumns(
  onDelete: (digest: string) => void,
): DataColumn<ImageDetail>[] {
  return [
    {
      id: "tags",
      header: "Tags",
      sortValue: (image) => image.imageTags?.join(", "),
      cell: (image) => imageTagsLabel(image.imageTags),
    },
    {
      id: "digest",
      header: "Digest",
      sortValue: (image) => image.imageDigest,
      hideBelow: "sm",
      cell: (image) => (
        <span className="font-mono text-xs" title={image.imageDigest}>
          {image.imageDigest?.slice(0, 19)}…
        </span>
      ),
    },
    {
      id: "size",
      header: "Size",
      sortValue: (image) => image.imageSizeInBytes,
      align: "end",
      hideBelow: "md",
      cell: (image) => (
        <span className="tabular-nums">
          {formatSize(image.imageSizeInBytes ?? 0)}
        </span>
      ),
    },
    {
      id: "pushed",
      header: "Pushed",
      sortValue: (image) => image.imagePushedAt?.getTime(),
      hideBelow: "lg",
      cell: (image) => (
        <span className="tabular-nums">
          {formatDateTime(image.imagePushedAt)}
        </span>
      ),
    },
    {
      id: "actions",
      header: "Actions",
      align: "end",
      cell: (image) => (
        <Button
          onClick={() => {
            onDelete(image.imageDigest ?? "")
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
  const [sort, setSort] = useState<SortState>()
  const columns = imageColumns(setDeleteTarget)

  const repository = (reposData.repositories ?? []).find(
    (r) => r.repositoryName === repositoryName,
  )

  const images = (imagesData.imageDetails ?? []).filter(
    (image) => image.imageDigest,
  )

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
          <TabsTab value="scan">Scan</TabsTab>
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
          <DataTable
            caption="Images"
            columns={columns}
            empty="No images pushed yet."
            getRowId={(image) => image.imageDigest ?? ""}
            onSortChange={setSort}
            rows={images}
            sort={sort}
          />
        </TabsPanel>

        <TabsPanel value="lifecycle">
          <LifecyclePolicyEditor repositoryName={repositoryName} />
        </TabsPanel>

        <TabsPanel value="scan">
          <ScanFindings />
        </TabsPanel>

        <TabsPanel value="permissions">
          <RepositoryPolicyEditor repositoryName={repositoryName} />
        </TabsPanel>
      </Tabs>

      <ResourceDeleteDialog
        description="This permanently deletes the image manifest and any tags pointing at it. This cannot be undone."
        identifier={{
          repositoryName,
          imageIds: [{ imageDigest: deleteTarget ?? "" }],
        }}
        mutation={deleteImage}
        onOpenChange={(open) => {
          if (!open) {
            setDeleteTarget(null)
          }
        }}
        open={deleteTarget !== null}
        title="Delete image?"
      />
    </>
  )
}

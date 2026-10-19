import { useSuspenseQuery } from "@tanstack/react-query"

import {
  buildFullS3Key,
  extractDisplayName,
  removeTrailingSlash,
} from "@/lib/utils"
import { s3BucketObjectsQueryOptions } from "@/queries/s3"

import { FolderListItem } from "./folder-list-item"
import { ObjectListItem } from "./object-list-item"

interface BucketObjectListProps {
  bucket: string
  // Empty at the bucket root, otherwise ends in "/".
  prefix: string
}

export function BucketObjectList({ bucket, prefix }: BucketObjectListProps) {
  const { data } = useSuspenseQuery(s3BucketObjectsQueryOptions(bucket, prefix))

  const objects = data.Contents ?? []
  const commonPrefixes = data.CommonPrefixes ?? []

  if (objects.length === 0 && commonPrefixes.length === 0) {
    return (
      <p className="text-muted-foreground">
        No objects found in this {prefix === "" ? "bucket" : "folder"}.
      </p>
    )
  }

  return (
    <>
      {commonPrefixes.length > 0 && (
        <div className="mb-6">
          <h2 className="mb-3 text-lg font-semibold">Folders</h2>
          <div className="space-y-2">
            {commonPrefixes.map((commonPrefix) => {
              if (!commonPrefix.Prefix) {
                return null
              }

              return (
                <FolderListItem
                  bucketName={bucket}
                  folderName={removeTrailingSlash(
                    extractDisplayName(commonPrefix.Prefix, prefix),
                  )}
                  fullPrefix={buildFullS3Key(commonPrefix.Prefix, prefix)}
                  key={commonPrefix.Prefix}
                />
              )
            })}
          </div>
        </div>
      )}
      {objects.length > 0 && (
        <div className="space-y-2">
          <h2 className="mb-3 text-lg font-semibold">Objects</h2>
          {objects.map((object) => {
            if (!object.Key) {
              return null
            }

            return (
              <ObjectListItem
                bucketName={bucket}
                displayName={extractDisplayName(object.Key, prefix)}
                fullKey={buildFullS3Key(object.Key, prefix)}
                key={object.Key}
                object={object}
              />
            )
          })}
        </div>
      )}
    </>
  )
}

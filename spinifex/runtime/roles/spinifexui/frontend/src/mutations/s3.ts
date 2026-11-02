import {
  CreateBucketCommand,
  DeleteBucketCommand,
  DeleteObjectCommand,
  DeleteObjectsCommand,
  GetObjectCommand,
  ListObjectsV2Command,
} from "@aws-sdk/client-s3"
import { useMutation, useQueryClient } from "@tanstack/react-query"

import { getS3Client } from "@/lib/awsClient"
import type { CreateBucketFormData } from "@/types/s3"

export function useCreateBucket() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: async ({ bucketName }: CreateBucketFormData) => {
      const command = new CreateBucketCommand({
        Bucket: bucketName,
      })

      return await getS3Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: ["s3", "buckets"],
      })
    },
  })
}

export function useDeleteObject() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: async ({ bucket, key }: { bucket: string; key: string }) => {
      const command = new DeleteObjectCommand({
        Bucket: bucket,
        Key: key,
      })

      return await getS3Client().send(command)
    },
    onSuccess: (_, variables) => {
      void queryClient.invalidateQueries({
        queryKey: ["s3", "buckets", variables.bucket],
      })
    },
  })
}

export function useDeleteBucket() {
  const queryClient = useQueryClient()

  return useMutation({
    // S3 refuses a bucket that still holds objects, with BucketNotEmpty.
    mutationFn: async (bucket: string) =>
      await getS3Client().send(new DeleteBucketCommand({ Bucket: bucket })),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["s3", "buckets"] })
    },
  })
}

// DeleteObjects takes at most 1000 keys a call.
const DELETE_BATCH = 1000

// Deletes keys in batches and returns the ones S3 refused, with its message.
async function deleteKeys(
  bucket: string,
  keys: readonly string[],
): Promise<Map<string, string>> {
  const refused = new Map<string, string>()

  for (let start = 0; start < keys.length; start += DELETE_BATCH) {
    const batch = keys.slice(start, start + DELETE_BATCH)

    // oxlint-disable-next-line no-await-in-loop -- one batch at a time keeps the load on the store even
    const result = await getS3Client().send(
      new DeleteObjectsCommand({
        Bucket: bucket,
        Delete: { Objects: batch.map((Key) => ({ Key })), Quiet: true },
      }),
    )

    for (const error of result.Errors ?? []) {
      if (error.Key) {
        refused.set(error.Key, error.Message ?? error.Code ?? "Refused")
      }
    }
  }

  return refused
}

// Every key under a prefix, across pages, as a folder delete needs them all.
async function keysUnder(bucket: string, prefix: string): Promise<string[]> {
  const keys: string[] = []
  let token: string | undefined

  do {
    // oxlint-disable-next-line no-await-in-loop -- each page needs the last one's token
    const page = await getS3Client().send(
      new ListObjectsV2Command({
        Bucket: bucket,
        Prefix: prefix,
        ContinuationToken: token,
      }),
    )

    for (const object of page.Contents ?? []) {
      if (object.Key) {
        keys.push(object.Key)
      }
    }

    token = page.IsTruncated ? page.NextContinuationToken : undefined
  } while (token)

  return keys
}

// Deletes the browser's selection: an ID ending in "/" is a folder, which
// goes with everything under it. Returns the IDs that failed, with why.
export function useDeleteObjects(bucket: string) {
  const queryClient = useQueryClient()

  return async (ids: readonly string[]) => {
    const failed = new Map<string, string>()
    const objects = ids.filter((id) => !id.endsWith("/"))

    try {
      for (const [key, message] of await deleteKeys(bucket, objects)) {
        failed.set(key, message)
      }

      for (const folder of ids.filter((id) => id.endsWith("/"))) {
        // oxlint-disable-next-line no-await-in-loop -- folders go one at a time, so each failure lands on its own row
        const keys = await keysUnder(bucket, folder)
        // oxlint-disable-next-line no-await-in-loop
        const refused = await deleteKeys(bucket, keys)
        const [first] = refused.values()

        if (first !== undefined) {
          failed.set(
            folder,
            `${refused.size} of ${keys.length} objects were not deleted: ${first}`,
          )
        }
      }
    } finally {
      await queryClient.invalidateQueries({
        queryKey: ["s3", "buckets", bucket],
      })
    }

    return failed
  }
}

// Saves an object through the browser. The body is read whole, which suits
// the console's use; a very large object is better fetched with the CLI.
export async function downloadObject(bucket: string, key: string) {
  const response = await getS3Client().send(
    new GetObjectCommand({ Bucket: bucket, Key: key }),
  )

  if (!response.Body) {
    return
  }

  // tsgolint cannot resolve the SDK's conditional streaming-blob type and
  // reports Body as an error type; tsc types it correctly.
  // oxlint-disable-next-line typescript/no-unsafe-call, typescript/no-unsafe-member-access, typescript/no-unsafe-assignment
  const bytes = await response.Body.transformToByteArray()

  const url = URL.createObjectURL(
    new Blob([bytes], { type: response.ContentType }),
  )

  const link = document.createElement("a")

  link.href = url
  link.download = key.split("/").at(-1) ?? key
  document.body.append(link)
  link.click()
  link.remove()
  URL.revokeObjectURL(url)
}

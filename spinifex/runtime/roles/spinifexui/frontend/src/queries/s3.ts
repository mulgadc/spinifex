import {
  HeadObjectCommand,
  ListBucketsCommand,
  ListObjectsV2Command,
} from "@aws-sdk/client-s3"
import { queryOptions } from "@tanstack/react-query"

import { getS3Client } from "@/lib/awsClient"

// Keys and folders per page of the object browser. S3 counts both toward
// MaxKeys, and caps a page at 1000.
export const OBJECT_PAGE_SIZE = 100

export const s3BucketsQueryOptions = queryOptions({
  queryKey: ["s3", "buckets"],
  queryFn: async () => {
    const command = new ListBucketsCommand({})

    return await getS3Client().send(command)
  },
})

// One page of a folder's listing. `prefix` is the folder plus anything typed
// into the find box, and `token` is the previous page's continuation token.
export const s3BucketObjectsQueryOptions = (
  bucketName: string,
  prefix?: string,
  token?: string,
) =>
  queryOptions({
    queryKey: [
      "s3",
      "buckets",
      bucketName,
      "objects",
      prefix ?? "",
      token ?? "",
    ],
    queryFn: async () => {
      const command = new ListObjectsV2Command({
        Bucket: bucketName,
        // The bucket root lists with no Prefix rather than an empty one.
        Prefix: prefix || undefined,
        Delimiter: "/",
        MaxKeys: OBJECT_PAGE_SIZE,
        ContinuationToken: token,
      })

      return await getS3Client().send(command)
    },
  })

export const s3ObjectHeadQueryOptions = (bucketName: string, key: string) =>
  queryOptions({
    queryKey: ["s3", "buckets", bucketName, "head", key],
    queryFn: async () => {
      const command = new HeadObjectCommand({ Bucket: bucketName, Key: key })

      return await getS3Client().send(command)
    },
  })

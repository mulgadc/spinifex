import { ListBucketsCommand, ListObjectsV2Command } from "@aws-sdk/client-s3"
import { queryOptions } from "@tanstack/react-query"

import { getS3Client } from "@/lib/awsClient"

export const s3BucketsQueryOptions = queryOptions({
  queryKey: ["s3", "buckets"],
  queryFn: async () => {
    const command = new ListBucketsCommand({})
    return await getS3Client().send(command)
  },
})

export const s3BucketObjectsQueryOptions = (
  bucketName: string,
  prefix?: string,
) =>
  queryOptions({
    queryKey: ["s3", "buckets", bucketName, "objects", prefix ?? ""],
    queryFn: async () => {
      const command = new ListObjectsV2Command({
        Bucket: bucketName,
        Prefix: prefix,
        Delimiter: "/",
      })
      return await getS3Client().send(command)
    },
  })

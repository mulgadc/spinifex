import { CreateBucketCommand, DeleteObjectCommand } from "@aws-sdk/client-s3"
import { Upload } from "@aws-sdk/lib-storage"
import { useMutation, useQueryClient } from "@tanstack/react-query"

import { getS3Client } from "@/lib/awsClient"
import type { CreateBucketFormData } from "@/types/s3"

interface UploadObjectParams {
  bucket: string
  key: string
  file: File
}

export function useUploadObject() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: async ({ bucket, key, file }: UploadObjectParams) => {
      const upload = new Upload({
        client: getS3Client(),
        params: {
          Bucket: bucket,
          Key: key,
          Body: file,
          ContentType: file.type,
        },
      })

      return await upload.done()
    },
    onSuccess: (_, variables) => {
      queryClient.invalidateQueries({
        queryKey: ["s3", "buckets", variables.bucket, "objects"],
      })
    },
  })
}

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
      queryClient.invalidateQueries({
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
      queryClient.invalidateQueries({
        queryKey: ["s3", "buckets", variables.bucket, "objects"],
      })
    },
  })
}

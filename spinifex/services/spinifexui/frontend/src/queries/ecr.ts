import {
  DescribeImagesCommand,
  DescribeRepositoriesCommand,
  GetRepositoryPolicyCommand,
  RepositoryPolicyNotFoundException,
} from "@aws-sdk/client-ecr"
import { queryOptions } from "@tanstack/react-query"

import { getEcrClient } from "@/lib/awsClient"

export const ecrRepositoriesQueryOptions = queryOptions({
  queryKey: ["ecr", "repositories"],
  queryFn: async () => {
    const command = new DescribeRepositoriesCommand({})
    return await getEcrClient().send(command)
  },
})

export const ecrRepositoryImagesQueryOptions = (repositoryName: string) =>
  queryOptions({
    queryKey: ["ecr", "repositories", repositoryName, "images"],
    queryFn: async () => {
      const command = new DescribeImagesCommand({ repositoryName })
      return await getEcrClient().send(command)
    },
  })

// ecrRepositoryPolicyQueryOptions resolves a repo's policy document. A repo with
// no policy attached returns RepositoryPolicyNotFoundException, which is a normal
// empty state here, so it is mapped to a null document rather than thrown.
export const ecrRepositoryPolicyQueryOptions = (repositoryName: string) =>
  queryOptions({
    queryKey: ["ecr", "repositories", repositoryName, "policy"],
    queryFn: async () => {
      try {
        const command = new GetRepositoryPolicyCommand({ repositoryName })
        const result = await getEcrClient().send(command)
        return result.policyText ?? null
      } catch (error) {
        if (error instanceof RepositoryPolicyNotFoundException) {
          return null
        }
        throw error
      }
    },
  })

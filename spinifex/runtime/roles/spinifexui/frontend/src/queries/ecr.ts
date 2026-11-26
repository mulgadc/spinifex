import {
  DescribeImagesCommand,
  DescribeRepositoriesCommand,
  GetLifecyclePolicyCommand,
  GetRepositoryPolicyCommand,
  type ImageDetail,
  LifecyclePolicyNotFoundException,
  type Repository,
  RepositoryPolicyNotFoundException,
} from "@aws-sdk/client-ecr"
import { queryOptions } from "@tanstack/react-query"

import { getEcrClient } from "@/lib/aws-client"

export const ecrRepositoriesQueryOptions = queryOptions({
  queryKey: ["ecr", "repositories"],
  queryFn: async () => {
    const repositories: Repository[] = []
    let nextToken: string | undefined

    do {
      // Each page needs the token the one before it returned.
      // oxlint-disable-next-line no-await-in-loop
      const page = await getEcrClient().send(
        new DescribeRepositoriesCommand({ nextToken }),
      )

      repositories.push(...(page.repositories ?? []))
      ;({ nextToken } = page)
    } while (nextToken)

    return { repositories }
  },
})

export const ecrRepositoryImagesQueryOptions = (repositoryName: string) =>
  queryOptions({
    queryKey: ["ecr", "repositories", repositoryName, "images"],
    queryFn: async () => {
      const imageDetails: ImageDetail[] = []
      let nextToken: string | undefined

      do {
        // oxlint-disable-next-line no-await-in-loop
        const page = await getEcrClient().send(
          new DescribeImagesCommand({ repositoryName, nextToken }),
        )

        imageDetails.push(...(page.imageDetails ?? []))
        ;({ nextToken } = page)
      } while (nextToken)

      return { imageDetails }
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

// ecrLifecyclePolicyQueryOptions resolves a repo's lifecycle-policy document. A
// repo with no policy returns LifecyclePolicyNotFoundException, which is a normal
// empty state here, so it is mapped to a null document rather than thrown.
export const ecrLifecyclePolicyQueryOptions = (repositoryName: string) =>
  queryOptions({
    queryKey: ["ecr", "repositories", repositoryName, "lifecycle"],
    queryFn: async () => {
      try {
        const command = new GetLifecyclePolicyCommand({ repositoryName })
        const result = await getEcrClient().send(command)

        return result.lifecyclePolicyText ?? null
      } catch (error) {
        if (error instanceof LifecyclePolicyNotFoundException) {
          return null
        }

        throw error
      }
    },
  })

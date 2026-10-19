import type { ListObjectsV2CommandOutput } from "@aws-sdk/client-s3"
import { screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import { s3BucketObjectsQueryOptions } from "@/queries/s3"
import { createTestQueryClient, renderWithClient } from "@/test/render"

import { BucketObjectList } from "./bucket-object-list"

function renderList(prefix: string, listing: ListObjectsV2CommandOutput) {
  const client = createTestQueryClient()
  client.setQueryData(
    s3BucketObjectsQueryOptions("bucket", prefix).queryKey,
    listing,
  )

  return renderWithClient(
    <BucketObjectList bucket="bucket" prefix={prefix} />,
    client,
  )
}

describe("BucketObjectList", () => {
  it("names folders and objects relative to the prefix", () => {
    renderList("docs/", {
      CommonPrefixes: [{ Prefix: "docs/img/" }],
      Contents: [{ Key: "docs/readme.md" }],
      $metadata: {},
    })

    expect(screen.getByText("img")).toBeInTheDocument()
    expect(screen.getByText("readme.md")).toBeInTheDocument()
  })

  it("says the bucket is empty at the root", () => {
    renderList("", { $metadata: {} })
    expect(
      screen.getByText("No objects found in this bucket."),
    ).toBeInTheDocument()
  })

  it("says the folder is empty under a prefix", () => {
    renderList("docs/", { $metadata: {} })
    expect(
      screen.getByText("No objects found in this folder."),
    ).toBeInTheDocument()
  })
})

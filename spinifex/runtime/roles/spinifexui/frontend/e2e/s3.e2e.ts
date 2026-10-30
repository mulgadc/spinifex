import { type ConsoleApi, expect, test } from "./console"

const S3_NAMESPACE = "http://s3.amazonaws.com/doc/2006-03-01/"

// Two folders and 1,200 objects at the root: twelve pages at the browser's
// page size, which is what pagination has to prove itself against.
const ENTRIES = [
  "2025/",
  "2026/",
  ...Array.from(
    { length: 1200 },
    (_, index) => `img-${String(index).padStart(4, "0")}.jpg`,
  ),
]

// ListObjectsV2 as S3 answers it: folders and keys share MaxKeys, and the
// continuation token is opaque, here the index the next page starts at.
function listObjects(params: URLSearchParams) {
  const prefix = params.get("prefix") ?? ""
  const maxKeys = Number(params.get("max-keys") ?? "1000")
  const start = Number(params.get("continuation-token") ?? "0")
  const matching = ENTRIES.filter((entry) => entry.startsWith(prefix))
  const page = matching.slice(start, start + maxKeys)
  const truncated = start + maxKeys < matching.length

  const folders = page.flatMap((entry) =>
    entry.endsWith("/")
      ? [`<CommonPrefixes><Prefix>${entry}</Prefix></CommonPrefixes>`]
      : [],
  )

  const objects = page.flatMap((key) =>
    key.endsWith("/")
      ? []
      : [
          `<Contents><Key>${key}</Key><LastModified>2026-10-30T09:00:00.000Z</LastModified><ETag>"e2e"</ETag><Size>48213</Size><StorageClass>STANDARD</StorageClass></Contents>`,
        ],
  )

  return {
    body: `<ListBucketResult xmlns="${S3_NAMESPACE}"><Name>photos</Name><Prefix>${prefix}</Prefix><KeyCount>${page.length}</KeyCount><MaxKeys>${maxKeys}</MaxKeys><Delimiter>/</Delimiter><IsTruncated>${truncated}</IsTruncated>${truncated ? `<NextContinuationToken>${start + maxKeys}</NextContinuationToken>` : ""}${folders.join("")}${objects.join("")}</ListBucketResult>`,
  }
}

function photosBucket(api: ConsoleApi) {
  api.handle("GET /proxy/s3/", () => ({
    body: `<ListAllMyBucketsResult xmlns="${S3_NAMESPACE}"><Owner><ID>e2e</ID></Owner><Buckets><Bucket><Name>photos</Name><CreationDate>2026-10-01T00:00:00.000Z</CreationDate></Bucket></Buckets></ListAllMyBucketsResult>`,
  }))
  api.handle("GET /proxy/s3/photos", listObjects)
}

test.describe("buckets", () => {
  test("opens a bucket from its row", async ({ api, page }) => {
    photosBucket(api)
    await page.goto("/s3/ls")

    await page
      .getByRole("row", { name: /photos/u })
      .getByRole("cell")
      .nth(2)
      .click()

    await expect(page).toHaveURL(/\/s3\/ls\/photos$/u)
    await expect(page.getByRole("heading", { name: "photos" })).toBeVisible()
  })
})

test.describe("object browser", () => {
  test("pages through more than a thousand objects", async ({ api, page }) => {
    photosBucket(api)
    await page.goto("/s3/ls/photos")

    const pages = page.getByRole("navigation", { name: "Pages" })
    await expect(pages).toContainText("Page 1 · 100 items")
    await expect(page.getByRole("link", { name: "2025/" })).toBeVisible()

    for (const shown of ["Page 2 ·", "Page 3 ·"]) {
      // oxlint-disable-next-line no-await-in-loop -- each page needs the last one's token
      await pages.getByRole("button", { name: "Next" }).click()
      // oxlint-disable-next-line no-await-in-loop
      await expect(pages).toContainText(shown)
    }

    // The two folders lead page 1, so page 3 holds keys 198 to 297.
    await expect(page.getByRole("link", { name: "img-0297.jpg" })).toBeVisible()
    await expect(page.getByRole("link", { name: "2025/" })).toBeHidden()

    await pages.getByRole("button", { name: "Previous" }).click()
    await expect(pages).toContainText("Page 2 ·")
    await expect(page.getByRole("link", { name: "img-0197.jpg" })).toBeVisible()
  })

  test("finds objects by prefix through the API", async ({ api, page }) => {
    photosBucket(api)
    await page.goto("/s3/ls/photos")

    await page.getByLabel("Find objects by prefix").fill("img-11")

    const pages = page.getByRole("navigation", { name: "Pages" })
    await expect(pages).toContainText("Page 1 · 100 items")
    await expect(page.getByRole("link", { name: "img-1100.jpg" })).toBeVisible()
    await expect(page.getByRole("link", { name: "img-0000.jpg" })).toBeHidden()
    await expect(pages.getByRole("button", { name: "Next" })).toHaveCount(0)
  })

  test("opens an object's details beside the list", async ({ api, page }) => {
    photosBucket(api)
    api.handle("HEAD /proxy/s3/photos/img-0003.jpg", () => ({
      body: "",
      contentType: "image/jpeg",
      headers: {
        "content-length": "48213",
        etag: '"e2e"',
        "last-modified": "Fri, 30 Oct 2026 09:00:00 GMT",
        "x-amz-meta-camera": "e2e-cam",
      },
    }))
    await page.goto("/s3/ls/photos")

    await page
      .getByRole("row", { name: /img-0003\.jpg/u })
      .getByRole("cell")
      .nth(3)
      .click()

    const panel = page.getByRole("region", { name: "Object details" })
    await expect(
      panel.getByRole("heading", { name: "img-0003.jpg" }),
    ).toBeVisible()
    await expect(panel.getByText("s3://photos/img-0003.jpg")).toBeVisible()
    await expect(panel.getByText("e2e-cam")).toBeVisible()
    await expect(panel.getByRole("button", { name: "Download" })).toBeVisible()
  })

  test("deletes the selected objects in one request", async ({ api, page }) => {
    photosBucket(api)
    const deleted: string[] = []
    api.handle("POST /proxy/s3/photos", () => ({
      body: `<DeleteResult xmlns="${S3_NAMESPACE}"/>`,
    }))
    await page.route("**/proxy/s3/photos?delete*", async (route) => {
      deleted.push(route.request().postData() ?? "")
      await route.fallback()
    })
    await page.goto("/s3/ls/photos")

    for (const key of ["img-0001.jpg", "img-0002.jpg"]) {
      // oxlint-disable-next-line no-await-in-loop -- one checkbox after another
      await page
        .getByRole("row", { name: new RegExp(key, "u") })
        .getByRole("checkbox")
        .check()
    }

    const bar = page.getByRole("toolbar", { name: "Selection actions" })
    await expect(bar).toContainText("2 selected")
    await bar.getByRole("button", { name: "Delete" }).click()

    const dialog = page.getByRole("alertdialog")
    await dialog.getByLabel(/^To confirm, type/u).fill("delete")
    await dialog.getByRole("button", { name: "Delete 2" }).click()

    await expect(dialog).toBeHidden()
    expect(deleted).toHaveLength(1)
    expect(deleted[0]).toContain("<Key>img-0001.jpg</Key>")
    expect(deleted[0]).toContain("<Key>img-0002.jpg</Key>")
  })
})

import { screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { beforeEach, describe, expect, it, vi } from "vitest"

vi.mock("@/lib/signed-fetch", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/signed-fetch")>()

  return { ...actual, signedFetch: vi.fn(), signedAdminFetch: vi.fn() }
})

import { SignedFetchError, signedAdminFetch } from "@/lib/signed-fetch"
import { createTestQueryClient, renderWithClient } from "@/test/render"
import type {
  ConsoleBrandingResponse,
  PutConsoleBrandingRequest,
} from "@/types/branding"

import { BrandingPage } from "./branding-page"

const mockAdmin = vi.mocked(signedAdminFetch)

const STORED: ConsoleBrandingResponse = {
  revision: 4,
  productName: "Acme Cloud",
  docsUrl: "https://docs.acme.example",
  primary: { light: "#0A4A8F" },
  logo: { contentType: "image/svg+xml", size: 41 },
  updatedBy: "admin",
  updatedAt: "2026-11-25T10:00:00Z",
}

// Answers the read with the stored record and records each write.
function serve(stored: ConsoleBrandingResponse = STORED) {
  const puts: PutConsoleBrandingRequest[] = []

  mockAdmin.mockImplementation(async ({ method, body }) => {
    if (method === "GetConsoleBranding") {
      return stored
    }

    // SAFETY: the page sends only PutConsoleBranding bodies besides the read.
    const put = body as PutConsoleBrandingRequest
    puts.push(put)

    return { ...stored, revision: stored.revision + 1 }
  })

  return puts
}

async function renderPage() {
  renderWithClient(<BrandingPage />, createTestQueryClient())

  return await screen.findByLabelText("Product name")
}

function previewLogo(theme: string): HTMLElement {
  return within(screen.getByRole("figure", { name: theme })).getByRole(
    "presentation",
  )
}

describe("BrandingPage", () => {
  beforeEach(() => {
    mockAdmin.mockReset()
  })

  it("saves the whole record and leaves an unchanged image out", async () => {
    const user = userEvent.setup()
    const puts = serve()
    const name = await renderPage()

    expect(name).toHaveValue("Acme Cloud")

    await user.clear(name)
    await user.type(name, "Acme")
    await user.click(screen.getByRole("combobox", { name: "Docs link" }))
    await user.click(
      await screen.findByRole("option", { name: "No Docs link" }),
    )
    await user.click(screen.getByRole("button", { name: "Save" }))

    await waitFor(() => {
      expect(puts).toHaveLength(1)
    })
    expect(puts[0]).toStrictEqual({
      productName: "Acme",
      docsUrl: "",
      primary: { light: "#0A4A8F", dark: undefined },
      logo: undefined,
      logoDark: undefined,
      favicon: undefined,
    })
    // The wire body leaves the stored logo out, so the gateway keeps it.
    expect(JSON.stringify(puts[0])).not.toContain('"logo"')
  })

  it("sends any #RRGGBB colour the operator chooses", async () => {
    const user = userEvent.setup()
    const puts = serve()
    await renderPage()

    const light = screen.getByLabelText("Primary colour, light theme")
    await user.clear(light)
    await user.type(light, "#FFFFFF")
    await user.click(screen.getByRole("button", { name: "Save" }))

    await waitFor(() => {
      expect(puts).toHaveLength(1)
    })
    expect(puts[0]?.primary?.light).toBe("#FFFFFF")
  })

  it("refuses a colour that is not #RRGGBB and does not send it", async () => {
    const user = userEvent.setup()
    const puts = serve()
    await renderPage()

    const light = screen.getByLabelText("Primary colour, light theme")
    await user.clear(light)
    await user.type(light, "blue")
    await user.click(screen.getByRole("button", { name: "Save" }))

    await expect(
      screen.findByText("Enter a colour as #RRGGBB."),
    ).resolves.toBeInTheDocument()
    expect(puts).toHaveLength(0)
  })

  it("sends a chosen image and removes a stored one", async () => {
    const user = userEvent.setup()
    const puts = serve()
    await renderPage()

    const favicon = new File(
      [new Uint8Array([0x89, 0x50, 0x4e, 0x47])],
      "f.png",
      {
        type: "image/png",
      },
    )

    await user.upload(screen.getByLabelText("Favicon"), favicon)
    await user.click(screen.getByRole("button", { name: "Remove Logo" }))
    await user.click(screen.getByRole("button", { name: "Save" }))

    await waitFor(() => {
      expect(puts).toHaveLength(1)
    })
    expect(puts[0]?.favicon).toStrictEqual({
      contentType: "image/png",
      data: "iVBORw==",
    })
    expect(puts[0]?.logo).toBeNull()
  })

  it("previews a removed logo as gone before it is saved", async () => {
    const user = userEvent.setup()
    serve()
    await renderPage()

    expect(previewLogo("Light theme")).toHaveAttribute(
      "src",
      "/branding/logo?rev=4",
    )
    expect(previewLogo("Dark theme")).toHaveAttribute(
      "src",
      "/branding/logo?rev=4",
    )

    await user.click(screen.getByRole("button", { name: "Remove Logo" }))

    expect(previewLogo("Light theme")).toHaveAttribute("src", "/mulga-logo.svg")
    expect(previewLogo("Dark theme")).toHaveAttribute("src", "/mulga-logo.svg")
  })

  it("refuses an image over the limit before sending it", async () => {
    const user = userEvent.setup()
    serve()
    await renderPage()

    const big = new File([new Uint8Array(33 * 1024)], "big.png", {
      type: "image/png",
    })

    await user.upload(screen.getByLabelText("Favicon"), big)

    expect(
      screen.getByText(/The file is 33\.0 KiB; at most 32\.0 KiB/u),
    ).toBeInTheDocument()
  })

  it("resets every value in one write after a confirm", async () => {
    const user = userEvent.setup()
    const puts = serve()
    await renderPage()

    await user.click(screen.getByRole("button", { name: "Reset to default" }))
    expect(puts).toHaveLength(0)
    await user.click(await screen.findByRole("button", { name: "Reset" }))

    await waitFor(() => {
      expect(puts).toStrictEqual([
        { logo: null, logoDark: null, favicon: null },
      ])
    })
  })

  it("names the missing grants when the principal is denied", async () => {
    mockAdmin.mockRejectedValue(
      new SignedFetchError(
        "GetConsoleBranding failed: 403",
        "AccessDenied",
        403,
      ),
    )
    renderWithClient(<BrandingPage />, createTestQueryClient())

    await expect(
      screen.findByText(
        /spinifex:GetConsoleBranding and spinifex:PutConsoleBranding/u,
      ),
    ).resolves.toBeInTheDocument()
    expect(screen.queryByLabelText("Product name")).toBeNull()
  })
})

import { test as base, expect, type Page } from "@playwright/test"

export interface LiveConfig {
  consoleUrl: string
  accessKeyId: string
  secretAccessKey: string
  instanceType: string
}

function required(name: string): string {
  const value = process.env[name]

  if (!value) {
    throw new Error(
      `${name} is not set: the live console suite needs a cluster to run against`,
    )
  }

  return value
}

export function liveConfig(): LiveConfig {
  return {
    consoleUrl: required("SPX_CONSOLE_URL"),
    accessKeyId: required("SPX_ACCESS_KEY_ID"),
    secretAccessKey: required("SPX_SECRET_ACCESS_KEY"),
    instanceType: process.env.SPX_INSTANCE_TYPE ?? "t3.micro",
  }
}

// Signs in with a long-lived access key, as an operator does. The credentials
// are read from the environment and never written to disk.
export async function signIn(page: Page): Promise<void> {
  const config = liveConfig()

  await page.goto("/login")
  await page.locator("#accessKeyId").fill(config.accessKeyId)
  await page.locator("#secretAccessKey").fill(config.secretAccessKey)
  await page.getByRole("button", { name: "Sign In" }).click()

  // The console routes away from /login once the session is stored.
  await expect(page).not.toHaveURL(/\/login/, { timeout: 30_000 })
}

export const test = base.extend<{ signedIn: Page }>({
  // The second argument is Playwright's fixture callback, not React's use().
  signedIn: async ({ page }, provide) => {
    await signIn(page)
    await provide(page)
  },
})

export { expect } from "@playwright/test"

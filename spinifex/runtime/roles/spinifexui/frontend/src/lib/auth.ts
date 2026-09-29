import { z } from "zod"

// Versioned storage key - increment version when schema changes. v2 holds
// short-lived STS session credentials; abandoned v1 long-lived entries simply
// force a one-time re-login.
const STORAGE_KEY = "spinifex:v2:aws-session"

// Long-lived credentials entered on the login form. Used once to exchange for
// session credentials via STS GetSessionToken and never persisted.
export const awsCredentialsSchema = z.object({
  accessKeyId: z
    .string()
    .min(16, "Access Key ID must be at least 16 characters"),
  secretAccessKey: z.string().min(1, "Secret Access Key is required"),
})

export type AwsCredentialsInput = z.infer<typeof awsCredentialsSchema>

// Short-lived STS session credentials persisted in localStorage.
export const sessionCredentialsSchema = z.object({
  accessKeyId: z.string().min(1),
  secretAccessKey: z.string().min(1),
  sessionToken: z.string().min(1),
  expiration: z.string().min(1),
})

export type SessionCredentials = z.infer<typeof sessionCredentialsSchema>

// In-memory cache to avoid repeated localStorage reads
let credentialsCache: SessionCredentials | null | undefined

function isExpired(expiration: string): boolean {
  const expiresAt = Date.parse(expiration)
  return Number.isNaN(expiresAt) || expiresAt <= Date.now()
}

function readStoredCredentials(): SessionCredentials | null {
  try {
    const stored = localStorage.getItem(STORAGE_KEY)
    if (!stored) {
      return null
    }
    const parsed: unknown = JSON.parse(stored)
    const result = sessionCredentialsSchema.safeParse(parsed)
    return result.success ? result.data : null
  } catch {
    return null
  }
}

export function getCredentials(): SessionCredentials | null {
  if (credentialsCache === undefined) {
    credentialsCache = readStoredCredentials()
  }
  // Treat expired sessions as logged out so route guards redirect to /login.
  if (credentialsCache && isExpired(credentialsCache.expiration)) {
    clearCredentials()
    return null
  }
  return credentialsCache
}

export function setSessionCredentials(credentials: SessionCredentials): void {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(credentials))
    credentialsCache = credentials
  } catch {
    // localStorage might be full or disabled - cache in memory only
    credentialsCache = credentials
  }
}

export function clearCredentials(): void {
  try {
    localStorage.removeItem(STORAGE_KEY)
  } catch {
    // Ignore errors when clearing
  }
  credentialsCache = null
}

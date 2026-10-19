import type { ReactNode } from "react"
import { vi } from "vitest"

type RouterModule = typeof import("@tanstack/react-router")

interface MockLinkProps {
  children?: ReactNode
  className?: string
  onClick?: () => void
  params?: Record<string, string>
  to?: string
}

// Renders the href the real router would build, with `$param` segments
// substituted, so tests can assert link targets without a router context.
function MockLink({ children, className, onClick, params, to }: MockLinkProps) {
  let href = to

  for (const [key, value] of Object.entries(params ?? {})) {
    href = href?.replace(`$${key}`, value)
  }

  return (
    <a className={className} href={href} onClick={onClick}>
      {children}
    </a>
  )
}

interface MockRouterOptions {
  navigate?: (...args: never[]) => unknown
  // Path params returned by `Route.useParams()` of file routes under test.
  params?: Record<string, string>
}

// The real router module with a stub `Link`, `useNavigate` returning `navigate`
// and, given `params`, a `createFileRoute` whose route returns them. Import it
// inside the vi.mock factory: a static import can still be uninitialised there.
export async function mockRouter(
  importOriginal: <T>() => Promise<T>,
  { navigate = vi.fn(), params }: MockRouterOptions = {},
) {
  const actual = await importOriginal<RouterModule>()

  return {
    ...actual,
    Link: MockLink,
    useNavigate: () => navigate,
    ...(params && {
      createFileRoute:
        () =>
        <TOptions,>(options: TOptions) => ({
          options,
          useParams: () => params,
        }),
    }),
  }
}

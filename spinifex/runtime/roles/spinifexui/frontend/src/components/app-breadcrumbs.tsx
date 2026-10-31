import { Link, useLocation } from "@tanstack/react-router"
import { Fragment } from "react"

import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from "@/components/ui/breadcrumb"
import { useAdmin } from "@/hooks/use-admin"
import { isOchreEnabled } from "@/lib/cluster-config"
import { breadcrumbTrail, visibleNav } from "@/lib/nav"

// AWS resource IDs (i-0a1b…, vpc-82…) read as identifiers, so they are mono.
const RESOURCE_ID = /^[a-z]+(?:-[a-z]+)?-[0-9a-f]{8,}$/u

// The trail in the top bar. A phone shows only the current page, so the bar
// never wraps; the service is one tap away in the sidebar sheet.
export function AppBreadcrumbs() {
  const pathname = useLocation({ select: (location) => location.pathname })
  const { isAdmin } = useAdmin()

  const trail = breadcrumbTrail(
    visibleNav({ isAdmin, ochreEnabled: isOchreEnabled() }),
    pathname,
  )

  if (trail.length === 0) {
    return null
  }

  return (
    <Breadcrumb className="min-w-0">
      <BreadcrumbList className="flex-nowrap">
        {trail.map((crumb, index) => {
          const isLast = index === trail.length - 1

          return (
            <Fragment key={crumb.to ?? crumb.label}>
              {index > 0 && <BreadcrumbSeparator className="max-sm:hidden" />}
              <BreadcrumbItem className={isLast ? "min-w-0" : "max-sm:hidden"}>
                {crumb.to && !isLast ? (
                  <BreadcrumbLink render={<Link to={crumb.to} />}>
                    {crumb.label}
                  </BreadcrumbLink>
                ) : (
                  <BreadcrumbPage>
                    <span
                      className={
                        RESOURCE_ID.test(crumb.label)
                          ? "block truncate font-mono"
                          : "block truncate"
                      }
                    >
                      {crumb.label}
                    </span>
                  </BreadcrumbPage>
                )}
              </BreadcrumbItem>
            </Fragment>
          )
        })}
      </BreadcrumbList>
    </Breadcrumb>
  )
}

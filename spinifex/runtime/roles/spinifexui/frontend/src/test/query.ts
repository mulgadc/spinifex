import {
  type QueryClient,
  type Query,
  type QueryFunction,
  type QueryKey,
  type SkipToken,
  skipToken,
} from "@tanstack/react-query"

import { createTestQueryClient } from "@/test/render"

interface CallableQueryOptions<TData, TQueryKey extends QueryKey> {
  queryKey: TQueryKey
  queryFn?: QueryFunction<TData, TQueryKey> | SkipToken
}

type RefetchInterval = number | false | undefined

interface PollingQueryOptions<TData, TQueryKey extends QueryKey> {
  refetchInterval?:
    | RefetchInterval
    | ((query: Query<TData, Error, TData, TQueryKey>) => RefetchInterval)
}

// Calls a query's fetcher with a real context, so a query that starts reading
// signal or client fails loudly here instead of silently taking another branch.
// The return type is preserved, so callers can assert on the response.
export async function callQueryFn<TData, TQueryKey extends QueryKey>(
  options: CallableQueryOptions<TData, TQueryKey>,
  client: QueryClient = createTestQueryClient(),
): Promise<TData> {
  const { queryFn, queryKey } = options

  if (queryFn === undefined || queryFn === skipToken) {
    throw new TypeError(
      `query options ${JSON.stringify(queryKey)} have no callable queryFn`,
    )
  }

  return await queryFn({
    client,
    meta: undefined,
    queryKey,
    signal: new AbortController().signal,
  })
}

// Asks a query what cadence it would poll at for the given cached data, so a
// test can assert the interval without standing up a live query. The data is
// partial: a caller supplies only the fields the cadence reads.
export function callRefetchInterval<TData, TQueryKey extends QueryKey>(
  options: PollingQueryOptions<TData, TQueryKey>,
  data: Partial<TData>,
): RefetchInterval {
  const { refetchInterval } = options

  // oxlint-disable-next-line anti-slop/no-runtime-typeof -- the option holds either a fixed interval or a function of the query
  if (typeof refetchInterval !== "function") {
    throw new TypeError("query options have no refetchInterval function")
  }

  // SAFETY: the option only reads state.data, which is all this stub carries.
  return refetchInterval({ state: { data } } as Query<
    TData,
    Error,
    TData,
    TQueryKey
  >)
}

import {
  QueryClient,
  type QueryFunction,
  type QueryKey,
  type SkipToken,
  skipToken,
} from "@tanstack/react-query"

interface CallableQueryOptions<TData, TQueryKey extends QueryKey> {
  queryKey: TQueryKey
  queryFn?: QueryFunction<TData, TQueryKey> | SkipToken
}

function createQueryClient() {
  return new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
}

// Calls a query's fetcher with a real context, so a query that starts reading
// signal or client fails loudly here instead of silently taking another branch.
// The return type is preserved, so callers can assert on the response.
export async function callQueryFn<TData, TQueryKey extends QueryKey>(
  options: CallableQueryOptions<TData, TQueryKey>,
  client: QueryClient = createQueryClient(),
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

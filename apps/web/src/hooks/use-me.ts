import { useQuery, type UseQueryOptions } from "@tanstack/react-query"
import { api } from "@/lib/api"
import type { User } from "@/lib/api"

type MeResponse = { user: User }

export function useMe(
  options?: Omit<UseQueryOptions<MeResponse, Error, MeResponse, ["me"]>, "queryKey" | "queryFn">,
) {
  return useQuery({
    queryKey: ["me"],
    queryFn: api.me,
    retry: 1,
    ...options,
  })
}

export function isTimeoutError(error: unknown): boolean {
  return error instanceof Error && error.message.includes("请求超时")
}

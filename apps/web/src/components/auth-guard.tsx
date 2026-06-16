import React from "react"
import { Navigate, useLocation } from "react-router-dom"
import { useMe, isTimeoutError } from "@/hooks/use-me"
import { AuthLoading, AuthError } from "@/components/auth-states"

export function AuthGuard({ children }: { children: React.ReactNode }) {
  const me = useMe()
  const location = useLocation()

  if (me.isLoading) return <AuthLoading />
  if (me.isError && isTimeoutError(me.error)) return <AuthError message={me.error.message} onRetry={() => me.refetch()} />
  if (me.isError || !me.data?.user) return <Navigate to="/login" replace state={{ from: location.pathname }} />

  return <>{children}</>
}

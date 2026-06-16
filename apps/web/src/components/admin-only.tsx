import React from "react"
import { Navigate } from "react-router-dom"
import { useMe } from "@/hooks/use-me"

export function AdminOnly({ children }: { children: React.ReactNode }) {
  const me = useMe()
  if (me.isLoading) return null
  if (!me.data?.user) return <Navigate to="/login" replace />
  if (me.data.user.role !== "admin") return <Navigate to="/" replace />
  return <>{children}</>
}

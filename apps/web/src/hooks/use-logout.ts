import { useCallback } from "react"
import { useNavigate } from "react-router-dom"
import { useQueryClient } from "@tanstack/react-query"
import { api } from "@/lib/api"

export function useLogout() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  return useCallback(async () => {
    await api.logout().catch(() => undefined)
    qc.clear()
    navigate("/login", { replace: true })
  }, [qc, navigate])
}

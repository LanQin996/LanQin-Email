import * as React from "react"
import { Link, Navigate } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { api } from "@/lib/api"
import { useMe } from "@/hooks/use-me"
import { TurnstileBox } from "@/components/turnstile-box"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { useToast } from "@/hooks/use-toast"

export function LoginPage() {
  const me = useMe()
  const qc = useQueryClient()
  const { toast } = useToast()
  const publicSettings = useQuery({ queryKey: ["public-settings"], queryFn: api.publicSettings })
  const [turnstileToken, setTurnstileToken] = React.useState("")
  const [challengeToken, setChallengeToken] = React.useState("")
  const login = useMutation({
    mutationFn: (form: FormData) => challengeToken
      ? api.login({ challengeToken, twoFactorCode: String(form.get("twoFactorCode") || "") })
      : api.login({ email: String(form.get("email") || ""), password: String(form.get("password") || ""), turnstileToken }),
    onSuccess: async (data) => {
      if (data.twoFactorRequired && data.challengeToken) {
        setChallengeToken(data.challengeToken)
        toast({ title: "请输入双因素验证码" })
        return
      }
      await qc.invalidateQueries({ queryKey: ["me"] })
    },
    onError: (e) => toast({ title: "登录失败", description: e.message }),
  })
  const turnstileRequired = !!publicSettings.data?.turnstileEnabled
  if (me.data?.user) return <Navigate to="/" replace />
  return (
    <div className="grid min-h-screen place-items-center bg-background px-4">
      <div className="w-full max-w-[360px]">
        <div className="mb-10 text-center">
          <h1 className="text-3xl font-bold tracking-tight">LanQin Email</h1>
        </div>
        <form className="space-y-5" onSubmit={(e) => { e.preventDefault(); if (!challengeToken && turnstileRequired && !turnstileToken) { toast({ title: "请先完成人机验证" }); return }; login.mutate(new FormData(e.currentTarget)) }}>
          {!challengeToken ? (
            <>
              <div className="space-y-2">
                <Label htmlFor="email">邮箱</Label>
                <Input id="email" name="email" type="email" autoComplete="username" required className="h-11 text-base" />
              </div>
              <div className="space-y-2">
                <Label htmlFor="password">密码</Label>
                <Input id="password" name="password" type="password" autoComplete="current-password" required className="h-11 text-base" />
              </div>
            </>
          ) : (
            <div className="space-y-2">
              <Label htmlFor="twoFactorCode">双因素验证码</Label>
              <Input id="twoFactorCode" name="twoFactorCode" inputMode="numeric" autoComplete="one-time-code" minLength={6} maxLength={6} required className="h-11 text-base" />
            </div>
          )}
          {!challengeToken && turnstileRequired && (
            <TurnstileBox siteKey={publicSettings.data?.turnstileSiteKey || ""} onToken={setTurnstileToken} />
          )}
          <Button className="h-11 w-full text-base" disabled={login.isPending}>
            {login.isPending ? "登录中..." : challengeToken ? "验证登录" : "登录"}
          </Button>
          {challengeToken && <Button type="button" variant="ghost" className="w-full" onClick={() => setChallengeToken("")}>返回登录</Button>}
          {!challengeToken && (
            <Button type="button" variant="ghost" className="w-full" asChild>
              <Link to="/register">注册账号</Link>
            </Button>
          )}
        </form>
      </div>
    </div>
  )
}


import * as React from "react"
import { Link, Navigate, useNavigate } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { api } from "@/lib/api"
import type { PublicDomain } from "@/lib/api"
import { useMe } from "@/hooks/use-me"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { useToast } from "@/hooks/use-toast"
import { PasswordInput } from "@/components/ui/password-input"
import { TurnstileBox } from "@/components/turnstile-box"
import { validatePasswordConfirm } from "@/lib/validation"

export function RegisterPage() {
  const me = useMe()
  const qc = useQueryClient()
  const navigate = useNavigate()
  const { toast } = useToast()
  const publicSettings = useQuery({ queryKey: ["public-settings"], queryFn: api.publicSettings })
  const [turnstileToken, setTurnstileToken] = React.useState("")
  const [domainId, setDomainId] = React.useState("")
  const domains: PublicDomain[] = publicSettings.data?.mailboxDomains || []
  const selectedDomain = domains.find((d) => d.id === domainId)

  const register = useMutation({
    mutationFn: (form: FormData) => {
      const password = String(form.get("password") || "")
      const confirmPassword = String(form.get("confirmPassword") || "")
      validatePasswordConfirm(password, confirmPassword)

      if (domainId && selectedDomain) {
        const localPart = String(form.get("localPart") || "").trim()
        if (!localPart) throw new Error("请输入邮箱前缀")
        return api.register({
          email: `${localPart}@${selectedDomain.name}`,
          displayName: String(form.get("displayName") || ""),
          password,
          turnstileToken,
          domainId,
          localPart,
        })
      }

      // Fallback: no domains available, use email directly
      return api.register({
        email: String(form.get("email") || ""),
        displayName: String(form.get("displayName") || ""),
        password,
        turnstileToken,
      })
    },
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ["me"] })
      toast({ title: "注册成功" })
      navigate("/profile", { replace: true })
    },
    onError: (e) => toast({ title: "注册失败", description: e.message }),
  })
  const turnstileRequired = !!publicSettings.data?.turnstileEnabled
  if (me.data?.user) return <Navigate to="/" replace />
  return (
    <div className="grid min-h-screen place-items-center bg-background px-4">
      <div className="w-full max-w-[360px]">
        <div className="mb-10 text-center">
          <h1 className="text-3xl font-bold tracking-tight">注册账号</h1>
        </div>
        {publicSettings.isSuccess && !publicSettings.data.openRegistration ? (
          <div className="space-y-4">
            <div className="rounded-md border p-4 text-center text-sm text-muted-foreground">当前未开放注册</div>
            <Button type="button" variant="outline" className="w-full" asChild>
              <Link to="/login">返回登录</Link>
            </Button>
          </div>
        ) : (
          <form className="space-y-5" onSubmit={(e) => { e.preventDefault(); if (turnstileRequired && !turnstileToken) { toast({ title: "请先完成人机验证" }); return }; register.mutate(new FormData(e.currentTarget)) }}>
            {domains.length > 0 ? (
              <div className="space-y-2">
                <Label htmlFor="localPart">邮箱地址</Label>
                <div className="flex items-center gap-2">
                  <Input id="localPart" name="localPart" className="h-11 flex-1 text-base" placeholder="your-name" required />
                  <span className="text-sm text-muted-foreground">@</span>
                  <Select value={domainId} onValueChange={setDomainId} required>
                    <SelectTrigger className="h-11 w-[140px]">
                      <SelectValue placeholder="选择域名" />
                    </SelectTrigger>
                    <SelectContent>
                      {domains.map((d) => (
                        <SelectItem key={d.id} value={d.id}>{d.name}</SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </div>
              </div>
            ) : (
              <div className="space-y-2">
                <Label htmlFor="email">邮箱</Label>
                <Input id="email" name="email" type="email" autoComplete="username" required className="h-11 text-base" />
              </div>
            )}
            <div className="space-y-2">
              <Label htmlFor="displayName">显示名称</Label>
              <Input id="displayName" name="displayName" autoComplete="name" className="h-11 text-base" />
            </div>
            <div className="space-y-2">
              <Label htmlFor="password">密码</Label>
              <PasswordInput id="password" name="password" autoComplete="new-password" minLength={8} required className="h-11 text-base" />
            </div>
            <div className="space-y-2">
              <Label htmlFor="confirmPassword">确认密码</Label>
              <PasswordInput id="confirmPassword" name="confirmPassword" autoComplete="new-password" minLength={8} required className="h-11 text-base" />
            </div>
            {turnstileRequired && <TurnstileBox siteKey={publicSettings.data?.turnstileSiteKey || ""} onToken={setTurnstileToken} />}
            <Button className="h-11 w-full text-base" disabled={register.isPending || publicSettings.isLoading}>
              {register.isPending ? "注册中..." : "注册"}
            </Button>
            <Button type="button" variant="ghost" className="w-full" asChild>
              <Link to="/login">返回登录</Link>
            </Button>
          </form>
        )}
      </div>
    </div>
  )
}

import * as React from "react"
import { Link, Navigate, useNavigate } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { ArrowLeft, ArrowRight, UserPlus } from "lucide-react"
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
    <div className="flex min-h-screen items-center justify-center bg-muted/20 px-4 py-10">
      <div className="w-full max-w-[420px]">
        <div className="mb-7 text-center">
          <h1 className="text-3xl font-semibold tracking-tight">LanQin Email</h1>
        </div>
        <div className="rounded-lg border bg-background p-6 shadow-sm sm:p-7">
          <div className="mb-6 flex items-center gap-2 text-sm font-medium text-muted-foreground">
            <UserPlus className="h-4 w-4" />
            注册账号
          </div>
          {publicSettings.isSuccess && !publicSettings.data.openRegistration ? (
            <div className="space-y-5">
              <div className="rounded-md bg-muted/40 px-4 py-3 text-center text-sm text-muted-foreground">当前未开放注册</div>
              <Button type="button" variant="outline" className="h-11 w-full text-base" asChild>
                <Link to="/login">
                  <ArrowLeft className="h-4 w-4" />
                  返回登录
                </Link>
              </Button>
            </div>
          ) : (
            <form className="space-y-5" onSubmit={(e) => { e.preventDefault(); if (turnstileRequired && !turnstileToken) { toast({ title: "请先完成人机验证" }); return }; register.mutate(new FormData(e.currentTarget)) }}>
              {domains.length > 0 ? (
                <div className="space-y-2">
                  <Label htmlFor="localPart" className="text-sm font-medium">邮箱地址</Label>
                  <div className="grid grid-cols-1 gap-2 sm:grid-cols-[minmax(0,1fr)_150px]">
                    <Input id="localPart" name="localPart" className="h-11 text-base" placeholder="邮箱前缀" required />
                    <Select value={domainId} onValueChange={setDomainId} required>
                      <SelectTrigger className="h-11">
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
                  <Label htmlFor="email" className="text-sm font-medium">邮箱</Label>
                  <Input id="email" name="email" type="email" autoComplete="username" required className="h-11 text-base" />
                </div>
              )}
              <div className="space-y-2">
                <Label htmlFor="displayName" className="text-sm font-medium">显示名称</Label>
                <Input id="displayName" name="displayName" autoComplete="name" className="h-11 text-base" />
              </div>
              <div className="space-y-2">
                <Label htmlFor="password" className="text-sm font-medium">密码</Label>
                <PasswordInput id="password" name="password" autoComplete="new-password" minLength={8} required className="h-11 text-base" />
              </div>
              <div className="space-y-2">
                <Label htmlFor="confirmPassword" className="text-sm font-medium">确认密码</Label>
                <PasswordInput id="confirmPassword" name="confirmPassword" autoComplete="new-password" minLength={8} required className="h-11 text-base" />
              </div>
              {turnstileRequired && <TurnstileBox siteKey={publicSettings.data?.turnstileSiteKey || ""} onToken={setTurnstileToken} />}
              <Button className="h-11 w-full text-base" disabled={register.isPending || publicSettings.isLoading}>
                {register.isPending ? "注册中..." : "注册"}
                {!register.isPending && <ArrowRight className="h-4 w-4" />}
              </Button>
            </form>
          )}
        </div>
        {!(publicSettings.isSuccess && !publicSettings.data.openRegistration) && (
          <div className="mt-5 flex items-center justify-center gap-2 text-sm text-muted-foreground">
            <span>已有账号？</span>
            <Button type="button" variant="link" className="h-auto px-0 text-sm" asChild>
              <Link to="/login">返回登录</Link>
            </Button>
          </div>
        )}
      </div>
    </div>
  )
}

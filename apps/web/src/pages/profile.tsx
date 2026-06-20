import * as React from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import type { ImperativePanelHandle } from "react-resizable-panels"
import { useNavigate, useSearchParams } from "react-router-dom"
import { ArrowLeft, BarChart3, Ban, Contact, Copy, Info, KeyRound, Laptop, LogOut, Mail, MailCheck, MailX, Moon, PanelLeftClose, PanelLeftOpen, PencilLine, Plus, RefreshCcw, Settings, ShieldCheck, SlidersHorizontal, Sun, Trash2, X } from "lucide-react"
import { QRCodeSVG } from "qrcode.react"
import { api, MailLabel, MailRule, MailRuleAction, MailRuleCondition, Mailbox, MailboxApplyOptions, MailSignature, MailStats } from "@/lib/api"
import { cn, formatBytes } from "@/lib/utils"
import { applyTheme, getInitialTheme } from "@/lib/theme"
import { DisplayMode, useDisplayMode } from "@/lib/display-mode"
import { useMe } from "@/hooks/use-me"
import { useLogout } from "@/hooks/use-logout"
import { useIsMobile } from "@/hooks/use-mobile"
import { validatePasswordConfirm } from "@/lib/validation"
import { Button } from "@/components/ui/button"
import { PasswordInput } from "@/components/ui/password-input"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Textarea } from "@/components/ui/textarea"
import { Badge } from "@/components/ui/badge"
import { Avatar, AvatarFallback } from "@/components/ui/avatar"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Checkbox } from "@/components/ui/checkbox"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Sheet, SheetContent, SheetTitle, SheetTrigger } from "@/components/ui/sheet"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Separator } from "@/components/ui/separator"
import { ScrollArea } from "@/components/ui/scroll-area"
import { ResizableHandle, ResizablePanel, ResizablePanelGroup } from "@/components/ui/resizable"
import { Sidebar, SidebarContent, SidebarGroup, SidebarGroupContent, SidebarGroupLabel, SidebarHeader, SidebarMenu, SidebarMenuButton, SidebarMenuItem, SidebarProvider } from "@/components/ui/sidebar"
import { ConfirmDialog } from "@/components/confirm-dialog"
import { useToast } from "@/hooks/use-toast"

type Tab = "profile" | "mailboxes" | "clients" | "signatures" | "contacts" | "cleanup" | "rules" | "blocked" | "stats"
type PendingConfirm = { title: string; description?: string; confirmText: string; destructive?: boolean; onConfirm: () => void }
const tabs: Record<Tab, { label: string; icon: React.ReactNode }> = {
  profile: { label: "账户资料", icon: <Settings className="h-4 w-4" /> },
  mailboxes: { label: "邮箱管理", icon: <Mail className="h-4 w-4" /> },
  clients: { label: "第三方客户端", icon: <Laptop className="h-4 w-4" /> },
  signatures: { label: "签名管理", icon: <KeyRound className="h-4 w-4" /> },
  contacts: { label: "联系人管理", icon: <Contact className="h-4 w-4" /> },
  cleanup: { label: "邮件清理", icon: <Trash2 className="h-4 w-4" /> },
  rules: { label: "收件规则", icon: <SlidersHorizontal className="h-4 w-4" /> },
  blocked: { label: "被拦截邮件", icon: <Ban className="h-4 w-4" /> },
  stats: { label: "数据统计", icon: <BarChart3 className="h-4 w-4" /> },
}
const tabKeys = Object.keys(tabs) as Tab[]
const actionLabels: Record<string, string> = { archive: "移入归档", trash: "移入回收站", star: "添加星标", "mark-read": "标记已读" }

export function ProfilePage() {
  const me = useMe()
  const qc = useQueryClient()
  const navigate = useNavigate()
  const [params, setParams] = useSearchParams()
  const { toast } = useToast()
  const passwordFormRef = React.useRef<HTMLFormElement>(null)
  const twoFactorFormRef = React.useRef<HTMLFormElement>(null)
  const sidebarPanelRef = React.useRef<ImperativePanelHandle>(null)
  const [sidebarCollapsed, setSidebarCollapsed] = React.useState(false)
  const [mailboxId, setMailboxId] = React.useState(() => localStorage.getItem("lanqin:selected-mailbox") || "")
  const [darkMode, setDarkMode] = React.useState(getInitialTheme)
  const [displayMode, setDisplayMode] = useDisplayMode()
  const [blockedMailboxId, setBlockedMailboxId] = React.useState("all")
  const [ruleDialogOpen, setRuleDialogOpen] = React.useState(false)
  const [mobileSidebarOpen, setMobileSidebarOpen] = React.useState(false)
  const isMobile = useIsMobile()
  const themeMountedRef = React.useRef(false)

  const rawTab = params.get("tab") as Tab | null
  const tab: Tab = rawTab && tabKeys.includes(rawTab) ? rawTab : "profile"
  const user = me.data?.user
  const mailboxes = useQuery({ queryKey: ["mailboxes", "mine"], queryFn: api.myMailboxes })
  const mailboxApplyOptions = useQuery({ queryKey: ["mailbox-apply-options"], queryFn: api.mailboxApplyOptions })
  const publicSettings = useQuery({ queryKey: ["public-settings"], queryFn: api.publicSettings })
  const contacts = useQuery({ queryKey: ["contacts"], queryFn: api.contacts })
  const signatures = useQuery({ queryKey: ["signatures"], queryFn: api.signatures })
  const rules = useQuery({ queryKey: ["rules"], queryFn: api.rules })
  const blocked = useQuery({ queryKey: ["blocked-senders"], queryFn: api.blockedSenders })
  const selectedMailbox = React.useMemo(() => mailboxes.data?.items.find((m) => m.id === mailboxId), [mailboxes.data?.items, mailboxId])
  const activeMailboxId = selectedMailbox?.id || ""
  const ruleLabels = useQuery({ queryKey: ["labels", "rules", activeMailboxId], queryFn: () => api.labels(activeMailboxId), enabled: !!activeMailboxId })
  const stats = useQuery({ queryKey: ["mail-stats", activeMailboxId], queryFn: () => api.mailStats(activeMailboxId), enabled: !!activeMailboxId })

  const profile = useMutation({
    mutationFn: (form: FormData) => api.updateProfile({ displayName: String(form.get("displayName") || "") }),
    onSuccess: (data) => { qc.setQueryData(["me"], data); toast({ title: "个人资料已保存" }) },
    onError: (error) => toast({ title: "保存失败", description: error.message }),
  })
  const password = useMutation({
    mutationFn: (form: FormData) => {
      const newPassword = String(form.get("newPassword") || "")
      validatePasswordConfirm(newPassword, String(form.get("confirmPassword") || ""), "两次输入的新密码不一致")
      return api.changePassword({ currentPassword: String(form.get("currentPassword") || ""), newPassword })
    },
    onSuccess: () => { passwordFormRef.current?.reset(); toast({ title: "密码已更新" }) },
    onError: (error) => toast({ title: "修改失败", description: error.message }),
  })
  const setupTwoFactor = useMutation({
    mutationFn: api.setupTwoFactor,
    onSuccess: () => toast({ title: "双因素密钥已生成" }),
    onError: (error) => toast({ title: "生成失败", description: error.message }),
  })
  const enableTwoFactor = useMutation({
    mutationFn: (form: FormData) => api.enableTwoFactor(String(form.get("code") || "")),
    onSuccess: (data) => { qc.setQueryData(["me"], data); setupTwoFactor.reset(); twoFactorFormRef.current?.reset(); toast({ title: "双因素认证已启用" }) },
    onError: (error) => toast({ title: "启用失败", description: error.message }),
  })
  const disableTwoFactor = useMutation({
    mutationFn: (form: FormData) => api.disableTwoFactor(String(form.get("code") || "")),
    onSuccess: (data) => { qc.setQueryData(["me"], data); twoFactorFormRef.current?.reset(); toast({ title: "双因素认证已关闭" }) },
    onError: (error) => toast({ title: "关闭失败", description: error.message }),
  })
  const createContact = useMutation({
    mutationFn: (form: FormData) => api.createContact({ name: String(form.get("name") || ""), email: String(form.get("email") || ""), note: String(form.get("note") || "") }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["contacts"] }); toast({ title: "联系人已保存" }) },
    onError: (error) => toast({ title: "保存失败", description: error.message }),
  })
  const deleteContact = useMutation({ mutationFn: api.deleteContact, onSuccess: () => { qc.invalidateQueries({ queryKey: ["contacts"] }); toast({ title: "联系人已删除" }) } })
  const createSignature = useMutation({
    mutationFn: (form: FormData) => api.createSignature({ mailboxId: String(form.get("mailboxId") || ""), name: String(form.get("name") || ""), content: String(form.get("content") || ""), isDefault: form.get("isDefault") === "on" }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["signatures"] }); qc.invalidateQueries({ queryKey: ["signature"] }); toast({ title: "签名已保存" }) },
    onError: (error) => toast({ title: "保存失败", description: error.message }),
  })
  const updateSignature = useMutation({
    mutationFn: ({ id, form }: { id: string; form: FormData }) => api.updateSignature(id, { mailboxId: String(form.get("mailboxId") || ""), name: String(form.get("name") || ""), content: String(form.get("content") || ""), isDefault: form.get("isDefault") === "on" }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["signatures"] }); qc.invalidateQueries({ queryKey: ["signature"] }); toast({ title: "签名已更新" }) },
    onError: (error) => toast({ title: "保存失败", description: error.message }),
  })
  const setDefaultSignature = useMutation({
    mutationFn: api.setDefaultSignature,
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["signatures"] }); qc.invalidateQueries({ queryKey: ["signature"] }); toast({ title: "默认签名已更新" }) },
    onError: (error) => toast({ title: "设置失败", description: error.message }),
  })
  const deleteSignature = useMutation({ mutationFn: api.deleteSignature, onSuccess: () => { qc.invalidateQueries({ queryKey: ["signatures"] }); qc.invalidateQueries({ queryKey: ["signature"] }); toast({ title: "签名已删除" }) } })
  const createRule = useMutation({
    mutationFn: (payload: {
      mailboxId: string
      name: string
      matchMode: "all" | "any"
      conditions: MailRuleCondition[]
      actions: MailRuleAction[]
      applyToExisting: boolean
      stopProcessing: boolean
      enabled: boolean
    }) => api.createRule(payload),
    onSuccess: (rule) => {
      qc.invalidateQueries({ queryKey: ["rules"] })
      qc.invalidateQueries({ queryKey: ["messages"] })
      qc.invalidateQueries({ queryKey: ["mail-stats"] })
      qc.invalidateQueries({ queryKey: ["labels"] })
      setRuleDialogOpen(false)
      toast({ title: rule.appliedExistingCount ? `收件规则已保存，已应用 ${rule.appliedExistingCount} 封邮件` : "收件规则已保存" })
    },
    onError: (error) => toast({ title: "保存失败", description: error.message }),
  })
  const deleteRule = useMutation({ mutationFn: api.deleteRule, onSuccess: () => { qc.invalidateQueries({ queryKey: ["rules"] }); toast({ title: "规则已删除" }) } })
  const createBlocked = useMutation({
    mutationFn: (form: FormData) => api.createBlockedSender({ mailboxId: blockedMailboxId === "all" ? "" : blockedMailboxId, email: String(form.get("email") || ""), reason: String(form.get("reason") || "") }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["blocked-senders"] }); toast({ title: "拦截规则已保存" }) },
    onError: (error) => toast({ title: "保存失败", description: error.message }),
  })
  const deleteBlocked = useMutation({ mutationFn: api.deleteBlockedSender, onSuccess: () => { qc.invalidateQueries({ queryKey: ["blocked-senders"] }); toast({ title: "拦截规则已删除" }) } })
  const cleanup = useMutation({
    mutationFn: (target: "empty-trash" | "empty-spam" | "archive-read-inbox") => api.cleanupMail({ mailboxId, target }),
    onSuccess: (res) => { qc.invalidateQueries({ queryKey: ["mail-stats"] }); qc.invalidateQueries({ queryKey: ["folders"] }); qc.invalidateQueries({ queryKey: ["messages"] }); toast({ title: `已处理 ${res.affected} 封邮件` }) },
    onError: (error) => toast({ title: "清理失败", description: error.message }),
  })
  const applyMailbox = useMutation({
    mutationFn: api.applyMailbox,
    onSuccess: (mailbox) => {
      qc.invalidateQueries({ queryKey: ["mailboxes", "mine"] })
      qc.invalidateQueries({ queryKey: ["mailbox-apply-options"] })
      setMailboxId(mailbox.id)
      toast({ title: "邮箱已申请" })
    },
    onError: (error) => toast({ title: "申请失败", description: error.message }),
  })

  React.useEffect(() => {
    if (!mailboxes.isSuccess) return
    const items = mailboxes.data?.items || []
    if (items.length === 0) {
      if (mailboxId) setMailboxId("")
      localStorage.removeItem("lanqin:selected-mailbox")
      return
    }
    if (!mailboxId || !items.some((m) => m.id === mailboxId)) setMailboxId(items[0].id)
  }, [mailboxId, mailboxes.isSuccess, mailboxes.data?.items])
  React.useEffect(() => { if (mailboxId) localStorage.setItem("lanqin:selected-mailbox", mailboxId); else localStorage.removeItem("lanqin:selected-mailbox") }, [mailboxId])
  React.useEffect(() => { applyTheme(darkMode, themeMountedRef.current); themeMountedRef.current = true }, [darkMode])

  const logout = useLogout()
  async function copy(text: string) { await navigator.clipboard.writeText(text); toast({ title: "已复制" }) }
  function setTab(next: Tab) { setParams(next === "profile" ? {} : { tab: next }); setMobileSidebarOpen(false) }
  function toggleSidebar() { sidebarCollapsed ? (sidebarPanelRef.current?.expand(14), setSidebarCollapsed(false)) : (sidebarPanelRef.current?.collapse(), setSidebarCollapsed(true)) }
  if (me.isLoading) return <div className="grid h-svh place-items-center text-muted-foreground">加载中...</div>
  if (me.isError || !user) return <div className="grid h-svh place-items-center text-muted-foreground">登录状态已失效</div>

  const sidebarContent = (
    <Sidebar collapsible="none" className="h-full w-full border-r bg-sidebar">
      <SidebarHeader className={cn("border-b py-4", sidebarCollapsed ? "px-2" : "px-4")}>
        <AccountHeader collapsed={sidebarCollapsed} name={user.displayName || selectedMailbox?.address || "LanQin"} email={user.email || selectedMailbox?.address} darkMode={darkMode} onToggleTheme={() => setDarkMode((v) => !v)} onBack={() => navigate("/")} />
      </SidebarHeader>
      <SidebarContent>
        <SidebarGroup>
          {!sidebarCollapsed && <SidebarGroupLabel>个人中心</SidebarGroupLabel>}
          <SidebarGroupContent>
            <SidebarMenu>{tabKeys.map((key) => <SidebarMenuItem key={key}><SidebarMenuButton isActive={tab === key} className={cn(sidebarCollapsed && "justify-center px-0")} onClick={() => setTab(key)}>{tabs[key].icon}{!sidebarCollapsed && <span>{tabs[key].label}</span>}</SidebarMenuButton></SidebarMenuItem>)}</SidebarMenu>
          </SidebarGroupContent>
        </SidebarGroup>
      </SidebarContent>
      <div className={cn("mt-auto border-t p-2", sidebarCollapsed ? "flex flex-col items-center" : "")}>
        <Button type="button" variant="ghost" size={sidebarCollapsed ? "icon" : "sm"} className={cn("text-muted-foreground", !sidebarCollapsed && "w-full justify-start")} onClick={logout}>
          <LogOut className="h-4 w-4" />
          {!sidebarCollapsed && <span>退出登录</span>}
        </Button>
        {!isMobile && (
          <>
            <Separator className="my-2" />
            <Button type="button" variant="ghost" size={sidebarCollapsed ? "icon" : "sm"} className={cn(!sidebarCollapsed && "w-full justify-start")} onClick={toggleSidebar}>{sidebarCollapsed ? <PanelLeftOpen className="h-4 w-4" /> : <PanelLeftClose className="h-4 w-4" />}{!sidebarCollapsed && <span>收起侧栏</span>}</Button>
          </>
        )}
      </div>
    </Sidebar>
  )

  return (
    <div className="h-svh overflow-hidden bg-background">
      <SidebarProvider className="h-full min-h-0 w-full">
        {isMobile ? (
          <div className="flex h-full min-h-0 flex-col">
            <header className="flex h-14 shrink-0 items-center gap-2 border-b px-3">
              <Sheet open={mobileSidebarOpen} onOpenChange={setMobileSidebarOpen}>
                <SheetTrigger asChild>
                  <Button size="icon" variant="ghost" aria-label="打开导航"><PanelLeftOpen className="h-4 w-4" /></Button>
                </SheetTrigger>
                <SheetContent side="left" className="w-[86vw] max-w-80 p-0 [&>button]:hidden" aria-describedby={undefined}>
                  <SheetTitle className="sr-only">个人中心导航</SheetTitle>
                  <div className="h-svh">{sidebarContent}</div>
                </SheetContent>
              </Sheet>
              <div className="min-w-0 flex-1 text-sm font-semibold">{tabs[tab].label}</div>
              <Button type="button" variant="ghost" size="icon" onClick={() => navigate("/")} aria-label="返回邮箱"><ArrowLeft className="h-4 w-4" /></Button>
            </header>
            <ScrollArea className="min-h-0 flex-1"><main className="w-full p-4">{renderTab()}</main></ScrollArea>
          </div>
        ) : (
          <ResizablePanelGroup direction="horizontal" className="h-full min-h-0 w-full">
            <ResizablePanel ref={sidebarPanelRef} collapsible collapsedSize={4} defaultSize={15} minSize={11} maxSize={24} onCollapse={() => setSidebarCollapsed(true)} onExpand={() => setSidebarCollapsed(false)}>
              {sidebarContent}
            </ResizablePanel>
            <ResizableHandle withHandle />
            <ResizablePanel defaultSize={85} minSize={60}>
              <section className="flex h-full min-h-0 flex-col">
                <header className="flex h-16 shrink-0 items-center justify-between gap-3 border-b px-5">
                  <div className="text-sm font-semibold">{tabs[tab].label}</div>
                </header>
                <ScrollArea className="min-h-0 flex-1"><main className="mx-auto w-full max-w-6xl p-6">{renderTab()}</main></ScrollArea>
              </section>
            </ResizablePanel>
          </ResizablePanelGroup>
        )}
      </SidebarProvider>
    </div>
  )
  function renderTab() {
    if (tab === "mailboxes") return <MailboxManagement mailboxes={mailboxes.data?.items || []} applyOptions={mailboxApplyOptions.data} applyPending={applyMailbox.isPending} selectedMailboxId={mailboxId} onSelect={setMailboxId} onCopy={copy} onOpen={(id) => { setMailboxId(id); navigate("/") }} onApply={(payload) => applyMailbox.mutateAsync(payload).then(() => undefined)} />
    if (tab === "clients") return <ClientSettingsSection mailboxes={mailboxes.data?.items || []} selectedMailboxId={mailboxId} hostname={publicSettings.data?.publicHostname} onSelectMailbox={setMailboxId} onCopy={copy} />
    if (tab === "signatures") return <SignaturesSection items={signatures.data?.items || []} mailboxes={mailboxes.data?.items || []} loading={signatures.isLoading} pending={createSignature.isPending || updateSignature.isPending || setDefaultSignature.isPending || deleteSignature.isPending} onCreate={(form) => createSignature.mutate(form)} onUpdate={(id, form) => updateSignature.mutate({ id, form })} onSetDefault={(id) => setDefaultSignature.mutate(id)} onDelete={(id) => deleteSignature.mutate(id)} />
    if (tab === "contacts") return <ContactsSection items={contacts.data?.items || []} loading={contacts.isLoading} pending={createContact.isPending} onCreate={(form) => createContact.mutate(form)} onDelete={(id) => deleteContact.mutate(id)} onCopy={copy} />
    if (tab === "cleanup") return <CleanupSection mailbox={selectedMailbox} stats={stats.data} pending={cleanup.isPending} onCleanup={(target) => cleanup.mutate(target)} />
    if (tab === "rules") return <RulesSection items={rules.data?.items || []} mailboxes={mailboxes.data?.items || []} labels={ruleLabels.data?.items || []} open={ruleDialogOpen} onOpenChange={setRuleDialogOpen} onCreate={(payload) => createRule.mutate(payload)} onDelete={(id) => deleteRule.mutate(id)} pending={createRule.isPending} />
    if (tab === "blocked") return <BlockedSection items={blocked.data?.items || []} mailboxes={mailboxes.data?.items || []} mailboxId={blockedMailboxId} spamCount={stats.data?.byFolder.find((f) => f.role === "spam")?.count || 0} onMailboxChange={setBlockedMailboxId} onCreate={(form) => createBlocked.mutate(form)} onDelete={(id) => deleteBlocked.mutate(id)} pending={createBlocked.isPending} />
    if (tab === "stats") return <StatsSection stats={stats.data} mailbox={selectedMailbox} onRefresh={() => stats.refetch()} />
    return <ProfileOverview user={user!} profile={profile} password={password} passwordFormRef={passwordFormRef} stats={stats.data} displayMode={displayMode} onDisplayModeChange={setDisplayMode} twoFactorFormRef={twoFactorFormRef} setupTwoFactor={setupTwoFactor} enableTwoFactor={enableTwoFactor} disableTwoFactor={disableTwoFactor} onCopy={copy} />
  }
}

function ProfileOverview({ user, profile, password, passwordFormRef, stats, displayMode, onDisplayModeChange, twoFactorFormRef, setupTwoFactor, enableTwoFactor, disableTwoFactor, onCopy }: { user: { email: string; displayName: string; role: string; disabled: boolean; twoFactorEnabled: boolean; createdAt: string }; profile: { mutate: (form: FormData) => void; isPending: boolean }; password: { mutate: (form: FormData) => void; isPending: boolean }; passwordFormRef: React.RefObject<HTMLFormElement>; stats?: MailStats; displayMode: DisplayMode; onDisplayModeChange: (mode: DisplayMode) => void; twoFactorFormRef: React.RefObject<HTMLFormElement>; setupTwoFactor: { data?: { secret: string; otpauthUrl: string }; mutate: () => void; reset: () => void; isPending: boolean }; enableTwoFactor: { mutate: (form: FormData) => void; isPending: boolean }; disableTwoFactor: { mutate: (form: FormData) => void; isPending: boolean }; onCopy: (text: string) => void }) {
  return (
    <div className="space-y-6">
      <Card>
        <CardHeader>
          <CardTitle>账户信息</CardTitle>
        </CardHeader>
        <CardContent className="space-y-6">
          <form className="space-y-4" onSubmit={(e) => { e.preventDefault(); profile.mutate(new FormData(e.currentTarget)) }}>
            <div className="grid gap-4 md:grid-cols-2">
              <Field label="用户名">
                <Input value={user.email} readOnly />
              </Field>
              <Field label="显示名称">
                <Input name="displayName" defaultValue={user.displayName} required />
              </Field>
            </div>
            <div className="flex justify-end">
              <Button disabled={profile.isPending}>{profile.isPending ? "保存中..." : "保存资料"}</Button>
            </div>
          </form>

          <Separator />

          <div className="space-y-3">
            <div className="flex items-center justify-between rounded-lg border p-3">
              <div className="flex items-center gap-2 text-sm">
                <ShieldCheck className="h-4 w-4" />
                角色
              </div>
              <Badge>{user.role === "admin" ? "管理员" : "普通用户"}</Badge>
            </div>
            <div className="flex items-center justify-between rounded-lg border p-3 text-sm">
              <span>账号状态</span>
              <Badge variant={user.disabled ? "secondary" : "default"}>{user.disabled ? "已停用" : "正常"}</Badge>
            </div>
            <div className="flex items-center justify-between rounded-lg border p-3 text-sm">
              <span>创建时间</span>
              <span>{new Date(user.createdAt).toLocaleString()}</span>
            </div>
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>界面设置</CardTitle>
        </CardHeader>
        <CardContent>
          <Field label="显示模式">
            <Select value={displayMode} onValueChange={(value) => onDisplayModeChange(value as DisplayMode)}>
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="detailed">详细</SelectItem>
                <SelectItem value="compact">简洁</SelectItem>
              </SelectContent>
            </Select>
          </Field>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>双因素认证</CardTitle>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex items-center justify-between rounded-lg border p-3">
            <div className="flex items-center gap-2 text-sm">
              <KeyRound className="h-4 w-4" />
              认证状态
            </div>
            <Badge variant={user.twoFactorEnabled ? "default" : "secondary"}>{user.twoFactorEnabled ? "已启用" : "未启用"}</Badge>
          </div>

          {!user.twoFactorEnabled && !setupTwoFactor.data && (
            <Button onClick={() => setupTwoFactor.mutate()} disabled={setupTwoFactor.isPending}>{setupTwoFactor.isPending ? "生成中..." : "启用双因素认证"}</Button>
          )}

          {!user.twoFactorEnabled && setupTwoFactor.data && (
            <form ref={twoFactorFormRef} className="space-y-4" onSubmit={(e) => { e.preventDefault(); enableTwoFactor.mutate(new FormData(e.currentTarget)) }}>
              <div className="grid gap-4 lg:grid-cols-[220px_minmax(0,1fr)]">
                <div className="flex justify-center rounded-lg border bg-white p-4">
                  <QRCodeSVG value={setupTwoFactor.data.otpauthUrl} size={184} level="M" />
                </div>
                <div className="space-y-4">
                  <Field label="密钥">
                    <div className="flex gap-2">
                      <Input value={setupTwoFactor.data.secret} readOnly />
                      <Button type="button" variant="outline" onClick={() => onCopy(setupTwoFactor.data!.secret)}><Copy className="h-4 w-4" />复制</Button>
                    </div>
                  </Field>
                  <Field label="绑定地址">
                    <div className="flex gap-2">
                      <Input value={setupTwoFactor.data.otpauthUrl} readOnly />
                      <Button type="button" variant="outline" onClick={() => onCopy(setupTwoFactor.data!.otpauthUrl)}><Copy className="h-4 w-4" />复制</Button>
                    </div>
                  </Field>
                </div>
              </div>
              <Field label="验证码">
                <Input name="code" inputMode="numeric" autoComplete="one-time-code" minLength={6} maxLength={6} required />
              </Field>
              <div className="flex justify-end gap-2">
                <Button type="button" variant="outline" onClick={() => setupTwoFactor.reset()}>取消</Button>
                <Button disabled={enableTwoFactor.isPending}>{enableTwoFactor.isPending ? "启用中..." : "确认启用"}</Button>
              </div>
            </form>
          )}

          {user.twoFactorEnabled && (
            <form ref={twoFactorFormRef} className="space-y-4" onSubmit={(e) => { e.preventDefault(); disableTwoFactor.mutate(new FormData(e.currentTarget)) }}>
              <Field label="当前验证码">
                <Input name="code" inputMode="numeric" autoComplete="one-time-code" minLength={6} maxLength={6} required />
              </Field>
              <div className="flex justify-end">
                <Button variant="destructive" disabled={disableTwoFactor.isPending}>{disableTwoFactor.isPending ? "关闭中..." : "关闭双因素认证"}</Button>
              </div>
            </form>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>修改密码</CardTitle>
        </CardHeader>
        <CardContent>
          <form ref={passwordFormRef} className="space-y-4" onSubmit={(e) => { e.preventDefault(); password.mutate(new FormData(e.currentTarget)) }}>
            <Field label="当前密码">
              <PasswordInput name="currentPassword" required />
            </Field>
            <div className="grid gap-4 md:grid-cols-2">
              <Field label="新密码">
                <PasswordInput name="newPassword" minLength={8} required />
              </Field>
              <Field label="确认新密码">
                <PasswordInput name="confirmPassword" minLength={8} required />
              </Field>
            </div>
            <div className="flex justify-end">
              <Button disabled={password.isPending}>{password.isPending ? "更新中..." : "更新密码"}</Button>
            </div>
          </form>
        </CardContent>
      </Card>

      <StatsSummary stats={stats} />
    </div>
  )
}

function MailboxManagement({ mailboxes, applyOptions, applyPending, selectedMailboxId, onSelect, onCopy, onOpen, onApply }: { mailboxes: Mailbox[]; applyOptions?: MailboxApplyOptions; applyPending: boolean; selectedMailboxId: string; onSelect: (id: string) => void; onCopy: (text: string) => void; onOpen: (id: string) => void; onApply: (payload: { domainId: string; localPart: string; displayName: string }) => Promise<void> }) {
  const canApply = !!applyOptions?.enabled && (applyOptions.domains || []).length > 0
  return (
    <div className="space-y-4">
      <div className="flex justify-end">
        {canApply && <ApplyMailboxDialog options={applyOptions} pending={applyPending} onApply={onApply} />}
      </div>
      <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
        {mailboxes.map((m) => <Card key={m.id} className={cn(selectedMailboxId === m.id && "border-primary")}><CardHeader><div className="flex items-start justify-between gap-3"><div className="min-w-0"><CardTitle className="truncate text-base">{m.address}</CardTitle></div>{selectedMailboxId === m.id && <Badge>当前</Badge>}</div></CardHeader><CardContent className="flex flex-wrap gap-2"><Button variant="outline" size="sm" onClick={() => onSelect(m.id)}>设为当前</Button><Button variant="outline" size="sm" onClick={() => onCopy(m.address)}><Copy className="h-4 w-4" />复制</Button><Button size="sm" onClick={() => onOpen(m.id)}>进入邮箱</Button></CardContent></Card>)}
        {mailboxes.length === 0 && <EmptyState text={canApply ? "暂无邮箱账号，点击申请邮箱创建" : "暂无邮箱账号"} />}
      </div>
    </div>
  )
}

function ApplyMailboxDialog({ options, pending, onApply }: { options: MailboxApplyOptions; pending: boolean; onApply: (payload: { domainId: string; localPart: string; displayName: string }) => Promise<void> }) {
  const [open, setOpen] = React.useState(false)
  const [domainId, setDomainId] = React.useState(options.domains[0]?.id || "")
  React.useEffect(() => {
    if (!open) return
    setDomainId((current) => options.domains.some((domain) => domain.id === current) ? current : options.domains[0]?.id || "")
  }, [open, options.domains])

  async function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const form = new FormData(event.currentTarget)
    try {
      await onApply({
        domainId,
        localPart: String(form.get("localPart") || ""),
        displayName: String(form.get("displayName") || ""),
      })
      event.currentTarget.reset()
      setOpen(false)
    } catch {}
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <Button type="button" onClick={() => setOpen(true)}><Plus className="h-4 w-4" />申请邮箱</Button>
      <DialogContent className="max-h-[92dvh] overflow-y-auto sm:max-w-lg">
        <DialogHeader><DialogTitle>申请邮箱</DialogTitle></DialogHeader>
        <form className="space-y-4" onSubmit={submit}>
          <Field label="邮箱前缀"><Input name="localPart" autoFocus required placeholder="your-name" /></Field>
          <Field label="域名后缀">
            <Select value={domainId} onValueChange={setDomainId}>
              <SelectTrigger><SelectValue placeholder="选择域名" /></SelectTrigger>
              <SelectContent>{options.domains.map((domain) => <SelectItem key={domain.id} value={domain.id}>@{domain.name}</SelectItem>)}</SelectContent>
            </Select>
          </Field>
          <Field label="显示名称"><Input name="displayName" placeholder="可选" /></Field>
          <DialogFooter className="gap-2 [&>button]:w-full sm:[&>button]:w-auto">
            <Button type="button" variant="outline" onClick={() => setOpen(false)}>取消</Button>
            <Button disabled={pending || !domainId}>{pending ? "申请中..." : "申请"}</Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function ClientSettingsSection({ mailboxes, selectedMailboxId, hostname, onSelectMailbox, onCopy }: { mailboxes: Mailbox[]; selectedMailboxId: string; hostname?: string; onSelectMailbox: (id: string) => void; onCopy: (text: string) => void }) {
  const selected = mailboxes.find((item) => item.id === selectedMailboxId) || mailboxes[0]
  const server = clientServerHost(hostname, selected?.address)
  const rows = [
    { label: "IMAP 服务器", value: `${server}:993`, security: "SSL" },
    { label: "POP3 服务器", value: `${server}:995`, security: "SSL" },
    { label: "SMTP 服务器", value: `${server}:465`, security: "SSL" },
  ]
  return (
    <div className="space-y-6">
      <Card>
        <CardHeader>
          <div className="flex items-start justify-between gap-4">
            <div>
              <CardTitle>第三方客户端</CardTitle>
              <div className="mt-1 text-sm text-muted-foreground">IMAP / POP3 / SMTP 配置用于 Thunderbird、Apple Mail、手机邮件客户端等。</div>
            </div>
            {!!selected && <Badge variant="secondary">{selected.address}</Badge>}
          </div>
        </CardHeader>
        <CardContent className="space-y-5">
          <Field label="选择邮箱">
            <Select value={selected?.id || ""} onValueChange={onSelectMailbox}>
              <SelectTrigger><SelectValue placeholder="选择邮箱" /></SelectTrigger>
              <SelectContent>{mailboxes.map((mailbox) => <SelectItem key={mailbox.id} value={mailbox.id}>{mailbox.address}</SelectItem>)}</SelectContent>
            </Select>
          </Field>

          {selected ? (
            <>
              <div className="rounded-lg border p-4">
                <div className="flex flex-wrap items-center justify-between gap-3">
                  <div className="min-w-0">
                    <div className="truncate font-medium">{selected.address}</div>
                    <div className="mt-3 flex flex-wrap gap-2">
                      <Badge variant="secondary" className="bg-emerald-100 text-emerald-700">● IMAP</Badge>
                      <Badge variant="secondary" className="bg-emerald-100 text-emerald-700">● POP3</Badge>
                      <Badge variant="secondary" className="bg-emerald-100 text-emerald-700">● SMTP</Badge>
                    </div>
                  </div>
                  <Badge variant="outline">已启用</Badge>
                </div>
              </div>

              <div className="rounded-lg bg-muted p-5">
                <div className="mb-4 font-medium">客户端配置</div>
                <div className="space-y-3">
                  {rows.map((row) => (
                    <ClientConfigRow key={row.label} label={row.label} value={row.value} security={row.security} onCopy={onCopy} />
                  ))}
                </div>
                <Separator className="my-4" />
                <div className="grid gap-3 text-sm sm:grid-cols-[120px_minmax(0,1fr)]">
                  <div className="text-muted-foreground">用户名</div>
                  <div className="flex min-w-0 items-center justify-between gap-2">
                    <span className="truncate text-right sm:text-left">{selected.address}</span>
                    <Button type="button" variant="ghost" size="icon" className="size-7" onClick={() => onCopy(selected.address)}><Copy className="h-4 w-4" /></Button>
                  </div>
                  <div className="text-muted-foreground">密码</div>
                  <div>邮箱登录密码</div>
                </div>
              </div>
            </>
          ) : (
            <EmptyState text="暂无邮箱账号，创建邮箱后可查看客户端配置" />
          )}
        </CardContent>
      </Card>
    </div>
  )
}

function ClientConfigRow({ label, value, security, onCopy }: { label: string; value: string; security: string; onCopy: (text: string) => void }) {
  return (
    <div className="grid items-center gap-2 text-sm sm:grid-cols-[120px_minmax(0,1fr)]">
      <div className="text-muted-foreground">{label}</div>
      <div className="flex min-w-0 items-center justify-between gap-2">
        <code className="truncate rounded border bg-background px-2 py-1 text-xs">{value}</code>
        <div className="flex shrink-0 items-center gap-1">
          <span className="text-xs font-medium text-emerald-600">{security}</span>
          <Button type="button" variant="ghost" size="icon" className="size-7" onClick={() => onCopy(value)}><Copy className="h-4 w-4" /></Button>
        </div>
      </div>
    </div>
  )
}

function SignaturesSection({ items, mailboxes, loading, pending, onCreate, onUpdate, onSetDefault, onDelete }: { items: MailSignature[]; mailboxes: Mailbox[]; loading: boolean; pending: boolean; onCreate: (form: FormData) => void; onUpdate: (id: string, form: FormData) => void; onSetDefault: (id: string) => void; onDelete: (id: string) => void }) {
  const [mailboxId, setMailboxId] = React.useState("all")
  const [isDefault, setIsDefault] = React.useState(false)
  const [editing, setEditing] = React.useState<MailSignature | null>(null)
  const [pendingConfirm, setPendingConfirm] = React.useState<PendingConfirm | null>(null)
  const editingMailboxId = editing?.mailboxId || "all"
  const editingIsDefault = editing?.isDefault || false
  function resetCreateForm(form: HTMLFormElement) {
    form.reset()
    setMailboxId("all")
    setIsDefault(false)
  }
  return (
    <div className="space-y-6">
      <Card>
        <CardHeader>
          <div className="flex items-start justify-between gap-4">
            <div>
              <CardTitle>签名管理</CardTitle>
              <div className="mt-1 text-sm text-muted-foreground">支持全局签名和按发件邮箱绑定的默认签名。</div>
            </div>
            <div className="text-sm text-muted-foreground">共 {items.length} 个签名</div>
          </div>
        </CardHeader>
        <CardContent>
          <form className="space-y-4 rounded-lg border p-4" onSubmit={(e) => { e.preventDefault(); const form = new FormData(e.currentTarget); form.set("mailboxId", mailboxId === "all" ? "" : mailboxId); form.set("isDefault", isDefault ? "on" : ""); onCreate(form); resetCreateForm(e.currentTarget) }}>
            <div className="grid gap-4 md:grid-cols-2">
              <Field label="签名名称"><Input name="name" required placeholder="例如：默认签名" /></Field>
              <Field label="绑定邮箱">
                <MailboxSelect value={mailboxId} mailboxes={mailboxes} onChange={setMailboxId} />
              </Field>
            </div>
            <Field label="签名内容">
              <Textarea name="content" required className="min-h-40" placeholder="支持多行文本，写信时会自动转为 HTML" />
            </Field>
            <label className="flex items-center gap-3 text-sm font-medium">
              <Checkbox checked={isDefault} onCheckedChange={(value) => setIsDefault(value === true)} />
              <span>设为当前范围默认签名</span>
            </label>
            <Button disabled={pending}>{pending ? "保存中..." : "创建签名"}</Button>
          </form>
        </CardContent>
      </Card>

      <Card>
        <CardHeader><CardTitle>签名列表</CardTitle></CardHeader>
        <CardContent className="space-y-3">
          {items.map((item) => {
            const mailbox = item.mailboxId ? mailboxes.find((m) => m.id === item.mailboxId)?.address || "未知邮箱" : "全局签名"
            return (
              <div key={item.id} className="rounded-lg border p-4">
                <div className="flex flex-wrap items-start justify-between gap-3">
                  <div className="min-w-0">
                    <div className="flex flex-wrap items-center gap-2">
                      <div className="font-medium">{item.name}</div>
                      {item.isDefault && <Badge>默认</Badge>}
                      <Badge variant="outline">{mailbox}</Badge>
                    </div>
                    <div className="mt-2 whitespace-pre-wrap text-sm text-muted-foreground">{item.content}</div>
                  </div>
                  <div className="flex shrink-0 gap-1">
                    {!item.isDefault && <Button variant="outline" size="sm" disabled={pending} onClick={() => onSetDefault(item.id)}>设为默认</Button>}
                    <Button variant="ghost" size="icon" className="size-8" disabled={pending} onClick={() => setEditing(item)}><PencilLine className="h-4 w-4" /></Button>
                    <Button variant="ghost" size="icon" className="size-8 text-destructive" disabled={pending} onClick={() => setPendingConfirm({ title: "删除签名？", description: `签名“${item.name}”将被删除。`, confirmText: "删除签名", onConfirm: () => { onDelete(item.id); setPendingConfirm(null) } })}><Trash2 className="h-4 w-4" /></Button>
                  </div>
                </div>
              </div>
            )
          })}
          {!loading && items.length === 0 && <EmptyState text="暂无签名" />}
        </CardContent>
      </Card>
      <Dialog open={!!editing} onOpenChange={(open) => { if (!open) setEditing(null) }}>
        <DialogContent className="max-h-[92dvh] overflow-y-auto sm:max-w-2xl">
          <DialogHeader><DialogTitle>编辑签名</DialogTitle></DialogHeader>
          {editing && (
            <form key={editing.id} className="space-y-4" onSubmit={(e) => { e.preventDefault(); const form = new FormData(e.currentTarget); form.set("mailboxId", editingMailboxId === "all" ? "" : editingMailboxId); form.set("isDefault", editingIsDefault ? "on" : ""); onUpdate(editing.id, form); setEditing(null) }}>
              <div className="grid gap-4 md:grid-cols-2">
                <Field label="签名名称"><Input name="name" defaultValue={editing.name} required /></Field>
                <Field label="绑定邮箱">
                  <MailboxSelect value={editingMailboxId} mailboxes={mailboxes} onChange={(value) => setEditing((current) => current ? { ...current, mailboxId: value === "all" ? "" : value } : current)} />
                </Field>
              </div>
              <Field label="签名内容">
                <Textarea name="content" required className="min-h-44" defaultValue={editing.content} />
              </Field>
              <label className="flex items-center gap-3 text-sm font-medium">
                <Checkbox checked={editingIsDefault} onCheckedChange={(value) => setEditing((current) => current ? { ...current, isDefault: value === true } : current)} />
                <span>设为当前范围默认签名</span>
              </label>
              <DialogFooter className="gap-2 [&>button]:w-full sm:[&>button]:w-auto">
                <Button type="button" variant="outline" onClick={() => setEditing(null)}>取消</Button>
                <Button disabled={pending}>{pending ? "保存中..." : "保存修改"}</Button>
              </DialogFooter>
            </form>
          )}
        </DialogContent>
      </Dialog>
      <ConfirmDialog open={!!pendingConfirm} title={pendingConfirm?.title || ""} description={pendingConfirm?.description} confirmText={pendingConfirm?.confirmText || "删除"} destructive pending={pending} onOpenChange={(open) => { if (!open) setPendingConfirm(null) }} onConfirm={() => pendingConfirm?.onConfirm()} />
    </div>
  )
}

function ContactsSection({ items, loading, onCreate, onDelete, onCopy, pending }: { items: { id: string; name: string; email: string; note: string }[]; loading: boolean; onCreate: (form: FormData) => void; onDelete: (id: string) => void; onCopy: (text: string) => void; pending: boolean }) {
  const [pendingConfirm, setPendingConfirm] = React.useState<PendingConfirm | null>(null)
  return (
    <div className="grid gap-6 lg:grid-cols-[380px_minmax(0,1fr)]">
      <Card>
        <CardHeader><CardTitle>新增联系人</CardTitle></CardHeader>
        <CardContent>
          <form className="space-y-4" onSubmit={(e) => { e.preventDefault(); onCreate(new FormData(e.currentTarget)); e.currentTarget.reset() }}>
            <Field label="姓名"><Input name="name" placeholder="张三" /></Field>
            <Field label="邮箱"><Input name="email" type="email" required /></Field>
            <Field label="备注"><Input name="note" /></Field>
            <Button className="w-full" disabled={pending}>{pending ? "保存中..." : "保存联系人"}</Button>
          </form>
        </CardContent>
      </Card>
      <Card>
        <CardHeader><CardTitle>联系人列表</CardTitle></CardHeader>
        <CardContent className="space-y-2">
          {items.map((item) => (
            <div key={item.id} className="flex items-center justify-between gap-3 rounded-lg border p-3">
              <div className="min-w-0">
                <div className="truncate text-sm font-medium">{item.name}</div>
                <div className="truncate text-xs text-muted-foreground">{item.email}{item.note ? ` · ${item.note}` : ""}</div>
              </div>
              <div className="flex shrink-0 gap-1">
                <Button variant="ghost" size="icon" className="size-8" onClick={() => onCopy(item.email)}><Copy className="h-4 w-4" /></Button>
                <Button variant="ghost" size="icon" className="size-8 text-destructive" onClick={() => setPendingConfirm({ title: "删除联系人？", description: `${item.email} 将从联系人列表中移除。`, confirmText: "删除联系人", onConfirm: () => { onDelete(item.id); setPendingConfirm(null) } })}><Trash2 className="h-4 w-4" /></Button>
              </div>
            </div>
          ))}
          {!loading && items.length === 0 && <EmptyState text="暂无联系人" />}
        </CardContent>
      </Card>
      <ConfirmDialog open={!!pendingConfirm} title={pendingConfirm?.title || ""} description={pendingConfirm?.description} confirmText={pendingConfirm?.confirmText || "删除"} destructive onOpenChange={(open) => { if (!open) setPendingConfirm(null) }} onConfirm={() => pendingConfirm?.onConfirm()} />
    </div>
  )
}

function CleanupSection({ mailbox, stats, pending, onCleanup }: { mailbox?: Mailbox; stats?: MailStats; pending: boolean; onCleanup: (target: "empty-trash" | "empty-spam" | "archive-read-inbox") => void }) {
  const [pendingConfirm, setPendingConfirm] = React.useState<PendingConfirm | null>(null)
  function confirmCleanup(target: "empty-trash" | "empty-spam" | "archive-read-inbox", title: string, destructive = false) {
    setPendingConfirm({
      title,
      description: mailbox ? `将对 ${mailbox.address} 执行此清理操作。` : "请先选择邮箱。",
      confirmText: destructive ? "确认清空" : "确认处理",
      destructive,
      onConfirm: () => { onCleanup(target); setPendingConfirm(null) },
    })
  }
  return (
    <div className="space-y-6">
      <StatsSummary stats={stats} />
      <Card>
        <CardHeader><CardTitle>清理当前邮箱</CardTitle></CardHeader>
        <CardContent className="grid gap-3 md:grid-cols-3">
          <CleanupButton icon={<MailCheck className="h-4 w-4" />} title="归档已读收件箱" disabled={!mailbox || pending} onClick={() => confirmCleanup("archive-read-inbox", "归档已读收件箱？")} />
          <CleanupButton icon={<MailX className="h-4 w-4" />} title="清空垃圾邮件" disabled={!mailbox || pending} onClick={() => confirmCleanup("empty-spam", "清空垃圾邮件？", true)} />
          <CleanupButton icon={<Trash2 className="h-4 w-4" />} title="清空回收站" disabled={!mailbox || pending} onClick={() => confirmCleanup("empty-trash", "清空回收站？", true)} />
        </CardContent>
      </Card>
      <ConfirmDialog open={!!pendingConfirm} title={pendingConfirm?.title || ""} description={pendingConfirm?.description} confirmText={pendingConfirm?.confirmText || "确认"} destructive={!!pendingConfirm?.destructive} pending={pending} onOpenChange={(open) => { if (!open) setPendingConfirm(null) }} onConfirm={() => pendingConfirm?.onConfirm()} />
    </div>
  )
}

type RuleCreatePayload = {
  mailboxId: string
  name: string
  matchMode: "all" | "any"
  conditions: MailRuleCondition[]
  actions: MailRuleAction[]
  applyToExisting: boolean
  stopProcessing: boolean
  enabled: boolean
}

const conditionFieldLabels: Record<MailRuleCondition["field"], string> = { from: "发件人地址", to: "收件人地址", subject: "邮件主题", body: "邮件正文" }
const conditionOperatorLabels: Record<MailRuleCondition["operator"], string> = { contains: "包含", "not-contains": "不包含", equals: "等于", "not-equals": "不等于", "starts-with": "开头是", "ends-with": "结尾是" }
const ruleActionLabels: Record<MailRuleAction["type"], string> = { archive: "移入归档", trash: "移入回收站", star: "添加星标", "mark-read": "标记已读", label: "添加标签", move: "移动到" }

function RulesSection({ items, mailboxes, labels, open, onOpenChange, onCreate, onDelete, pending }: { items: MailRule[]; mailboxes: Mailbox[]; labels: MailLabel[]; open: boolean; onOpenChange: (open: boolean) => void; onCreate: (payload: RuleCreatePayload) => void; onDelete: (id: string) => void; pending: boolean }) {
  return (
    <div className="space-y-4">
      <div className="flex justify-end">
        <Button onClick={() => onOpenChange(true)}><Plus className="h-4 w-4" />新建规则</Button>
      </div>
      <Card>
        <CardHeader><CardTitle>规则列表</CardTitle></CardHeader>
        <CardContent className="space-y-2">
          {items.map((item) => <RuleListItem key={item.id} item={item} mailboxes={mailboxes} onDelete={onDelete} />)}
          {items.length === 0 && <EmptyState text="暂无收件规则" />}
        </CardContent>
      </Card>
      <RuleDialog open={open} onOpenChange={onOpenChange} mailboxes={mailboxes} labels={labels} pending={pending} onCreate={onCreate} />
    </div>
  )
}

function RuleDialog({ open, onOpenChange, mailboxes, labels, pending, onCreate }: { open: boolean; onOpenChange: (open: boolean) => void; mailboxes: Mailbox[]; labels: MailLabel[]; pending: boolean; onCreate: (payload: RuleCreatePayload) => void }) {
  const [name, setName] = React.useState("我的规则")
  const [mailboxId, setMailboxId] = React.useState("all")
  const [matchMode, setMatchMode] = React.useState<"all" | "any">("all")
  const [conditions, setConditions] = React.useState<MailRuleCondition[]>([{ field: "from", operator: "contains", value: "" }])
  const [actions, setActions] = React.useState<MailRuleAction[]>([{ type: "label", value: labels[0]?.name || "" }])
  const [enabled, setEnabled] = React.useState(true)
  const [applyToExisting, setApplyToExisting] = React.useState(false)
  const [stopProcessing, setStopProcessing] = React.useState(false)
  const selectedMailboxId = mailboxId === "all" ? "" : mailboxId
  const labelQuery = useQuery({ queryKey: ["labels", "rule-dialog", selectedMailboxId], queryFn: () => api.labels(selectedMailboxId), enabled: !!selectedMailboxId })
  const availableLabels = selectedMailboxId ? (labelQuery.data?.items || []) : labels

  React.useEffect(() => {
    if (!open) return
    setName("我的规则")
    setMailboxId("all")
    setMatchMode("all")
    setConditions([{ field: "from", operator: "contains", value: "" }])
    setActions([{ type: "label", value: labels[0]?.name || "" }])
    setEnabled(true)
    setApplyToExisting(false)
    setStopProcessing(false)
  }, [open, labels])

  function updateCondition(index: number, patch: Partial<MailRuleCondition>) {
    setConditions((items) => items.map((item, i) => i === index ? { ...item, ...patch } : item))
  }
  function updateAction(index: number, patch: Partial<MailRuleAction>) {
    setActions((items) => items.map((item, i) => i === index ? normalizeDraftAction({ ...item, ...patch }, availableLabels) : item))
  }
  function addCondition() { setConditions((items) => [...items, { field: "subject", operator: "contains", value: "" }]) }
  function addAction() { setActions((items) => [...items, { type: "star" }]) }
  function removeCondition(index: number) { setConditions((items) => items.length > 1 ? items.filter((_, i) => i !== index) : items) }
  function removeAction(index: number) { setActions((items) => items.length > 1 ? items.filter((_, i) => i !== index) : items) }

  const validConditions = conditions.map((item) => ({ ...item, value: item.value.trim() })).filter((item) => item.value)
  const validActions = actions.map((item) => normalizeDraftAction(item, availableLabels)).filter((item) => item.type !== "label" || item.value || item.labelId).filter((item) => item.type !== "move" || item.value)
  const canCreate = validConditions.length > 0 && validActions.length > 0 && !pending

  function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!canCreate) return
    onCreate({ mailboxId: selectedMailboxId, name: name.trim() || "我的规则", matchMode, conditions: validConditions, actions: validActions, applyToExisting, stopProcessing, enabled })
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="flex h-svh w-screen max-w-none gap-0 overflow-hidden p-0 sm:h-auto sm:max-h-[92vh] sm:w-[min(94vw,84rem)]">
        <DialogHeader className="border-b px-4 py-4 text-left sm:px-8 sm:py-6">
          <DialogTitle className="text-xl sm:text-2xl">新建规则</DialogTitle>
        </DialogHeader>
        <form className="flex min-h-0 flex-1 flex-col" onSubmit={submit}>
          <div className="min-h-0 flex-1 space-y-6 overflow-y-auto px-4 py-5 sm:space-y-7 sm:px-8 sm:py-7">
            <Field label="名称"><Input value={name} onChange={(event) => setName(event.target.value)} placeholder="我的规则" /></Field>
            <Field label="适用邮箱"><MailboxSelect value={mailboxId} mailboxes={mailboxes} onChange={setMailboxId} /></Field>

            <div className="space-y-4">
              <div className="flex flex-wrap items-center gap-3 text-sm">
                <span>当新邮件到达时，满足以下</span>
                <Select value={matchMode} onValueChange={(value) => setMatchMode(value as "all" | "any")}>
                  <SelectTrigger className="h-9 w-[132px]"><SelectValue /></SelectTrigger>
                  <SelectContent><SelectItem value="all">所有条件</SelectItem><SelectItem value="any">任一条件</SelectItem></SelectContent>
                </Select>
              </div>
              <div className="space-y-3">
                {conditions.map((condition, index) => (
                  <div key={index} className="grid gap-3 md:grid-cols-[220px_150px_minmax(0,1fr)_auto_auto]">
                    <Select value={condition.field} onValueChange={(value) => updateCondition(index, { field: value as MailRuleCondition["field"] })}>
                      <SelectTrigger><SelectValue /></SelectTrigger>
                      <SelectContent>{(Object.keys(conditionFieldLabels) as MailRuleCondition["field"][]).map((value) => <SelectItem key={value} value={value}>{conditionFieldLabels[value]}</SelectItem>)}</SelectContent>
                    </Select>
                    <Select value={condition.operator} onValueChange={(value) => updateCondition(index, { operator: value as MailRuleCondition["operator"] })}>
                      <SelectTrigger><SelectValue /></SelectTrigger>
                      <SelectContent>{(Object.keys(conditionOperatorLabels) as MailRuleCondition["operator"][]).map((value) => <SelectItem key={value} value={value}>{conditionOperatorLabels[value]}</SelectItem>)}</SelectContent>
                    </Select>
                    <Input value={condition.value} onChange={(event) => updateCondition(index, { value: event.target.value })} placeholder="输入值" />
                    <Button type="button" variant="ghost" size="icon" className="text-muted-foreground" onClick={() => removeCondition(index)} disabled={conditions.length === 1}><X className="h-4 w-4" /></Button>
                    <Button type="button" variant="ghost" size="icon" onClick={addCondition}><Plus className="h-4 w-4" /></Button>
                  </div>
                ))}
              </div>
            </div>

            <div className="space-y-4">
              <div className="text-sm">执行以下动作</div>
              <div className="space-y-3">
                {actions.map((action, index) => (
                  <div key={index} className="grid gap-3 md:grid-cols-[220px_minmax(0,1fr)_auto_auto]">
                    <Select value={action.type} onValueChange={(value) => updateAction(index, { type: value as MailRuleAction["type"], value: "", labelId: "" })}>
                      <SelectTrigger><SelectValue /></SelectTrigger>
                      <SelectContent>{(Object.keys(ruleActionLabels) as MailRuleAction["type"][]).map((value) => <SelectItem key={value} value={value}>{ruleActionLabels[value]}</SelectItem>)}</SelectContent>
                    </Select>
                    <RuleActionValue action={action} labels={availableLabels} onChange={(patch) => updateAction(index, patch)} />
                    <Button type="button" variant="ghost" size="icon" className="text-muted-foreground" onClick={() => removeAction(index)} disabled={actions.length === 1}><X className="h-4 w-4" /></Button>
                    <Button type="button" variant="ghost" size="icon" onClick={addAction}><Plus className="h-4 w-4" /></Button>
                  </div>
                ))}
              </div>
            </div>

            <Separator />

            <div className="space-y-4">
              <RuleCheckbox checked={enabled} onCheckedChange={setEnabled} label="立即启用" />
              <RuleCheckbox checked={applyToExisting} onCheckedChange={setApplyToExisting} label="应用于现有邮件" />
              <div className="flex items-center gap-2">
                <RuleCheckbox checked={stopProcessing} onCheckedChange={setStopProcessing} label="终止规则：命中此规则后不再应用其他规则" />
                <Info className="h-4 w-4 text-muted-foreground" />
              </div>
            </div>
          </div>
          <DialogFooter className="gap-2 border-t px-4 py-4 sm:px-8 sm:py-5 [&>button]:w-full sm:[&>button]:w-auto">
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>取消</Button>
            <Button disabled={!canCreate}>{pending ? "创建中..." : "创建"}</Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function RuleActionValue({ action, labels, onChange }: { action: MailRuleAction; labels: MailLabel[]; onChange: (patch: Partial<MailRuleAction>) => void }) {
  if (action.type === "label") {
    if (labels.length > 0) {
      return (
        <Select value={action.value || labels[0].name} onValueChange={(value) => onChange({ value, labelId: labels.find((item) => item.name === value)?.id || "" })}>
          <SelectTrigger><SelectValue placeholder="选择标签" /></SelectTrigger>
          <SelectContent>{labels.map((label) => <SelectItem key={label.id} value={label.name}>{label.name}</SelectItem>)}</SelectContent>
        </Select>
      )
    }
    return <Input value={action.value || ""} onChange={(event) => onChange({ value: event.target.value, labelId: "" })} placeholder="标签名称" />
  }
  if (action.type === "move") {
    return (
      <Select value={action.value || "Archive"} onValueChange={(value) => onChange({ value })}>
        <SelectTrigger><SelectValue /></SelectTrigger>
        <SelectContent><SelectItem value="Inbox">收件箱</SelectItem><SelectItem value="Archive">归档</SelectItem><SelectItem value="Spam">垃圾邮件</SelectItem><SelectItem value="Trash">回收站</SelectItem></SelectContent>
      </Select>
    )
  }
  return <Input value="无需填写" readOnly />
}

function RuleCheckbox({ checked, onCheckedChange, label }: { checked: boolean; onCheckedChange: (checked: boolean) => void; label: string }) {
  const id = React.useId()
  return <div className="flex items-center gap-3"><Checkbox id={id} checked={checked} onCheckedChange={(value) => onCheckedChange(value === true)} /><Label htmlFor={id} className="text-base font-medium">{label}</Label></div>
}

function RuleListItem({ item, mailboxes, onDelete }: { item: MailRule; mailboxes: Mailbox[]; onDelete: (id: string) => void }) {
  const mailbox = item.mailboxId ? mailboxes.find((m) => m.id === item.mailboxId)?.address : "全部邮箱"
  const [confirmOpen, setConfirmOpen] = React.useState(false)
  return (
    <div className="flex items-center justify-between gap-3 rounded-lg border p-3">
      <div className="min-w-0 space-y-1">
        <div className="flex min-w-0 flex-wrap items-center gap-2 text-sm font-medium">
          <span className="truncate">{item.name}</span>
          <Badge variant={item.enabled ? "default" : "secondary"}>{item.enabled ? "启用" : "停用"}</Badge>
          {item.actions.map((action, index) => <Badge key={`${action.type}-${index}`} variant="outline">{actionSummary(action)}</Badge>)}
        </div>
        <div className="truncate text-xs text-muted-foreground">{mailbox} · {item.matchMode === "any" ? "任一条件" : "所有条件"} · {conditionSummary(item.conditions, item.fromContains, item.subjectContains)}</div>
      </div>
      <Button variant="ghost" size="icon" className="size-8 shrink-0 text-destructive" onClick={() => setConfirmOpen(true)}><Trash2 className="h-4 w-4" /></Button>
      <ConfirmDialog open={confirmOpen} title="删除收件规则？" description={`规则“${item.name}”将不再处理后续邮件。`} confirmText="删除规则" destructive onOpenChange={setConfirmOpen} onConfirm={() => { onDelete(item.id); setConfirmOpen(false) }} />
    </div>
  )
}

function normalizeDraftAction(action: MailRuleAction, labels: MailLabel[]): MailRuleAction {
  if (action.type === "label") {
    const value = action.value || labels[0]?.name || ""
    return { type: "label", value, labelId: labels.find((label) => label.name === value)?.id || action.labelId || "" }
  }
  if (action.type === "move") return { type: "move", value: action.value || "Archive" }
  return { type: action.type }
}

function conditionSummary(conditions: MailRuleCondition[] = [], fromContains = "", subjectContains = "") {
  const items = conditions.length > 0 ? conditions : [fromContains ? { field: "from", operator: "contains", value: fromContains } as MailRuleCondition : undefined, subjectContains ? { field: "subject", operator: "contains", value: subjectContains } as MailRuleCondition : undefined].filter(Boolean) as MailRuleCondition[]
  return items.map((item) => `${conditionFieldLabels[item.field]} ${conditionOperatorLabels[item.operator]} ${item.value}`).join("；") || "无条件"
}

function actionSummary(action: MailRuleAction) {
  if (action.type === "label") return `${ruleActionLabels[action.type]}${action.value ? `：${action.value}` : ""}`
  if (action.type === "move") return `${ruleActionLabels[action.type]}：${folderLabel(action.value || "Archive")}`
  return ruleActionLabels[action.type]
}

function BlockedSection({ items, mailboxes, mailboxId, spamCount, onMailboxChange, onCreate, onDelete, pending }: { items: any[]; mailboxes: Mailbox[]; mailboxId: string; spamCount: number; onMailboxChange: (value: string) => void; onCreate: (form: FormData) => void; onDelete: (id: string) => void; pending: boolean }) {
  const [pendingConfirm, setPendingConfirm] = React.useState<PendingConfirm | null>(null)
  return (
    <div className="grid gap-6 lg:grid-cols-[420px_minmax(0,1fr)]">
      <Card>
        <CardHeader><CardTitle>新增拦截发件人</CardTitle></CardHeader>
        <CardContent>
          <form className="space-y-4" onSubmit={(e) => { e.preventDefault(); onCreate(new FormData(e.currentTarget)); e.currentTarget.reset() }}>
            <Field label="适用邮箱"><MailboxSelect value={mailboxId} mailboxes={mailboxes} onChange={onMailboxChange} /></Field>
            <Field label="发件人邮箱"><Input name="email" type="email" required /></Field>
            <Field label="原因"><Input name="reason" /></Field>
            <Button className="w-full" disabled={pending}>{pending ? "保存中..." : "加入拦截"}</Button>
          </form>
        </CardContent>
      </Card>
      <Card>
        <CardHeader><CardTitle>被拦截邮件</CardTitle></CardHeader>
        <CardContent className="space-y-2">
          {items.map((item) => (
            <div key={item.id} className="flex items-center justify-between gap-3 rounded-lg border p-3">
              <div className="min-w-0">
                <div className="truncate text-sm font-medium">{item.email}</div>
                <div className="truncate text-xs text-muted-foreground">{item.mailboxId ? mailboxes.find((m) => m.id === item.mailboxId)?.address : "全部邮箱"}{item.reason ? ` · ${item.reason}` : ""}</div>
              </div>
              <Button variant="ghost" size="icon" className="size-8 text-destructive" onClick={() => setPendingConfirm({ title: "移除拦截规则？", description: `${item.email} 之后将不再被此规则拦截。`, confirmText: "移除规则", onConfirm: () => { onDelete(item.id); setPendingConfirm(null) } })}><Trash2 className="h-4 w-4" /></Button>
            </div>
          ))}
          {items.length === 0 && <EmptyState text="暂无拦截发件人" />}
        </CardContent>
      </Card>
      <ConfirmDialog open={!!pendingConfirm} title={pendingConfirm?.title || ""} description={pendingConfirm?.description} confirmText={pendingConfirm?.confirmText || "移除"} destructive onOpenChange={(open) => { if (!open) setPendingConfirm(null) }} onConfirm={() => pendingConfirm?.onConfirm()} />
    </div>
  )
}

function StatsSection({ stats, mailbox, onRefresh }: { stats?: MailStats; mailbox?: Mailbox; onRefresh: () => void }) {
  return <div className="space-y-6"><div className="flex items-center justify-between"><div className="text-sm text-muted-foreground">当前统计：{mailbox?.address || "未选择邮箱"}</div><Button variant="outline" onClick={onRefresh}><RefreshCcw className="h-4 w-4" />刷新</Button></div><StatsSummary stats={stats} /><Card><CardHeader><CardTitle>文件夹分布</CardTitle></CardHeader><CardContent className="space-y-2">{(stats?.byFolder || []).map((f) => <div key={f.folder} className="grid grid-cols-[1fr_auto_auto_auto] items-center gap-3 rounded-lg border p-3 text-sm"><div className="font-medium">{folderLabel(f.folder)}</div><Badge variant="secondary">{f.count} 封</Badge><span className="text-muted-foreground">未读 {f.unread}</span><span className="text-muted-foreground">{formatBytes(f.bytes)}</span></div>)}</CardContent></Card></div>
}

function StatsSummary({ stats }: { stats?: MailStats }) {
  const cards = [{ label: "总邮件", value: stats?.totalMessages || 0 }, { label: "未读", value: stats?.unreadMessages || 0 }, { label: "星标", value: stats?.starredMessages || 0 }, { label: "附件", value: stats?.attachmentCount || 0 }, { label: "容量", value: formatBytes(stats?.storageBytes || 0) }]
  return <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-5">{cards.map((c) => <Card key={c.label}><CardContent className="p-4"><div className="text-2xl font-semibold tracking-tight">{c.value}</div><div className="text-xs text-muted-foreground">{c.label}</div></CardContent></Card>)}</div>
}

function CleanupButton({ icon, title, disabled, onClick }: { icon: React.ReactNode; title: string; disabled: boolean; onClick: () => void }) { return <Button variant="outline" className="h-auto justify-start p-4 text-left" disabled={disabled} onClick={onClick}><div className="mr-3 rounded-lg bg-muted p-2">{icon}</div><div className="font-medium">{title}</div></Button> }
function MailboxSelect({ value, mailboxes, onChange }: { value: string; mailboxes: Mailbox[]; onChange: (value: string) => void }) { return <Select value={value} onValueChange={onChange}><SelectTrigger><SelectValue /></SelectTrigger><SelectContent><SelectItem value="all">全部邮箱</SelectItem>{mailboxes.map((m) => <SelectItem key={m.id} value={m.id}>{m.address}</SelectItem>)}</SelectContent></Select> }
function Field({ label, children }: { label: string; children: React.ReactNode }) { return <div className="space-y-2"><Label>{label}</Label>{children}</div> }
function EmptyState({ text }: { text: string }) { return <div className="rounded-lg border border-dashed p-8 text-center text-sm text-muted-foreground">{text}</div> }
function folderLabel(folder: string) { return ({ Inbox: "收件箱", Sent: "已发送", Drafts: "草稿箱", Archive: "归档", Spam: "垃圾邮件", Trash: "回收站" } as Record<string, string>)[folder] || folder }
function clientServerHost(hostname?: string, address?: string) { const value = (hostname || "").trim(); if (value) return value; const domain = (address || "").split("@")[1]; return domain ? `mail.${domain}` : "mail.example.com" }
function AccountHeader({ collapsed, name, email, darkMode, onToggleTheme, onBack }: { collapsed: boolean; name: string; email?: string; darkMode: boolean; onToggleTheme: () => void; onBack: () => void }) {
  const displayName = cleanAccountName(name, email)
  if (collapsed) return <div className="flex justify-center"><Avatar className="size-9 rounded-full"><AvatarFallback className="bg-primary text-sm font-semibold text-primary-foreground">{accountInitial(displayName, email)}</AvatarFallback></Avatar></div>
  return <div className="flex items-center justify-between gap-3"><div className="flex min-w-0 items-center gap-3"><Avatar className="size-10 rounded-full"><AvatarFallback className="bg-primary text-sm font-semibold text-primary-foreground">{accountInitial(displayName, email)}</AvatarFallback></Avatar><div className="min-w-0 text-sm"><div className="truncate text-base font-semibold leading-5">{displayName}</div></div></div><div className="flex shrink-0 items-center gap-1"><Button type="button" variant="ghost" size="icon" className="size-9 rounded-lg text-muted-foreground" onClick={onToggleTheme}>{darkMode ? <Sun className="h-4 w-4" /> : <Moon className="h-4 w-4" />}</Button><Button type="button" variant="ghost" size="icon" className="size-9 rounded-lg text-muted-foreground" onClick={onBack}><ArrowLeft className="h-4 w-4" /></Button></div></div>
}
function cleanAccountName(name: string, email?: string) { const value = name.trim(); if (!value || (email && value.toLowerCase() === email.toLowerCase())) return email?.split("@")[0] || "用户"; return value }
function accountInitial(name: string, email?: string) { const source = cleanAccountName(name, email); const first = Array.from(source.trim())[0]; return (first || "蓝").toUpperCase() }

import * as React from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import type { ImperativePanelHandle } from "react-resizable-panels"
import { useNavigate, useSearchParams } from "react-router-dom"
import { ArrowLeft, BarChart3, Ban, Contact, Copy, KeyRound, LogOut, Mail, MailCheck, MailX, Moon, PanelLeftClose, PanelLeftOpen, RefreshCcw, Settings, ShieldCheck, SlidersHorizontal, Sun, Trash2 } from "lucide-react"
import { QRCodeSVG } from "qrcode.react"
import { api, Mailbox, MailStats } from "@/lib/api"
import { cn, formatBytes } from "@/lib/utils"
import { applyTheme, getInitialTheme } from "@/lib/theme"
import { useMe } from "@/hooks/use-me"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Badge } from "@/components/ui/badge"
import { Avatar, AvatarFallback } from "@/components/ui/avatar"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Separator } from "@/components/ui/separator"
import { ScrollArea } from "@/components/ui/scroll-area"
import { ResizableHandle, ResizablePanel, ResizablePanelGroup } from "@/components/ui/resizable"
import { Sidebar, SidebarContent, SidebarGroup, SidebarGroupContent, SidebarGroupLabel, SidebarHeader, SidebarMenu, SidebarMenuButton, SidebarMenuItem, SidebarProvider } from "@/components/ui/sidebar"
import { useToast } from "@/hooks/use-toast"

type Tab = "profile" | "mailboxes" | "contacts" | "cleanup" | "rules" | "blocked" | "stats"
const tabs: Record<Tab, { label: string; icon: React.ReactNode }> = {
  profile: { label: "账户资料", icon: <Settings className="h-4 w-4" /> },
  mailboxes: { label: "邮箱管理", icon: <Mail className="h-4 w-4" /> },
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
  const [ruleMailboxId, setRuleMailboxId] = React.useState("all")
  const [ruleAction, setRuleAction] = React.useState("archive")
  const [blockedMailboxId, setBlockedMailboxId] = React.useState("all")
  const themeMountedRef = React.useRef(false)

  const rawTab = params.get("tab") as Tab | null
  const tab: Tab = rawTab && tabKeys.includes(rawTab) ? rawTab : "profile"
  const user = me.data?.user
  const mailboxes = useQuery({ queryKey: ["mailboxes", "mine"], queryFn: api.myMailboxes })
  const contacts = useQuery({ queryKey: ["contacts"], queryFn: api.contacts })
  const rules = useQuery({ queryKey: ["rules"], queryFn: api.rules })
  const blocked = useQuery({ queryKey: ["blocked-senders"], queryFn: api.blockedSenders })
  const selectedMailbox = React.useMemo(() => mailboxes.data?.items.find((m) => m.id === mailboxId), [mailboxes.data?.items, mailboxId])
  const stats = useQuery({ queryKey: ["mail-stats", mailboxId], queryFn: () => api.mailStats(mailboxId), enabled: !!mailboxId })

  const profile = useMutation({
    mutationFn: (form: FormData) => api.updateProfile({ displayName: String(form.get("displayName") || "") }),
    onSuccess: (data) => { qc.setQueryData(["me"], data); toast({ title: "个人资料已保存" }) },
    onError: (error) => toast({ title: "保存失败", description: error.message }),
  })
  const password = useMutation({
    mutationFn: (form: FormData) => {
      const newPassword = String(form.get("newPassword") || "")
      if (newPassword !== String(form.get("confirmPassword") || "")) throw new Error("两次输入的新密码不一致")
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
  const createRule = useMutation({
    mutationFn: (form: FormData) => api.createRule({ mailboxId: ruleMailboxId === "all" ? "" : ruleMailboxId, name: String(form.get("name") || ""), fromContains: String(form.get("fromContains") || ""), subjectContains: String(form.get("subjectContains") || ""), action: ruleAction, enabled: true }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["rules"] }); toast({ title: "收件规则已保存" }) },
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

  React.useEffect(() => {
    const items = mailboxes.data?.items || []
    if (items.length > 0 && (!mailboxId || !items.some((m) => m.id === mailboxId))) setMailboxId(items[0].id)
  }, [mailboxId, mailboxes.data?.items])
  React.useEffect(() => { if (mailboxId) localStorage.setItem("lanqin:selected-mailbox", mailboxId) }, [mailboxId])
  React.useEffect(() => { applyTheme(darkMode, themeMountedRef.current); themeMountedRef.current = true }, [darkMode])

  async function logout() { await api.logout().catch(() => undefined); qc.clear(); navigate("/login", { replace: true }) }
  async function copy(text: string) { await navigator.clipboard.writeText(text); toast({ title: "已复制" }) }
  function setTab(next: Tab) { setParams(next === "profile" ? {} : { tab: next }) }
  function toggleSidebar() { sidebarCollapsed ? (sidebarPanelRef.current?.expand(14), setSidebarCollapsed(false)) : (sidebarPanelRef.current?.collapse(), setSidebarCollapsed(true)) }
  if (me.isLoading) return <div className="grid h-svh place-items-center text-muted-foreground">加载中...</div>
  if (me.isError || !user) return <div className="grid h-svh place-items-center text-muted-foreground">登录状态已失效</div>

  return (
    <div className="h-svh bg-background">
      <SidebarProvider className="h-full min-h-0 w-full">
        <ResizablePanelGroup direction="horizontal" className="h-full min-h-0 w-full">
          <ResizablePanel ref={sidebarPanelRef} collapsible collapsedSize={4} defaultSize={15} minSize={11} maxSize={24} onCollapse={() => setSidebarCollapsed(true)} onExpand={() => setSidebarCollapsed(false)}>
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
                <Separator className="my-2" />
                <Button type="button" variant="ghost" size={sidebarCollapsed ? "icon" : "sm"} className={cn(!sidebarCollapsed && "w-full justify-start")} onClick={toggleSidebar}>{sidebarCollapsed ? <PanelLeftOpen className="h-4 w-4" /> : <PanelLeftClose className="h-4 w-4" />}{!sidebarCollapsed && <span>收起侧栏</span>}</Button>
              </div>
            </Sidebar>
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
      </SidebarProvider>
    </div>
  )

  function renderTab() {
    if (tab === "mailboxes") return <MailboxManagement mailboxes={mailboxes.data?.items || []} selectedMailboxId={mailboxId} onSelect={setMailboxId} onCopy={copy} onOpen={(id) => { setMailboxId(id); navigate("/") }} />
    if (tab === "contacts") return <ContactsSection items={contacts.data?.items || []} loading={contacts.isLoading} pending={createContact.isPending} onCreate={(form) => createContact.mutate(form)} onDelete={(id) => deleteContact.mutate(id)} onCopy={copy} />
    if (tab === "cleanup") return <CleanupSection mailbox={selectedMailbox} stats={stats.data} pending={cleanup.isPending} onCleanup={(target) => cleanup.mutate(target)} />
    if (tab === "rules") return <RulesSection items={rules.data?.items || []} mailboxes={mailboxes.data?.items || []} mailboxId={ruleMailboxId} action={ruleAction} onMailboxChange={setRuleMailboxId} onActionChange={setRuleAction} onCreate={(form) => createRule.mutate(form)} onDelete={(id) => deleteRule.mutate(id)} pending={createRule.isPending} />
    if (tab === "blocked") return <BlockedSection items={blocked.data?.items || []} mailboxes={mailboxes.data?.items || []} mailboxId={blockedMailboxId} spamCount={stats.data?.byFolder.find((f) => f.role === "spam")?.count || 0} onMailboxChange={setBlockedMailboxId} onCreate={(form) => createBlocked.mutate(form)} onDelete={(id) => deleteBlocked.mutate(id)} pending={createBlocked.isPending} />
    if (tab === "stats") return <StatsSection stats={stats.data} mailbox={selectedMailbox} onRefresh={() => stats.refetch()} />
    return <ProfileOverview user={user!} profile={profile} password={password} passwordFormRef={passwordFormRef} stats={stats.data} twoFactorFormRef={twoFactorFormRef} setupTwoFactor={setupTwoFactor} enableTwoFactor={enableTwoFactor} disableTwoFactor={disableTwoFactor} onCopy={copy} />
  }
}

function ProfileOverview({ user, profile, password, passwordFormRef, stats, twoFactorFormRef, setupTwoFactor, enableTwoFactor, disableTwoFactor, onCopy }: { user: { email: string; displayName: string; role: string; disabled: boolean; twoFactorEnabled: boolean; createdAt: string }; profile: { mutate: (form: FormData) => void; isPending: boolean }; password: { mutate: (form: FormData) => void; isPending: boolean }; passwordFormRef: React.RefObject<HTMLFormElement>; stats?: MailStats; twoFactorFormRef: React.RefObject<HTMLFormElement>; setupTwoFactor: { data?: { secret: string; otpauthUrl: string }; mutate: () => void; reset: () => void; isPending: boolean }; enableTwoFactor: { mutate: (form: FormData) => void; isPending: boolean }; disableTwoFactor: { mutate: (form: FormData) => void; isPending: boolean }; onCopy: (text: string) => void }) {
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
              <Input name="currentPassword" type="password" required />
            </Field>
            <div className="grid gap-4 md:grid-cols-2">
              <Field label="新密码">
                <Input name="newPassword" type="password" minLength={8} required />
              </Field>
              <Field label="确认新密码">
                <Input name="confirmPassword" type="password" minLength={8} required />
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

function MailboxManagement({ mailboxes, selectedMailboxId, onSelect, onCopy, onOpen }: { mailboxes: Mailbox[]; selectedMailboxId: string; onSelect: (id: string) => void; onCopy: (text: string) => void; onOpen: (id: string) => void }) {
  return <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">{mailboxes.map((m) => <Card key={m.id} className={cn(selectedMailboxId === m.id && "border-primary")}><CardHeader><div className="flex items-start justify-between gap-3"><div className="min-w-0"><CardTitle className="truncate text-base">{m.address}</CardTitle></div>{selectedMailboxId === m.id && <Badge>当前</Badge>}</div></CardHeader><CardContent className="flex flex-wrap gap-2"><Button variant="outline" size="sm" onClick={() => onSelect(m.id)}>设为当前</Button><Button variant="outline" size="sm" onClick={() => onCopy(m.address)}><Copy className="h-4 w-4" />复制</Button><Button size="sm" onClick={() => onOpen(m.id)}>进入邮箱</Button></CardContent></Card>)}{mailboxes.length === 0 && <EmptyState text="暂无邮箱账号" />}</div>
}

function ContactsSection({ items, loading, onCreate, onDelete, onCopy, pending }: { items: { id: string; name: string; email: string; note: string }[]; loading: boolean; onCreate: (form: FormData) => void; onDelete: (id: string) => void; onCopy: (text: string) => void; pending: boolean }) {
  return <div className="grid gap-6 lg:grid-cols-[380px_minmax(0,1fr)]"><Card><CardHeader><CardTitle>新增联系人</CardTitle></CardHeader><CardContent><form className="space-y-4" onSubmit={(e) => { e.preventDefault(); onCreate(new FormData(e.currentTarget)); e.currentTarget.reset() }}><Field label="姓名"><Input name="name" placeholder="张三" /></Field><Field label="邮箱"><Input name="email" type="email" required /></Field><Field label="备注"><Input name="note" /></Field><Button className="w-full" disabled={pending}>{pending ? "保存中..." : "保存联系人"}</Button></form></CardContent></Card><Card><CardHeader><CardTitle>联系人列表</CardTitle></CardHeader><CardContent className="space-y-2">{items.map((item) => <div key={item.id} className="flex items-center justify-between gap-3 rounded-lg border p-3"><div className="min-w-0"><div className="truncate text-sm font-medium">{item.name}</div><div className="truncate text-xs text-muted-foreground">{item.email}{item.note ? ` · ${item.note}` : ""}</div></div><div className="flex shrink-0 gap-1"><Button variant="ghost" size="icon" className="size-8" onClick={() => onCopy(item.email)}><Copy className="h-4 w-4" /></Button><Button variant="ghost" size="icon" className="size-8 text-destructive" onClick={() => onDelete(item.id)}><Trash2 className="h-4 w-4" /></Button></div></div>)}{!loading && items.length === 0 && <EmptyState text="暂无联系人" />}</CardContent></Card></div>
}

function CleanupSection({ mailbox, stats, pending, onCleanup }: { mailbox?: Mailbox; stats?: MailStats; pending: boolean; onCleanup: (target: "empty-trash" | "empty-spam" | "archive-read-inbox") => void }) {
  return <div className="space-y-6"><StatsSummary stats={stats} /><Card><CardHeader><CardTitle>清理当前邮箱</CardTitle></CardHeader><CardContent className="grid gap-3 md:grid-cols-3"><CleanupButton icon={<MailCheck className="h-4 w-4" />} title="归档已读收件箱" disabled={!mailbox || pending} onClick={() => onCleanup("archive-read-inbox")} /><CleanupButton icon={<MailX className="h-4 w-4" />} title="清空垃圾邮件" disabled={!mailbox || pending} onClick={() => onCleanup("empty-spam")} /><CleanupButton icon={<Trash2 className="h-4 w-4" />} title="清空回收站" disabled={!mailbox || pending} onClick={() => onCleanup("empty-trash")} /></CardContent></Card></div>
}

function RulesSection({ items, mailboxes, mailboxId, action, onMailboxChange, onActionChange, onCreate, onDelete, pending }: { items: any[]; mailboxes: Mailbox[]; mailboxId: string; action: string; onMailboxChange: (value: string) => void; onActionChange: (value: string) => void; onCreate: (form: FormData) => void; onDelete: (id: string) => void; pending: boolean }) {
  return <div className="grid gap-6 lg:grid-cols-[420px_minmax(0,1fr)]"><Card><CardHeader><CardTitle>新增收件规则</CardTitle></CardHeader><CardContent><form className="space-y-4" onSubmit={(e) => { e.preventDefault(); onCreate(new FormData(e.currentTarget)); e.currentTarget.reset() }}><Field label="规则名称"><Input name="name" /></Field><Field label="适用邮箱"><MailboxSelect value={mailboxId} mailboxes={mailboxes} onChange={onMailboxChange} /></Field><div className="grid gap-3 md:grid-cols-2"><Field label="发件人包含"><Input name="fromContains" /></Field><Field label="主题包含"><Input name="subjectContains" /></Field></div><Field label="执行动作"><Select value={action} onValueChange={onActionChange}><SelectTrigger><SelectValue /></SelectTrigger><SelectContent><SelectItem value="archive">移入归档</SelectItem><SelectItem value="trash">移入回收站</SelectItem><SelectItem value="star">添加星标</SelectItem><SelectItem value="mark-read">标记已读</SelectItem></SelectContent></Select></Field><Button className="w-full" disabled={pending}>{pending ? "保存中..." : "保存规则"}</Button></form></CardContent></Card><Card><CardHeader><CardTitle>规则列表</CardTitle></CardHeader><CardContent className="space-y-2">{items.map((item) => <div key={item.id} className="flex items-center justify-between gap-3 rounded-lg border p-3"><div className="min-w-0"><div className="truncate text-sm font-medium">{item.name}<Badge variant="outline" className="ml-2">{actionLabels[item.action]}</Badge></div><div className="truncate text-xs text-muted-foreground">{item.mailboxId ? mailboxes.find((m) => m.id === item.mailboxId)?.address : "全部邮箱"} · {item.fromContains ? `发件人包含 ${item.fromContains}` : ""} {item.subjectContains ? `主题包含 ${item.subjectContains}` : ""}</div></div><Button variant="ghost" size="icon" className="size-8 text-destructive" onClick={() => onDelete(item.id)}><Trash2 className="h-4 w-4" /></Button></div>)}{items.length === 0 && <EmptyState text="暂无收件规则" />}</CardContent></Card></div>
}

function BlockedSection({ items, mailboxes, mailboxId, spamCount, onMailboxChange, onCreate, onDelete, pending }: { items: any[]; mailboxes: Mailbox[]; mailboxId: string; spamCount: number; onMailboxChange: (value: string) => void; onCreate: (form: FormData) => void; onDelete: (id: string) => void; pending: boolean }) {
  return <div className="grid gap-6 lg:grid-cols-[420px_minmax(0,1fr)]"><Card><CardHeader><CardTitle>新增拦截发件人</CardTitle></CardHeader><CardContent><form className="space-y-4" onSubmit={(e) => { e.preventDefault(); onCreate(new FormData(e.currentTarget)); e.currentTarget.reset() }}><Field label="适用邮箱"><MailboxSelect value={mailboxId} mailboxes={mailboxes} onChange={onMailboxChange} /></Field><Field label="发件人邮箱"><Input name="email" type="email" required /></Field><Field label="原因"><Input name="reason" /></Field><Button className="w-full" disabled={pending}>{pending ? "保存中..." : "加入拦截"}</Button></form></CardContent></Card><Card><CardHeader><CardTitle>被拦截邮件</CardTitle></CardHeader><CardContent className="space-y-2">{items.map((item) => <div key={item.id} className="flex items-center justify-between gap-3 rounded-lg border p-3"><div className="min-w-0"><div className="truncate text-sm font-medium">{item.email}</div><div className="truncate text-xs text-muted-foreground">{item.mailboxId ? mailboxes.find((m) => m.id === item.mailboxId)?.address : "全部邮箱"}{item.reason ? ` · ${item.reason}` : ""}</div></div><Button variant="ghost" size="icon" className="size-8 text-destructive" onClick={() => onDelete(item.id)}><Trash2 className="h-4 w-4" /></Button></div>)}{items.length === 0 && <EmptyState text="暂无拦截发件人" />}</CardContent></Card></div>
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
function AccountHeader({ collapsed, name, email, darkMode, onToggleTheme, onBack }: { collapsed: boolean; name: string; email?: string; darkMode: boolean; onToggleTheme: () => void; onBack: () => void }) {
  const displayName = cleanAccountName(name, email)
  if (collapsed) return <div className="flex justify-center"><Avatar className="size-9 rounded-full"><AvatarFallback className="bg-primary text-sm font-semibold text-primary-foreground">{accountInitial(displayName, email)}</AvatarFallback></Avatar></div>
  return <div className="flex items-center justify-between gap-3"><div className="flex min-w-0 items-center gap-3"><Avatar className="size-10 rounded-full"><AvatarFallback className="bg-primary text-sm font-semibold text-primary-foreground">{accountInitial(displayName, email)}</AvatarFallback></Avatar><div className="min-w-0 text-sm"><div className="truncate text-base font-semibold leading-5">{displayName}</div></div></div><div className="flex shrink-0 items-center gap-1"><Button type="button" variant="ghost" size="icon" className="size-9 rounded-lg text-muted-foreground" onClick={onToggleTheme}>{darkMode ? <Sun className="h-4 w-4" /> : <Moon className="h-4 w-4" />}</Button><Button type="button" variant="ghost" size="icon" className="size-9 rounded-lg text-muted-foreground" onClick={onBack}><ArrowLeft className="h-4 w-4" /></Button></div></div>
}
function cleanAccountName(name: string, email?: string) { const value = name.trim(); if (!value || (email && value.toLowerCase() === email.toLowerCase())) return email?.split("@")[0] || "用户"; return value }
function accountInitial(name: string, email?: string) { const source = cleanAccountName(name, email); const first = Array.from(source.trim())[0]; return (first || "蓝").toUpperCase() }

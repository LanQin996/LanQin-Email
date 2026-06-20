import * as React from "react"
import DOMPurify from "dompurify"
import { useSearchParams } from "react-router-dom"
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { ArrowRight, CheckCircle2, Circle, Copy, Globe2, Mailbox, MoreHorizontal, Plus, RefreshCcw, Search, ShieldCheck, Trash2, Users } from "lucide-react"
import { api, AdminUser, Alias, DNSRecord, Domain, Mailbox as MailboxType, MailMessage, MailTemplate, SystemSettings } from "@/lib/api"
import { cn, decodeMimeHeader, formatBytes, formatDate } from "@/lib/utils"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Badge } from "@/components/ui/badge"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Checkbox } from "@/components/ui/checkbox"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog"
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu"
import { ScrollArea } from "@/components/ui/scroll-area"
import { Separator } from "@/components/ui/separator"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Switch } from "@/components/ui/switch"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Textarea } from "@/components/ui/textarea"
import { ConfirmDialog } from "@/components/confirm-dialog"
import { useToast } from "@/hooks/use-toast"

type Section = "overview" | "users" | "domains" | "mailboxes" | "aliases" | "messages" | "settings"
type PendingConfirm = { title: string; description?: string; confirmText: string; onConfirm: () => void }

const sectionLabels: Record<Section, string> = {
  overview: "概览",
  users: "用户",
  domains: "域名",
  mailboxes: "邮箱账号",
  aliases: "别名转发",
  messages: "全部邮件",
  settings: "系统设置",
}
const sectionKeys = Object.keys(sectionLabels) as Section[]

export function AdminPage() {
  const overview = useQuery({ queryKey: ["admin", "overview"], queryFn: api.adminOverview })
  const users = useQuery({ queryKey: ["admin", "users"], queryFn: api.users })
  const domains = useQuery({ queryKey: ["admin", "domains"], queryFn: api.domains })
  const mailboxes = useQuery({ queryKey: ["admin", "mailboxes"], queryFn: api.mailboxes })
  const aliases = useQuery({ queryKey: ["admin", "aliases"], queryFn: api.aliases })
  const settings = useQuery({ queryKey: ["admin", "settings"], queryFn: api.systemSettings })
  const [params, setParams] = useSearchParams()

  const domainItems = domains.data?.items || []
  const mailboxItems = mailboxes.data?.items || []
  const aliasItems = aliases.data?.items || []
  const userItems = users.data?.items || []
  const rawSection = params.get("section") as Section | null
  const section: Section = rawSection && sectionKeys.includes(rawSection) ? rawSection : "overview"

  return (
    <ScrollArea className="h-[calc(100svh-3rem)] md:h-svh">
      <main className="p-4 sm:p-6">
        <div className="mb-6 flex flex-col gap-4 md:flex-row md:items-center md:justify-between">
          <h1 className="text-2xl font-semibold tracking-tight">{sectionLabels[section]}</h1>
        </div>

        {section === "overview" && (
          <div className="mb-6 grid gap-4 md:grid-cols-4">
            <Stat icon={<Users />} label="用户" value={overview.data?.users || 0} />
            <Stat icon={<Globe2 />} label="域名" value={overview.data?.domains || 0} />
            <Stat icon={<Mailbox />} label="邮箱账号" value={overview.data?.mailboxes || 0} />
            <Stat icon={<ShieldCheck />} label="存储" value={formatBytes(overview.data?.storageBytes || 0)} />
          </div>
        )}

        {section === "overview" && <OverviewSection overview={overview.data} domains={domainItems} settings={settings.data} onSectionChange={(next) => setParams(next === "overview" ? {} : { section: next })} />}
        {section === "users" && <UsersSection users={userItems} />}
        {section === "domains" && <DomainsSection domains={domainItems} />}
        {section === "mailboxes" && <MailboxesSection mailboxes={mailboxItems} users={userItems} domains={domainItems} />}
        {section === "aliases" && <AliasesSection aliases={aliasItems} domains={domainItems} />}
        {section === "messages" && <AdminMessagesSection mailboxes={mailboxItems} />}
        {section === "settings" && <SystemSettingsSection settings={settings.data} domains={domainItems} />}
      </main>
    </ScrollArea>
  )
}
function OverviewSection({ overview, domains, settings, onSectionChange }: { overview?: { activeUsers: number; activeMailboxes: number; aliases: number; messages: number; unreadMessages: number }; domains: Domain[]; settings?: SystemSettings; onSectionChange: (section: Section) => void }) {
  const checklist = setupChecklist(overview, domains, settings)
  return (
    <div className="space-y-6">
      <div className="grid gap-6 xl:grid-cols-[minmax(0,1fr)_420px]">
        <Card>
          <CardHeader><CardTitle>系统状态</CardTitle></CardHeader>
          <CardContent className="grid gap-3 md:grid-cols-2 xl:grid-cols-4">
            <InfoBox label="活跃用户" value={overview?.activeUsers || 0} />
            <InfoBox label="活跃邮箱" value={overview?.activeMailboxes || 0} />
            <InfoBox label="别名转发" value={overview?.aliases || 0} />
            <InfoBox label="未读邮件" value={overview?.unreadMessages || 0} />
          </CardContent>
        </Card>
        <Card>
          <CardHeader><CardTitle>首次配置</CardTitle></CardHeader>
          <CardContent className="space-y-2">
            {checklist.map((item) => (
              <Button key={item.key} type="button" variant="outline" className="h-auto w-full justify-start gap-3 px-3 py-2 text-left font-normal" onClick={() => onSectionChange(item.section)}>
                {item.done ? <CheckCircle2 className="h-4 w-4 shrink-0 text-green-600" /> : <Circle className="h-4 w-4 shrink-0 text-muted-foreground" />}
                <span className="min-w-0 flex-1">
                  <span className="block font-medium">{item.title}</span>
                  <span className="block truncate text-xs text-muted-foreground">{item.detail}</span>
                </span>
                <ArrowRight className="h-4 w-4 shrink-0 text-muted-foreground" />
              </Button>
            ))}
          </CardContent>
        </Card>
      </div>
      <div className="grid gap-6 xl:grid-cols-[minmax(0,1fr)_420px]">
        <Card>
          <CardHeader><CardTitle>DNS 状态</CardTitle></CardHeader>
          <CardContent className="space-y-2">
            {domains.map((domain) => <DomainBadgeRow key={domain.id} domain={domain} />)}
            {domains.length === 0 && <Empty text="暂无域名" />}
          </CardContent>
        </Card>
        <Card>
          <CardHeader><CardTitle>运行提示</CardTitle></CardHeader>
          <CardContent className="space-y-3 text-sm text-muted-foreground">
            <InfoLine label="公网地址" value={settings?.publicBaseUrl || "-"} />
            <InfoLine label="SMTP" value={settings?.smtpHost ? `${settings.smtpHost}:${settings.smtpPort}` : "-"} />
            <InfoLine label="注册" value={settings?.openRegistration ? "已开放" : "关闭"} />
            <InfoLine label="用户自助申请" value={settings?.userMailboxApplyEnabled ? "已启用" : "关闭"} />
          </CardContent>
        </Card>
      </div>
    </div>
  )
}

function setupChecklist(overview: { activeUsers: number; activeMailboxes: number; aliases: number; messages: number; unreadMessages: number } | undefined, domains: Domain[], settings?: SystemSettings) {
  const hasDomain = domains.length > 0
  const dnsReady = domains.some((domain) => domain.dnsStatus === "ok")
  const hasMailbox = (overview?.activeMailboxes || 0) > 0
  const hasMail = (overview?.messages || 0) > 0
  return [
    { key: "domain", title: "添加邮件域名", detail: hasDomain ? `${domains.length} 个域名已添加` : "先添加 example.com 这样的邮件域名", done: hasDomain, section: "domains" as Section },
    { key: "dns", title: "完成 DNS 检测", detail: dnsReady ? "至少一个域名 DNS 正常" : "配置 MX、SPF、DKIM、DMARC 后执行检测", done: dnsReady, section: "domains" as Section },
    { key: "mailbox", title: "创建邮箱账号", detail: hasMailbox ? `${overview?.activeMailboxes || 0} 个活跃邮箱` : "给管理员或用户创建第一个邮箱", done: hasMailbox, section: "mailboxes" as Section },
    { key: "smtp", title: "确认发信链路", detail: settings?.smtpHost ? `内置 Postfix：${settings.smtpHost}:${settings.smtpPort}` : "默认使用内置 Postfix", done: true, section: "settings" as Section },
    { key: "mail", title: "完成收发测试", detail: hasMail ? `${overview?.messages || 0} 封邮件已入库` : "发送或接收一封测试邮件", done: hasMail, section: "messages" as Section },
  ]
}

function InfoLine({ label, value }: { label: string; value: React.ReactNode }) {
  return <div className="flex items-center justify-between gap-3 rounded-md border px-3 py-2"><span>{label}</span><span className="min-w-0 truncate font-medium text-foreground">{value}</span></div>
}

function UsersSection({ users }: { users: AdminUser[] }) {
  const qc = useQueryClient()
  const { toast } = useToast()
  const [query, setQuery] = React.useState("")
  const [roleFilter, setRoleFilter] = React.useState("all")
  const [statusFilter, setStatusFilter] = React.useState("all")
  const [pendingConfirm, setPendingConfirm] = React.useState<PendingConfirm | null>(null)
  const filteredUsers = users.filter((user) => {
    const keyword = query.trim().toLowerCase()
    const matchesKeyword = !keyword || [user.email, user.displayName, ...(user.mailboxes || [])].some((value) => value.toLowerCase().includes(keyword))
    const matchesRole = roleFilter === "all" || user.role === roleFilter
    const matchesStatus = statusFilter === "all" || (statusFilter === "active" ? !user.disabled : user.disabled)
    return matchesKeyword && matchesRole && matchesStatus
  })
  const remove = useMutation({ mutationFn: api.deleteUser, onSuccess: () => { setPendingConfirm(null); invalidateAdmin(qc); toast({ title: "用户已删除" }) }, onError: (e) => toast({ title: "删除失败", description: e.message }) })
  return (
    <Card>
      <CardHeader>
        <div className="flex flex-col gap-3 md:flex-row md:items-center md:justify-between">
          <CardTitle>用户管理</CardTitle>
          <CreateUserDialog />
        </div>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex flex-col gap-3 lg:flex-row">
          <div className="relative flex-1">
            <Search className="absolute left-3 top-2.5 h-4 w-4 text-muted-foreground" />
            <Input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="搜索用户、邮箱、显示名称" className="pl-9" />
          </div>
          <Select value={roleFilter} onValueChange={setRoleFilter}>
            <SelectTrigger className="lg:w-36"><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem value="all">全部角色</SelectItem>
              <SelectItem value="admin">管理员</SelectItem>
              <SelectItem value="user">普通用户</SelectItem>
            </SelectContent>
          </Select>
          <Select value={statusFilter} onValueChange={setStatusFilter}>
            <SelectTrigger className="lg:w-36"><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem value="all">全部状态</SelectItem>
              <SelectItem value="active">正常</SelectItem>
              <SelectItem value="disabled">停用</SelectItem>
            </SelectContent>
          </Select>
        </div>
        <div className="space-y-3 md:hidden">
          {filteredUsers.map((user) => (
            <div key={user.id} className="rounded-lg border p-4">
              <div className="flex items-start justify-between gap-3">
                <div className="min-w-0">
                  <div className="truncate font-medium">{user.displayName}</div>
                  <div className="truncate text-xs text-muted-foreground">{user.email}</div>
                </div>
                <UserActions user={user} onDelete={() => setPendingConfirm({ title: "删除用户？", description: `将删除 ${user.email} 及其关联数据。`, confirmText: "删除用户", onConfirm: () => remove.mutate(user.id) })} />
              </div>
              <div className="mt-3 flex flex-wrap gap-2">
                <Badge variant={user.role === "admin" ? "default" : "secondary"}>{user.role === "admin" ? "管理员" : "普通用户"}</Badge>
                <Badge variant={user.disabled ? "secondary" : "default"}>{user.disabled ? "停用" : "正常"}</Badge>
                <Badge variant="outline">{new Date(user.createdAt).toLocaleDateString()}</Badge>
              </div>
              <div className="mt-3"><UserMailboxCell user={user} /></div>
            </div>
          ))}
        </div>
        <div className="hidden md:block">
          <Table>
            <TableHeader><TableRow><TableHead>用户</TableHead><TableHead>角色</TableHead><TableHead>邮箱</TableHead><TableHead>状态</TableHead><TableHead>创建时间</TableHead><TableHead className="w-16"></TableHead></TableRow></TableHeader>
            <TableBody>
              {filteredUsers.map((user) => (
                <TableRow key={user.id}>
                  <TableCell>
                    <div className="font-medium">{user.displayName}</div>
                    <div className="text-xs text-muted-foreground">{user.email}</div>
                  </TableCell>
                  <TableCell><Badge variant={user.role === "admin" ? "default" : "secondary"}>{user.role === "admin" ? "管理员" : "普通用户"}</Badge></TableCell>
                  <TableCell><UserMailboxCell user={user} /></TableCell>
                  <TableCell><Badge variant={user.disabled ? "secondary" : "default"}>{user.disabled ? "停用" : "正常"}</Badge></TableCell>
                  <TableCell className="text-muted-foreground">{new Date(user.createdAt).toLocaleDateString()}</TableCell>
                  <TableCell><UserActions user={user} onDelete={() => setPendingConfirm({ title: "删除用户？", description: `将删除 ${user.email} 及其关联数据。`, confirmText: "删除用户", onConfirm: () => remove.mutate(user.id) })} /></TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
        {filteredUsers.length === 0 && <Empty text="没有匹配的用户" />}
      </CardContent>
      <ConfirmDialog open={!!pendingConfirm} title={pendingConfirm?.title || ""} description={pendingConfirm?.description} confirmText={pendingConfirm?.confirmText || "删除"} destructive pending={remove.isPending} onOpenChange={(open) => { if (!open) setPendingConfirm(null) }} onConfirm={() => pendingConfirm?.onConfirm()} />
    </Card>
  )
}

function DomainsSection({ domains }: { domains: Domain[] }) {
  const qc = useQueryClient()
  const { toast } = useToast()
  const [pendingConfirm, setPendingConfirm] = React.useState<PendingConfirm | null>(null)
  const update = useMutation({ mutationFn: ({ id, status }: { id: string; status: string }) => api.updateDomain(id, { status }), onSuccess: () => { invalidateAdmin(qc); toast({ title: "域名已更新" }) }, onError: (e) => toast({ title: "更新失败", description: e.message }) })
  const remove = useMutation({ mutationFn: api.deleteDomain, onSuccess: () => { setPendingConfirm(null); invalidateAdmin(qc); toast({ title: "域名已删除" }) }, onError: (e) => toast({ title: "删除失败", description: e.message }) })
  return (
    <Card>
      <CardHeader>
        <div className="flex flex-col gap-3 md:flex-row md:items-center md:justify-between">
          <CardTitle>域名管理</CardTitle>
          <CreateDomainDialog />
        </div>
      </CardHeader>
      <CardContent className="space-y-3">
        {domains.map((domain) => (
          <div key={domain.id} className="flex flex-col gap-3 rounded-lg border p-4 md:flex-row md:items-center md:justify-between">
            <div>
              <div className="font-medium">{domain.name}</div>
              <div className="text-xs text-muted-foreground">selector: {domain.dkimSelector}</div>
            </div>
            <div className="flex flex-wrap items-center gap-2">
              <Badge variant={domain.status === "active" ? "default" : "secondary"}>{domain.status === "active" ? "启用" : "停用"}</Badge>
              <Badge variant={domain.dnsStatus === "ok" ? "default" : "secondary"}>{domain.dnsStatus === "ok" ? "DNS 正常" : domain.dnsStatus}</Badge>
              <DomainDNSDialog domain={domain} />
              <Button variant="outline" size="sm" onClick={() => update.mutate({ id: domain.id, status: domain.status === "active" ? "disabled" : "active" })}>{domain.status === "active" ? "停用" : "启用"}</Button>
              <Button variant="outline" size="sm" onClick={() => setPendingConfirm({ title: "删除域名？", description: `将删除 ${domain.name}，相关邮箱、别名和邮件也可能受影响。`, confirmText: "删除域名", onConfirm: () => remove.mutate(domain.id) })}><Trash2 className="h-4 w-4" />删除</Button>
            </div>
          </div>
        ))}
        {domains.length === 0 && <Empty text="暂无域名" />}
      </CardContent>
      <ConfirmDialog open={!!pendingConfirm} title={pendingConfirm?.title || ""} description={pendingConfirm?.description} confirmText={pendingConfirm?.confirmText || "删除"} destructive pending={remove.isPending} onOpenChange={(open) => { if (!open) setPendingConfirm(null) }} onConfirm={() => pendingConfirm?.onConfirm()} />
    </Card>
  )
}

function DomainDNSDialog({ domain }: { domain: Domain }) {
  return (
    <Dialog>
      <DialogTrigger asChild>
        <Button variant="outline" size="sm">DNS</Button>
      </DialogTrigger>
      <DialogContent className="max-h-[86vh] overflow-y-auto sm:max-w-3xl">
        <DialogHeader><DialogTitle>{domain.name} DNS</DialogTitle></DialogHeader>
        <DNSPanel domain={domain} embedded />
      </DialogContent>
    </Dialog>
  )
}

function MailboxesSection({ mailboxes, users, domains }: { mailboxes: MailboxType[]; users: AdminUser[]; domains: Domain[] }) {
  const qc = useQueryClient()
  const { toast } = useToast()
  const [pendingConfirm, setPendingConfirm] = React.useState<PendingConfirm | null>(null)
  const remove = useMutation({ mutationFn: api.deleteMailbox, onSuccess: () => { setPendingConfirm(null); invalidateAdmin(qc); toast({ title: "邮箱已删除" }) }, onError: (e) => toast({ title: "删除失败", description: e.message }) })
  return (
    <Card>
      <CardHeader>
        <div className="flex flex-col gap-3 md:flex-row md:items-center md:justify-between">
          <CardTitle>邮箱账号管理</CardTitle>
          <CreateMailboxDialog domains={domains} users={users} />
        </div>
      </CardHeader>
      <CardContent>
        <div className="space-y-3 md:hidden">
          {mailboxes.map((mailbox) => (
            <div key={mailbox.id} className="rounded-lg border p-4">
              <div className="flex items-start justify-between gap-3">
                <div className="min-w-0">
                  <div className="truncate font-medium">{mailbox.address}</div>
                  <div className="truncate text-xs text-muted-foreground">{mailbox.userEmail || mailbox.userId}</div>
                </div>
                <MailboxActions mailbox={mailbox} users={users} onDelete={() => setPendingConfirm({ title: "删除邮箱？", description: `将删除 ${mailbox.address} 和其中邮件。`, confirmText: "删除邮箱", onConfirm: () => remove.mutate(mailbox.id) })} />
              </div>
              <div className="mt-3 flex flex-wrap gap-2">
                <Badge variant={mailbox.status === "active" ? "default" : "secondary"}>{mailbox.status === "active" ? "启用" : "停用"}</Badge>
                <Badge variant="outline">{mailbox.quotaMb} MB</Badge>
                <Badge variant="outline">{mailbox.displayName || "未命名"}</Badge>
              </div>
            </div>
          ))}
        </div>
        <div className="hidden md:block">
          <Table>
            <TableHeader><TableRow><TableHead>地址</TableHead><TableHead>归属用户</TableHead><TableHead>名称</TableHead><TableHead>配额</TableHead><TableHead>状态</TableHead><TableHead className="w-16"></TableHead></TableRow></TableHeader>
            <TableBody>
              {mailboxes.map((mailbox) => (
                <TableRow key={mailbox.id}>
                  <TableCell className="font-medium">{mailbox.address}</TableCell>
                  <TableCell className="text-muted-foreground">{mailbox.userEmail || mailbox.userId}</TableCell>
                  <TableCell>{mailbox.displayName}</TableCell>
                  <TableCell>{mailbox.quotaMb} MB</TableCell>
                  <TableCell><Badge variant={mailbox.status === "active" ? "default" : "secondary"}>{mailbox.status === "active" ? "启用" : "停用"}</Badge></TableCell>
                  <TableCell><MailboxActions mailbox={mailbox} users={users} onDelete={() => setPendingConfirm({ title: "删除邮箱？", description: `将删除 ${mailbox.address} 和其中邮件。`, confirmText: "删除邮箱", onConfirm: () => remove.mutate(mailbox.id) })} /></TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
        {mailboxes.length === 0 && <Empty text="暂无邮箱账号" />}
      </CardContent>
      <ConfirmDialog open={!!pendingConfirm} title={pendingConfirm?.title || ""} description={pendingConfirm?.description} confirmText={pendingConfirm?.confirmText || "删除"} destructive pending={remove.isPending} onOpenChange={(open) => { if (!open) setPendingConfirm(null) }} onConfirm={() => pendingConfirm?.onConfirm()} />
    </Card>
  )
}

function AliasesSection({ aliases, domains }: { aliases: Alias[]; domains: Domain[] }) {
  const qc = useQueryClient()
  const { toast } = useToast()
  const [pendingConfirm, setPendingConfirm] = React.useState<PendingConfirm | null>(null)
  const update = useMutation({ mutationFn: ({ id, payload }: { id: string; payload: { source: string; destination: string; enabled: boolean } }) => api.updateAlias(id, payload), onSuccess: () => { invalidateAdmin(qc); toast({ title: "别名已更新" }) }, onError: (e) => toast({ title: "更新失败", description: e.message }) })
  const remove = useMutation({ mutationFn: api.deleteAlias, onSuccess: () => { setPendingConfirm(null); invalidateAdmin(qc); toast({ title: "别名已删除" }) }, onError: (e) => toast({ title: "删除失败", description: e.message }) })
  return (
    <Card>
      <CardHeader>
        <div className="flex flex-col gap-3 md:flex-row md:items-center md:justify-between">
          <CardTitle>别名/转发管理</CardTitle>
          <CreateAliasDialog domains={domains} />
        </div>
      </CardHeader>
      <CardContent>
        <div className="space-y-3 md:hidden">
          {aliases.map((alias) => (
            <div key={alias.id} className="rounded-lg border p-4">
              <div className="flex items-start justify-between gap-3">
                <div className="min-w-0">
                  <div className="truncate font-medium">{alias.source}</div>
                  <div className="truncate text-xs text-muted-foreground">{alias.destination}</div>
                </div>
                <AliasActions alias={alias} onToggle={() => update.mutate({ id: alias.id, payload: { source: alias.source, destination: alias.destination, enabled: !alias.enabled } })} onDelete={() => setPendingConfirm({ title: "删除别名？", description: `${alias.source} 将不再转发到 ${alias.destination}。`, confirmText: "删除别名", onConfirm: () => remove.mutate(alias.id) })} />
              </div>
              <div className="mt-3 flex flex-wrap gap-2">
                <Badge variant={alias.enabled ? "default" : "secondary"}>{alias.enabled ? "启用" : "停用"}</Badge>
                <Badge variant="outline">{domains.find((d) => d.id === alias.domainId)?.name || alias.domainId}</Badge>
              </div>
            </div>
          ))}
        </div>
        <div className="hidden md:block">
          <Table>
            <TableHeader><TableRow><TableHead>来源</TableHead><TableHead>目标</TableHead><TableHead>域名</TableHead><TableHead>状态</TableHead><TableHead className="w-16"></TableHead></TableRow></TableHeader>
            <TableBody>
              {aliases.map((alias) => (
                <TableRow key={alias.id}>
                  <TableCell className="font-medium">{alias.source}</TableCell>
                  <TableCell>{alias.destination}</TableCell>
                  <TableCell className="text-muted-foreground">{domains.find((d) => d.id === alias.domainId)?.name || alias.domainId}</TableCell>
                  <TableCell><Badge variant={alias.enabled ? "default" : "secondary"}>{alias.enabled ? "启用" : "停用"}</Badge></TableCell>
                  <TableCell><AliasActions alias={alias} onToggle={() => update.mutate({ id: alias.id, payload: { source: alias.source, destination: alias.destination, enabled: !alias.enabled } })} onDelete={() => setPendingConfirm({ title: "删除别名？", description: `${alias.source} 将不再转发到 ${alias.destination}。`, confirmText: "删除别名", onConfirm: () => remove.mutate(alias.id) })} /></TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
        {aliases.length === 0 && <Empty text="暂无别名转发" />}
      </CardContent>
      <ConfirmDialog open={!!pendingConfirm} title={pendingConfirm?.title || ""} description={pendingConfirm?.description} confirmText={pendingConfirm?.confirmText || "删除"} destructive pending={remove.isPending} onOpenChange={(open) => { if (!open) setPendingConfirm(null) }} onConfirm={() => pendingConfirm?.onConfirm()} />
    </Card>
  )
}

function AdminMessagesSection({ mailboxes }: { mailboxes: MailboxType[] }) {
  const qc = useQueryClient()
  const [query, setQuery] = React.useState("")
  const [mailboxId, setMailboxId] = React.useState("all")
  const [folder, setFolder] = React.useState("all")
  const [selectedId, setSelectedId] = React.useState<string | null>(null)
  const messages = useInfiniteQuery({
    queryKey: ["admin", "messages", mailboxId, folder, query],
    queryFn: ({ pageParam }) => api.adminMessages({
      mailboxId: mailboxId === "all" ? "" : mailboxId,
      folder: folder === "all" ? "" : folder,
      q: query,
      cursor: typeof pageParam === "string" ? pageParam : "",
    }),
    initialPageParam: "",
    getNextPageParam: (lastPage) => lastPage.nextCursor || undefined,
  })
  const detail = useQuery({ queryKey: ["admin", "message", selectedId], queryFn: () => api.adminMessage(selectedId!), enabled: !!selectedId })
  const items = messages.data?.pages.flatMap((page) => page.items || []) || []
  return (
    <Card>
      <CardHeader>
        <div className="flex flex-col gap-3 md:flex-row md:items-center md:justify-between">
          <CardTitle>全部邮件</CardTitle>
          <Button variant="outline" size="sm" onClick={() => qc.invalidateQueries({ queryKey: ["admin", "messages"] })}>
            <RefreshCcw className="h-4 w-4" />刷新
          </Button>
        </div>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex flex-col gap-3 xl:flex-row">
          <div className="relative flex-1">
            <Search className="absolute left-3 top-2.5 h-4 w-4 text-muted-foreground" />
            <Input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="搜索主题、发件人、收件人、邮箱" className="pl-9" />
          </div>
          <Select value={mailboxId} onValueChange={setMailboxId}>
            <SelectTrigger className="xl:w-72"><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem value="all">全部邮箱</SelectItem>
              <SelectItem value="unregistered">未注册收件</SelectItem>
              {mailboxes.map((mailbox) => <SelectItem key={mailbox.id} value={mailbox.id}>{mailbox.address}</SelectItem>)}
            </SelectContent>
          </Select>
          <Select value={folder} onValueChange={setFolder}>
            <SelectTrigger className="xl:w-40"><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem value="all">全部文件夹</SelectItem>
              <SelectItem value="Inbox">收件箱</SelectItem>
              <SelectItem value="Sent">已发送</SelectItem>
              <SelectItem value="Archive">归档</SelectItem>
              <SelectItem value="Spam">垃圾邮件</SelectItem>
              <SelectItem value="Trash">回收站</SelectItem>
              <SelectItem value="Unregistered">未注册收件</SelectItem>
            </SelectContent>
          </Select>
        </div>
        <div className="space-y-3 md:hidden">
          {items.map((message) => (
            <div key={message.id} className="rounded-lg border p-4">
              <div className="flex items-start justify-between gap-3">
                <div className="min-w-0">
                  <div className="truncate font-medium">{message.subject}</div>
                  <div className="mt-1 line-clamp-2 text-xs text-muted-foreground">{message.snippet}</div>
                </div>
                <Button variant="ghost" size="sm" onClick={() => setSelectedId(message.id)}>查看</Button>
              </div>
              <div className="mt-3 space-y-2 text-sm">
                <div className="truncate text-muted-foreground">邮箱：{message.mailboxAddress || message.recipientAddress || "-"}</div>
                <div className="truncate text-muted-foreground">发件人：{adminSenderDisplayName(message)}</div>
                <div className="truncate text-muted-foreground">收件人：{message.recipientAddress || message.to?.join(", ") || ""}</div>
              </div>
              <div className="mt-3 flex flex-wrap gap-2">
                <Badge variant="secondary">{folderName(message.folder)}</Badge>
                <Badge variant="outline">{formatDate(message.receivedAt)}</Badge>
              </div>
            </div>
          ))}
        </div>
        <div className="hidden md:block">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>邮件</TableHead>
                <TableHead>邮箱</TableHead>
                <TableHead>发件人</TableHead>
                <TableHead>收件人</TableHead>
                <TableHead>文件夹</TableHead>
                <TableHead>时间</TableHead>
                <TableHead className="w-20"></TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {items.map((message) => (
                <TableRow key={message.id}>
                  <TableCell className="max-w-[360px]">
                    <div className="truncate font-medium">{message.subject}</div>
                    <div className="truncate text-xs text-muted-foreground">{message.snippet}</div>
                  </TableCell>
                  <TableCell>
                    <div className="font-medium">{message.mailboxAddress || message.recipientAddress || "-"}</div>
                    {message.ownerEmail && <div className="text-xs text-muted-foreground">{message.ownerEmail}</div>}
                  </TableCell>
                  <TableCell className="max-w-[220px] truncate" title={adminSenderTitle(message)}>{adminSenderDisplayName(message)}</TableCell>
                  <TableCell className="max-w-[220px] truncate">{message.recipientAddress || message.to?.join(", ") || ""}</TableCell>
                  <TableCell><Badge variant="secondary">{folderName(message.folder)}</Badge></TableCell>
                  <TableCell className="text-muted-foreground">{formatDate(message.receivedAt)}</TableCell>
                  <TableCell><Button variant="ghost" size="sm" onClick={() => setSelectedId(message.id)}>查看</Button></TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
        {messages.isLoading && <Empty text="加载中..." />}
        {!messages.isLoading && items.length === 0 && <Empty text="暂无邮件" />}
        {!messages.isLoading && messages.hasNextPage && (
          <div className="flex justify-center">
            <Button variant="outline" size="sm" disabled={messages.isFetchingNextPage} onClick={() => messages.fetchNextPage()}>
              {messages.isFetchingNextPage ? "加载中..." : "加载更多"}
            </Button>
          </div>
        )}
      </CardContent>
      <AdminMessageDialog message={detail.data} loading={detail.isLoading} open={!!selectedId} onOpenChange={(open) => { if (!open) setSelectedId(null) }} />
    </Card>
  )
}

function SystemSettingsSection({ settings, domains }: { settings?: SystemSettings; domains: Domain[] }) {
  const qc = useQueryClient()
  const { toast } = useToast()
  const templates = useQuery({ queryKey: ["admin", "mail-templates"], queryFn: api.mailTemplates })
  const [settingsTab, setSettingsTab] = React.useState<"base" | "smtp" | "storage" | "mail" | "templates" | "security">("base")
  const [smtpRequireTls, setSmtpRequireTls] = React.useState(false)
  const [allowInsecureHttp, setAllowInsecureHttp] = React.useState(true)
  const [openRegistration, setOpenRegistration] = React.useState(false)
  const [twoFactorEnabled, setTwoFactorEnabled] = React.useState(false)
  const [turnstileEnabled, setTurnstileEnabled] = React.useState(false)
  const [catchAllEnabled, setCatchAllEnabled] = React.useState(false)
  const [mailAutoRefresh, setMailAutoRefresh] = React.useState(true)
  const [userMailboxApplyEnabled, setUserMailboxApplyEnabled] = React.useState(false)
  const [userMailboxDomainIds, setUserMailboxDomainIds] = React.useState<string[]>([])
  React.useEffect(() => {
    if (!settings) return
    setSmtpRequireTls(settings.smtpRequireTls)
    setAllowInsecureHttp(settings.allowInsecureHttp)
    setOpenRegistration(settings.openRegistration)
    setTwoFactorEnabled(settings.twoFactorEnabled)
    setTurnstileEnabled(settings.turnstileEnabled)
    setCatchAllEnabled(settings.catchAllEnabled)
    setMailAutoRefresh(settings.mailAutoRefresh)
    setUserMailboxApplyEnabled(settings.userMailboxApplyEnabled)
    setUserMailboxDomainIds(settings.userMailboxDomainIds || [])
  }, [settings])
  const save = useMutation({
    mutationFn: (form: FormData) => api.updateSystemSettings({
      publicHostname: fieldValue(form, "publicHostname", settings?.publicHostname || ""),
      publicBaseUrl: fieldValue(form, "publicBaseUrl", settings?.publicBaseUrl || ""),
      smtpHost: fieldValue(form, "smtpHost", settings?.smtpHost || ""),
      smtpPort: fieldValue(form, "smtpPort", settings?.smtpPort || "25"),
      smtpUsername: fieldValue(form, "smtpUsername", settings?.smtpUsername || ""),
      smtpPassword: fieldValue(form, "smtpPassword", ""),
      smtpRequireTls,
      maildirRoot: fieldValue(form, "maildirRoot", settings?.maildirRoot || ""),
      maildirScanSeconds: fieldNumber(form, "maildirScanSeconds", settings?.maildirScanSeconds || 30),
      sessionTtlHours: fieldNumber(form, "sessionTtlHours", settings?.sessionTtlHours || 168),
      allowInsecureHttp,
      openRegistration,
      twoFactorEnabled,
      turnstileEnabled,
      turnstileSiteKey: fieldValue(form, "turnstileSiteKey", settings?.turnstileSiteKey || ""),
      turnstileSecretKey: fieldValue(form, "turnstileSecretKey", ""),
      catchAllEnabled,
      mailAutoRefresh,
      mailRefreshSeconds: fieldNumber(form, "mailRefreshSeconds", settings?.mailRefreshSeconds || 30),
      userMailboxApplyEnabled,
      userMailboxDomainIds,
      reservedMailboxPrefixes: fieldValue(form, "reservedMailboxPrefixes", settings?.reservedMailboxPrefixes || ""),
    }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "settings"] })
      qc.invalidateQueries({ queryKey: ["dns-records"] })
      qc.invalidateQueries({ queryKey: ["public-settings"] })
      toast({ title: "系统设置已保存" })
    },
    onError: (e) => toast({ title: "保存失败", description: e.message }),
  })
  const formKey = settings ? [
    settings.publicHostname,
    settings.publicBaseUrl,
    settings.smtpHost,
    settings.smtpPort,
    settings.smtpUsername,
    settings.smtpPasswordSet,
    settings.smtpRequireTls,
    settings.maildirRoot,
    settings.maildirScanSeconds,
    settings.sessionTtlHours,
    settings.allowInsecureHttp,
    settings.openRegistration,
    settings.twoFactorEnabled,
    settings.turnstileEnabled,
    settings.turnstileSiteKey,
    settings.turnstileSecretSet,
    settings.catchAllEnabled,
    settings.mailAutoRefresh,
    settings.mailRefreshSeconds,
    settings.userMailboxApplyEnabled,
    (settings.userMailboxDomainIds || []).join(","),
    settings.reservedMailboxPrefixes,
  ].join("|") : "loading"
  const tabs: { key: typeof settingsTab; label: string }[] = [
    { key: "base", label: "基础" },
    { key: "smtp", label: "SMTP" },
    { key: "storage", label: "存储" },
    { key: "mail", label: "邮件" },
    { key: "templates", label: "模板" },
    { key: "security", label: "安全" },
  ]
  return (
    <form key={formKey} onSubmit={(event) => { event.preventDefault(); save.mutate(new FormData(event.currentTarget)) }} className="space-y-6">
      <div className="flex flex-wrap gap-2 rounded-lg border bg-card p-2">
        {tabs.map((tab) => (
          <Button key={tab.key} type="button" variant={settingsTab === tab.key ? "default" : "ghost"} size="sm" onClick={() => setSettingsTab(tab.key)}>
            {tab.label}
          </Button>
        ))}
      </div>

      {settingsTab === "base" && <Card>
        <CardHeader><CardTitle>基础设置</CardTitle></CardHeader>
        <CardContent className="grid gap-4 md:grid-cols-2">
          <Field name="publicHostname" label="公网主机名" defaultValue={settings?.publicHostname || ""} placeholder="mail.example.com" />
          <Field name="publicBaseUrl" label="访问地址" defaultValue={settings?.publicBaseUrl || ""} placeholder="https://mail.example.com" required={false} />
          <Field name="sessionTtlHours" label="登录有效期小时" type="number" defaultValue={String(settings?.sessionTtlHours || 168)} />
          <Field name="maildirScanSeconds" label="Maildir 扫描秒数" type="number" defaultValue={String(settings?.maildirScanSeconds || 30)} />
          <SwitchRow label="允许 HTTP 调试" checked={allowInsecureHttp} onCheckedChange={setAllowInsecureHttp} className="md:col-span-2" />
        </CardContent>
      </Card>}

      {settingsTab === "smtp" && <Card>
        <CardHeader>
          <div className="flex items-center justify-between gap-3">
            <div>
              <CardTitle>发信通道</CardTitle>
            </div>
            <TestSMTPDialog disabled={!settings} />
          </div>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="rounded-lg border bg-muted/30 p-4 text-sm text-muted-foreground">
            <div className="font-medium text-foreground">当前默认：内置 Postfix</div>
            <div>Host 填 127.0.0.1、端口 25、用户名/密码留空、强制 TLS 关闭。这里的“强制 TLS”仅用于外部 SMTP 中继（587 STARTTLS 或 465 TLS）。</div>
          </div>
          <div className="grid gap-4 md:grid-cols-2">
            <Field name="smtpHost" label="发信主机" defaultValue={settings?.smtpHost || ""} placeholder="127.0.0.1" required={false} />
            <Field name="smtpPort" label="发信端口" defaultValue={settings?.smtpPort || "25"} />
            <Field name="smtpUsername" label="中继用户名（内置 Postfix 留空）" defaultValue={settings?.smtpUsername || ""} required={false} />
            <Field name="smtpPassword" label={settings?.smtpPasswordSet ? "中继密码（留空不变）" : "中继密码（内置 Postfix 留空）"} type="password" required={false} />
            <SwitchRow label="外部中继强制 TLS" checked={smtpRequireTls} onCheckedChange={setSmtpRequireTls} className="md:col-span-2" />
          </div>
        </CardContent>
      </Card>}

      {settingsTab === "storage" && <Card>
        <CardHeader><CardTitle>存储设置</CardTitle></CardHeader>
        <CardContent>
          <Field name="maildirRoot" label="Maildir 根目录" defaultValue={settings?.maildirRoot || ""} required={false} />
        </CardContent>
      </Card>}

      {settingsTab === "mail" && <Card>
        <CardHeader><CardTitle>邮件设置</CardTitle></CardHeader>
        <CardContent className="space-y-5">
          <SwitchRow label="无人收件" checked={catchAllEnabled} onCheckedChange={setCatchAllEnabled} />
          <Separator />
          <SwitchRow label="用户自助申请邮箱" checked={userMailboxApplyEnabled} onCheckedChange={setUserMailboxApplyEnabled} />
          {userMailboxApplyEnabled && (
            <div className="space-y-5 border-t pt-5">
              <div className="space-y-3">
                <Label>开放域名</Label>
                <div className="grid gap-2 md:grid-cols-2">
                  {domains.map((domain) => {
                    const checked = userMailboxDomainIds.includes(domain.id)
                    const disabled = domain.status !== "active"
                    return (
                      <label key={domain.id} className={cn("flex min-h-11 items-center gap-3 rounded-md border px-3 py-2", disabled && "cursor-not-allowed opacity-50")}>
                        <Checkbox
                          checked={checked}
                          disabled={disabled}
                          onCheckedChange={(value) => setUserMailboxDomainIds((items) => value === true ? Array.from(new Set([...items, domain.id])) : items.filter((id) => id !== domain.id))}
                        />
                        <span className="text-sm font-medium">{domain.name}</span>
                      </label>
                    )
                  })}
                </div>
                {domains.length === 0 && <Empty text="暂无域名" />}
              </div>
              <div className="space-y-2">
                <Label>禁止前缀</Label>
                <Textarea name="reservedMailboxPrefixes" defaultValue={settings?.reservedMailboxPrefixes || ""} className="min-h-28 font-mono text-sm" />
              </div>
            </div>
          )}
          <Separator />
          <SwitchRow label="自动刷新" checked={mailAutoRefresh} onCheckedChange={setMailAutoRefresh} />
          {mailAutoRefresh && (
            <div className="border-t pt-5">
              <Field name="mailRefreshSeconds" label="刷新间隔秒数" type="number" min={5} defaultValue={String(settings?.mailRefreshSeconds || 30)} />
            </div>
          )}
        </CardContent>
      </Card>}

      {settingsTab === "templates" && <MailTemplatesPanel templates={templates.data?.items || []} loading={templates.isLoading} />}

      {settingsTab === "security" && <Card>
        <CardHeader><CardTitle>安全设置</CardTitle></CardHeader>
        <CardContent className="space-y-5">
          <SwitchRow label="开放注册" checked={openRegistration} onCheckedChange={setOpenRegistration} />
          <Separator />
          <SwitchRow label="双因素认证 (2FA)" checked={twoFactorEnabled} onCheckedChange={setTwoFactorEnabled} />
          <Separator />
          <SwitchRow label="Turnstile" checked={turnstileEnabled} onCheckedChange={setTurnstileEnabled} />
          {turnstileEnabled && (
            <div className="grid gap-4 border-t pt-5 md:grid-cols-2">
              <Field name="turnstileSiteKey" label="Site Key" defaultValue={settings?.turnstileSiteKey || ""} required />
              <Field name="turnstileSecretKey" label={settings?.turnstileSecretSet ? "Secret Key（留空不变）" : "Secret Key"} type="password" required={!settings?.turnstileSecretSet} />
            </div>
          )}
        </CardContent>
      </Card>}

      <div className="flex justify-end">
        <Button disabled={save.isPending || !settings}>{save.isPending ? "保存中..." : "保存设置"}</Button>
      </div>
    </form>
  )
}

function TestSMTPDialog({ disabled }: { disabled?: boolean }) {
  const { toast } = useToast()
  const [open, setOpen] = React.useState(false)
  const test = useMutation({
    mutationFn: (form: FormData) => api.testSmtp(String(form.get("to") || "")),
    onSuccess: () => {
      setOpen(false)
      toast({ title: "测试邮件已发送" })
    },
    onError: (e) => toast({ title: "发送失败", description: e.message }),
  })
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button type="button" variant="outline" size="sm" disabled={disabled}>测试发送</Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader><DialogTitle>SMTP 测试发送</DialogTitle></DialogHeader>
        <form className="space-y-4" onSubmit={(event) => { event.preventDefault(); test.mutate(new FormData(event.currentTarget)) }}>
          <Field name="to" label="收件邮箱" type="email" placeholder="test@example.com" />
          <DialogFooter><Button disabled={test.isPending}>{test.isPending ? "发送中..." : "发送"}</Button></DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function MailTemplatesPanel({ templates, loading }: { templates: MailTemplate[]; loading: boolean }) {
  const qc = useQueryClient()
  const { toast } = useToast()
  const [selectedKey, setSelectedKey] = React.useState("")
  const selected = templates.find((template) => template.key === selectedKey) || templates[0]
  const [subject, setSubject] = React.useState("")
  const [bodyText, setBodyText] = React.useState("")
  const [bodyHtml, setBodyHtml] = React.useState("")
  React.useEffect(() => {
    if (!selectedKey && templates[0]) setSelectedKey(templates[0].key)
  }, [selectedKey, templates])
  React.useEffect(() => {
    if (!selected) return
    setSubject(selected.subject)
    setBodyText(selected.bodyText)
    setBodyHtml(selected.bodyHtml)
  }, [selected])
  const save = useMutation({
    mutationFn: () => api.updateMailTemplate(selected!.key, { subject, bodyText, bodyHtml }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "mail-templates"] })
      toast({ title: "模板已保存" })
    },
    onError: (e) => toast({ title: "保存失败", description: e.message }),
  })
  const reset = useMutation({
    mutationFn: () => api.resetMailTemplate(selected!.key),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "mail-templates"] })
      toast({ title: "模板已恢复" })
    },
    onError: (e) => toast({ title: "恢复失败", description: e.message }),
  })
  if (loading) return <Card><CardContent className="p-6"><Empty text="加载中..." /></CardContent></Card>
  if (!selected) return <Card><CardContent className="p-6"><Empty text="暂无模板" /></CardContent></Card>
  return (
    <Card>
      <CardHeader><CardTitle>邮件模板</CardTitle></CardHeader>
      <CardContent className="space-y-4">
        <SelectField label="模板" value={selected.key} onValueChange={setSelectedKey} items={templates.map((template) => [template.key, template.name])} />
        <div className="space-y-2">
          <Label>主题</Label>
          <Input value={subject} onChange={(event) => setSubject(event.target.value)} />
        </div>
        <div className="grid gap-4 lg:grid-cols-2">
          <div className="space-y-2">
            <Label>纯文本</Label>
            <Textarea value={bodyText} onChange={(event) => setBodyText(event.target.value)} className="min-h-64 font-mono text-sm" />
          </div>
          <div className="space-y-2">
            <Label>HTML</Label>
            <Textarea value={bodyHtml} onChange={(event) => setBodyHtml(event.target.value)} className="min-h-64 font-mono text-sm" />
          </div>
        </div>
        <div className="flex justify-end gap-2">
          <Button type="button" variant="outline" disabled={reset.isPending || save.isPending} onClick={() => reset.mutate()}>
            {reset.isPending ? "恢复中..." : "恢复默认"}
          </Button>
          <Button type="button" disabled={save.isPending || reset.isPending} onClick={() => save.mutate()}>
            {save.isPending ? "保存中..." : "保存模板"}
          </Button>
        </div>
      </CardContent>
    </Card>
  )
}

function AdminMessageDialog({ message, loading, open, onOpenChange }: { message?: MailMessage; loading: boolean; open: boolean; onOpenChange: (open: boolean) => void }) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[86vh] overflow-y-auto sm:max-w-4xl">
        <DialogHeader><DialogTitle>{loading ? "加载中..." : message?.subject || "邮件详情"}</DialogTitle></DialogHeader>
        {message && (
          <div className="space-y-5">
            <div className="grid gap-3 rounded-lg border p-4 text-sm md:grid-cols-2">
              <MessageMeta label="所属邮箱" value={message.mailboxAddress || message.recipientAddress || ""} />
              <MessageMeta label="所属用户" value={message.ownerEmail || ""} />
              <MessageMeta label="发件人" value={adminSenderTitle(message)} />
              <MessageMeta label="收件人" value={message.recipientAddress || message.to?.join(", ") || ""} />
              <MessageMeta label="文件夹" value={folderName(message.folder)} />
              <MessageMeta label="时间" value={formatDate(message.receivedAt)} />
            </div>
            <div className="mail-html prose max-w-none rounded-lg border p-5 text-sm leading-7" dangerouslySetInnerHTML={{ __html: DOMPurify.sanitize(message.bodyHtml || `<pre>${escapeHtml(message.bodyText || message.snippet || "")}</pre>`) }} />
            {message.attachments && message.attachments.length > 0 && (
              <div className="rounded-lg border p-4">
                <div className="mb-3 font-medium">附件</div>
                <div className="space-y-2">
                  {message.attachments.map((attachment) => (
                    <a className="flex items-center justify-between rounded-md border p-3 text-sm hover:bg-accent" href={`/api/admin/attachments/${attachment.id}`} key={attachment.id}>
                      <span className="truncate">{attachment.filename}</span>
                      <span className="text-muted-foreground">{formatBytes(attachment.sizeBytes)}</span>
                    </a>
                  ))}
                </div>
              </div>
            )}
          </div>
        )}
      </DialogContent>
    </Dialog>
  )
}

function MessageMeta({ label, value }: { label: string; value: string }) {
  return <div className="min-w-0"><div className="text-xs text-muted-foreground">{label}</div><div className="truncate font-medium">{value || "-"}</div></div>
}

function folderName(folder: string) {
  const labels: Record<string, string> = { Inbox: "收件箱", Sent: "已发送", Drafts: "草稿箱", Archive: "归档", Spam: "垃圾邮件", Trash: "回收站", Unregistered: "未注册收件" }
  return labels[folder] || folder
}

function escapeHtml(value: string) {
  return value.replace(/[&<>"']/g, (char) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[char] || char)
}

function adminSenderDisplayName(message: MailMessage) {
  const fromName = decodeMimeHeader(message.fromName?.trim() || "")
  if (fromName) return fromName
  const text = decodeMimeHeader(message.from.trim())
  const namedAddress = text.match(/^"?([^"<]+?)"?\s*<[^>]+>$/)
  const name = namedAddress?.[1]?.trim()
  if (name) return name
  const address = text.match(/<([^>]+)>/)?.[1]?.trim() || text
  return address.split("@")[0]?.trim() || text || "未知发件人"
}

function adminSenderTitle(message: MailMessage) {
  const name = decodeMimeHeader(message.fromName?.trim() || "")
  const from = decodeMimeHeader(message.from)
  return name ? `${name} <${from}>` : from
}

function Stat({ icon, label, value }: { icon: React.ReactNode; label: string; value: React.ReactNode }) {
  return <Card><CardContent className="flex items-center gap-3 p-4 sm:gap-4 sm:p-5"><div className="grid h-9 w-9 shrink-0 place-items-center rounded-lg bg-muted text-foreground sm:h-10 sm:w-10">{icon}</div><div className="min-w-0"><div className="truncate text-xl font-semibold tracking-tight sm:text-2xl">{value}</div><div className="text-xs text-muted-foreground">{label}</div></div></CardContent></Card>
}
function InfoBox({ label, value }: { label: string; value: React.ReactNode }) { return <div className="rounded-lg border p-4"><div className="text-xl font-semibold tracking-tight sm:text-2xl">{value}</div><div className="text-xs text-muted-foreground">{label}</div></div> }
function Empty({ text }: { text: string }) { return <div className="rounded-lg border border-dashed p-8 text-center text-sm text-muted-foreground">{text}</div> }
function DomainBadgeRow({ domain }: { domain: Domain }) { return <div className="flex items-center justify-between rounded-lg border p-3"><span className="font-medium">{domain.name}</span><Badge variant={domain.dnsStatus === "ok" ? "default" : "secondary"}>{domain.dnsStatus === "ok" ? "正常" : domain.dnsStatus}</Badge></div> }
function invalidateAdmin(qc: ReturnType<typeof useQueryClient>) { qc.invalidateQueries({ queryKey: ["admin"] }); qc.invalidateQueries({ queryKey: ["mailboxes"] }); qc.invalidateQueries({ queryKey: ["me"] }) }

function UserMailboxCell({ user }: { user: AdminUser }) {
  const mailboxes = user.mailboxes || []
  if (mailboxes.length === 0) return <span className="text-muted-foreground">未绑定</span>
  return (
    <div className="flex max-w-md flex-wrap gap-1">
      {mailboxes.slice(0, 2).map((mailbox) => <Badge key={mailbox} variant="outline" className="font-normal">{mailbox}</Badge>)}
      {mailboxes.length > 2 && <Badge variant="secondary">+{mailboxes.length - 2}</Badge>}
    </div>
  )
}

function UserActions({ user, onDelete }: { user: AdminUser; onDelete: () => void }) {
  const qc = useQueryClient()
  const { toast } = useToast()
  const [editOpen, setEditOpen] = React.useState(false)
  const [passwordOpen, setPasswordOpen] = React.useState(false)
  const update = useMutation({
    mutationFn: (payload: { displayName: string; role: "admin" | "user"; disabled: boolean }) => api.updateUser(user.id, payload),
    onSuccess: () => { invalidateAdmin(qc); toast({ title: "用户已更新" }) },
    onError: (e) => toast({ title: "更新失败", description: e.message }),
  })
  function quickPatch(patch: Partial<{ role: "admin" | "user"; disabled: boolean }>) {
    update.mutate({ displayName: user.displayName, role: patch.role || user.role, disabled: patch.disabled ?? user.disabled })
  }
  return <><DropdownMenu><DropdownMenuTrigger asChild><Button variant="ghost" size="icon"><MoreHorizontal className="h-4 w-4" /></Button></DropdownMenuTrigger><DropdownMenuContent align="end"><DropdownMenuItem onSelect={() => setEditOpen(true)}>编辑用户</DropdownMenuItem><DropdownMenuItem onSelect={() => setPasswordOpen(true)}>重置密码</DropdownMenuItem><DropdownMenuSeparator /><DropdownMenuItem onSelect={() => quickPatch({ disabled: !user.disabled })}>{user.disabled ? "启用用户" : "停用用户"}</DropdownMenuItem><DropdownMenuItem onSelect={() => quickPatch({ role: user.role === "admin" ? "user" : "admin" })}>{user.role === "admin" ? "设为普通用户" : "设为管理员"}</DropdownMenuItem><DropdownMenuSeparator /><DropdownMenuItem className="text-destructive" onSelect={onDelete}>删除用户</DropdownMenuItem></DropdownMenuContent></DropdownMenu><EditUserDialog user={user} open={editOpen} onOpenChange={setEditOpen} /><ResetPasswordDialog user={user} open={passwordOpen} onOpenChange={setPasswordOpen} /></>
}

function CreateUserDialog() {
  const qc = useQueryClient()
  const { toast } = useToast()
  const [open, setOpen] = React.useState(false)
  const [role, setRole] = React.useState<"admin" | "user">("user")
  const [status, setStatus] = React.useState("active")
  const create = useMutation({
    mutationFn: (form: FormData) => api.createUser({ email: String(form.get("email") || ""), displayName: String(form.get("displayName") || ""), password: String(form.get("password") || ""), role, disabled: status === "disabled" }),
    onSuccess: () => { invalidateAdmin(qc); setOpen(false); toast({ title: "用户已创建" }) },
    onError: (e) => toast({ title: "创建失败", description: e.message }),
  })
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild><Button size="sm"><Plus className="h-4 w-4" />用户</Button></DialogTrigger>
      <DialogContent>
        <DialogHeader><DialogTitle>创建用户</DialogTitle></DialogHeader>
        <form className="space-y-4" onSubmit={(event) => { event.preventDefault(); create.mutate(new FormData(event.currentTarget)) }}>
          <Field name="email" label="登录邮箱" type="email" placeholder="user@example.com" />
          <Field name="displayName" label="显示名称" placeholder="用户名称" />
          <Field name="password" label="初始密码" type="password" minLength={8} />
          <div className="grid grid-cols-2 gap-3">
            <SelectField label="角色" value={role} onValueChange={(value) => setRole(value as "admin" | "user")} items={[["user", "普通用户"], ["admin", "管理员"]]} />
            <SelectField label="状态" value={status} onValueChange={setStatus} items={[["active", "正常"], ["disabled", "停用"]]} />
          </div>
          <DialogFooter><Button disabled={create.isPending}>{create.isPending ? "创建中..." : "创建"}</Button></DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function MailboxActions({ mailbox, users, onDelete }: { mailbox: MailboxType; users: AdminUser[]; onDelete: () => void }) {
  const [open, setOpen] = React.useState(false)
  return <><DropdownMenu><DropdownMenuTrigger asChild><Button variant="ghost" size="icon"><MoreHorizontal className="h-4 w-4" /></Button></DropdownMenuTrigger><DropdownMenuContent align="end"><DropdownMenuItem onSelect={() => setOpen(true)}>编辑邮箱</DropdownMenuItem><DropdownMenuSeparator /><DropdownMenuItem className="text-destructive" onSelect={onDelete}>删除邮箱</DropdownMenuItem></DropdownMenuContent></DropdownMenu><EditMailboxDialog mailbox={mailbox} users={users} open={open} onOpenChange={setOpen} /></>
}

function AliasActions({ alias, onToggle, onDelete }: { alias: Alias; onToggle: () => void; onDelete: () => void }) {
  return <DropdownMenu><DropdownMenuTrigger asChild><Button variant="ghost" size="icon"><MoreHorizontal className="h-4 w-4" /></Button></DropdownMenuTrigger><DropdownMenuContent align="end"><DropdownMenuItem onSelect={onToggle}>{alias.enabled ? "停用" : "启用"}</DropdownMenuItem><DropdownMenuSeparator /><DropdownMenuItem className="text-destructive" onSelect={onDelete}>删除别名</DropdownMenuItem></DropdownMenuContent></DropdownMenu>
}

function EditUserDialog({ user, open, onOpenChange }: { user: AdminUser; open: boolean; onOpenChange: (open: boolean) => void }) {
  const qc = useQueryClient(); const { toast } = useToast(); const [role, setRole] = React.useState(user.role); const [disabled, setDisabled] = React.useState(user.disabled ? "disabled" : "active")
  React.useEffect(() => { setRole(user.role); setDisabled(user.disabled ? "disabled" : "active") }, [user, open])
  const mut = useMutation({ mutationFn: (form: FormData) => api.updateUser(user.id, { displayName: String(form.get("displayName") || ""), role, disabled: disabled === "disabled" }), onSuccess: () => { invalidateAdmin(qc); onOpenChange(false); toast({ title: "用户已更新" }) }, onError: (e) => toast({ title: "更新失败", description: e.message }) })
  return <Dialog open={open} onOpenChange={onOpenChange}><DialogContent><DialogHeader><DialogTitle>编辑用户</DialogTitle></DialogHeader><form className="space-y-4" onSubmit={(e) => { e.preventDefault(); mut.mutate(new FormData(e.currentTarget)) }}><Field name="email" label="登录邮箱" value={user.email} readOnly /><Field name="displayName" label="显示名称" defaultValue={user.displayName} /><div className="grid grid-cols-2 gap-3"><SelectField label="角色" value={role} onValueChange={(value) => setRole(value as "admin" | "user")} items={[['user','普通用户'],['admin','管理员']]} /><SelectField label="状态" value={disabled} onValueChange={setDisabled} items={[['active','正常'],['disabled','停用']]} /></div><DialogFooter><Button disabled={mut.isPending}>{mut.isPending ? "保存中..." : "保存"}</Button></DialogFooter></form></DialogContent></Dialog>
}

function ResetPasswordDialog({ user, open, onOpenChange }: { user: AdminUser; open: boolean; onOpenChange: (open: boolean) => void }) {
  const { toast } = useToast(); const mut = useMutation({ mutationFn: (form: FormData) => api.resetUserPassword(user.id, String(form.get("password") || "")), onSuccess: () => { onOpenChange(false); toast({ title: "密码已重置" }) }, onError: (e) => toast({ title: "重置失败", description: e.message }) })
  return <Dialog open={open} onOpenChange={onOpenChange}><DialogContent><DialogHeader><DialogTitle>重置密码</DialogTitle></DialogHeader><form className="space-y-4" onSubmit={(e) => { e.preventDefault(); mut.mutate(new FormData(e.currentTarget)); e.currentTarget.reset() }}><Field name="email" label="用户" value={user.email} readOnly /><Field name="password" label="新密码" type="password" minLength={8} /><DialogFooter><Button disabled={mut.isPending}>{mut.isPending ? "重置中..." : "重置"}</Button></DialogFooter></form></DialogContent></Dialog>
}

function EditMailboxDialog({ mailbox, users, open, onOpenChange }: { mailbox: MailboxType; users: AdminUser[]; open: boolean; onOpenChange: (open: boolean) => void }) {
  const qc = useQueryClient(); const { toast } = useToast(); const [userId, setUserId] = React.useState(mailbox.userId); const [status, setStatus] = React.useState(mailbox.status)
  React.useEffect(() => { setUserId(mailbox.userId); setStatus(mailbox.status) }, [mailbox, open])
  const mut = useMutation({ mutationFn: (form: FormData) => api.updateMailbox(mailbox.id, { userId, displayName: String(form.get("displayName") || ""), quotaMb: Number(form.get("quotaMb") || 1024), status }), onSuccess: () => { invalidateAdmin(qc); onOpenChange(false); toast({ title: "邮箱已更新" }) }, onError: (e) => toast({ title: "更新失败", description: e.message }) })
  return <Dialog open={open} onOpenChange={onOpenChange}><DialogContent><DialogHeader><DialogTitle>编辑邮箱</DialogTitle></DialogHeader><form className="space-y-4" onSubmit={(e) => { e.preventDefault(); mut.mutate(new FormData(e.currentTarget)) }}><Field name="address" label="邮箱地址" value={mailbox.address} readOnly /><SelectField label="归属用户" value={userId} onValueChange={setUserId} items={users.filter((u) => !u.disabled).map((u) => [u.id, u.email])} /><div className="grid grid-cols-2 gap-3"><Field name="displayName" label="显示名称" defaultValue={mailbox.displayName} /><Field name="quotaMb" label="配额 MB" type="number" defaultValue={String(mailbox.quotaMb)} /></div><SelectField label="状态" value={status} onValueChange={setStatus} items={[['active','启用'],['disabled','停用']]} /><DialogFooter><Button disabled={mut.isPending}>{mut.isPending ? "保存中..." : "保存"}</Button></DialogFooter></form></DialogContent></Dialog>
}

function CreateDomainDialog() {
  const qc = useQueryClient(); const { toast } = useToast(); const [open, setOpen] = React.useState(false)
  const mut = useMutation({ mutationFn: (form: FormData) => api.createDomain(String(form.get("name"))), onSuccess: () => { invalidateAdmin(qc); setOpen(false); toast({ title: "域名已创建" }) }, onError: (e) => toast({ title: "创建失败", description: e.message }) })
  return <Dialog open={open} onOpenChange={setOpen}><DialogTrigger asChild><Button variant="outline"><Plus className="h-4 w-4" />域名</Button></DialogTrigger><DialogContent><DialogHeader><DialogTitle>添加域名</DialogTitle></DialogHeader><form className="space-y-4" onSubmit={(e) => { e.preventDefault(); mut.mutate(new FormData(e.currentTarget)) }}><Field name="name" label="域名" placeholder="example.com" /><DialogFooter><Button disabled={mut.isPending}>创建</Button></DialogFooter></form></DialogContent></Dialog>
}

function CreateMailboxDialog({ domains, users }: { domains: Domain[]; users: AdminUser[] }) {
  const qc = useQueryClient(); const { toast } = useToast(); const [open, setOpen] = React.useState(false); const [domainId, setDomainId] = React.useState(""); const [role, setRole] = React.useState("user"); const [ownerMode, setOwnerMode] = React.useState("new"); const [userId, setUserId] = React.useState("")
  React.useEffect(() => { if (!domainId && domains[0]) setDomainId(domains[0].id); if (!userId && users[0]) setUserId(users[0].id) }, [domains, domainId, users, userId])
  const mut = useMutation({ mutationFn: (form: FormData) => api.createMailbox({ domainId, localPart: String(form.get("localPart")), displayName: String(form.get("displayName")), password: String(form.get("password")), quotaMb: Number(form.get("quotaMb") || 1024), role: role as "admin" | "user", ownerEmail: String(form.get("ownerEmail") || ""), userId: ownerMode === "existing" ? userId : "" }), onSuccess: () => { invalidateAdmin(qc); setOpen(false); toast({ title: "邮箱已创建" }) }, onError: (e) => toast({ title: "创建失败", description: e.message }) })
  return <Dialog open={open} onOpenChange={setOpen}><DialogTrigger asChild><Button><Plus className="h-4 w-4" />邮箱</Button></DialogTrigger><DialogContent><DialogHeader><DialogTitle>创建邮箱账号</DialogTitle></DialogHeader><form className="space-y-4" onSubmit={(e) => { e.preventDefault(); mut.mutate(new FormData(e.currentTarget)) }}><DomainSelect domains={domains} value={domainId} onChange={setDomainId} /><div className="grid grid-cols-2 gap-3"><Field name="localPart" label="账号" placeholder="alice" /><Field name="displayName" label="显示名" placeholder="Alice" /></div><SelectField label="归属方式" value={ownerMode} onValueChange={setOwnerMode} items={[['new','新建/按邮箱匹配用户'],['existing','追加到已有用户']]} />{ownerMode === "existing" ? <SelectField label="已有用户" value={userId} onValueChange={setUserId} items={users.filter((u) => !u.disabled).map((u) => [u.id, u.email])} /> : <Field name="ownerEmail" label="归属用户邮箱" placeholder="留空则使用新邮箱" required={false} />}<div className="grid grid-cols-2 gap-3"><Field name="password" label="密码" type="password" placeholder="至少 8 位" /><Field name="quotaMb" label="配额 MB" type="number" defaultValue="1024" /></div><SelectField label="角色" value={role} onValueChange={setRole} items={[['user','普通用户'],['admin','管理员']]} /><DialogFooter><Button disabled={mut.isPending || !domainId}>创建</Button></DialogFooter></form></DialogContent></Dialog>
}

function CreateAliasDialog({ domains }: { domains: Domain[] }) {
  const qc = useQueryClient(); const { toast } = useToast(); const [open, setOpen] = React.useState(false); const [domainId, setDomainId] = React.useState("")
  React.useEffect(() => { if (!domainId && domains[0]) setDomainId(domains[0].id) }, [domains, domainId])
  const mut = useMutation({ mutationFn: (form: FormData) => api.createAlias({ domainId, source: String(form.get("source")), destination: String(form.get("destination")), enabled: true }), onSuccess: () => { invalidateAdmin(qc); setOpen(false); toast({ title: "别名已创建" }) }, onError: (e) => toast({ title: "创建失败", description: e.message }) })
  return <Dialog open={open} onOpenChange={setOpen}><DialogTrigger asChild><Button variant="outline"><Plus className="h-4 w-4" />别名</Button></DialogTrigger><DialogContent><DialogHeader><DialogTitle>创建别名/转发</DialogTitle></DialogHeader><form className="space-y-4" onSubmit={(e) => { e.preventDefault(); mut.mutate(new FormData(e.currentTarget)) }}><DomainSelect domains={domains} value={domainId} onChange={setDomainId} /><Field name="source" label="来源" placeholder="sales 或 sales@example.com" /><Field name="destination" label="目标邮箱" placeholder="alice@example.com" /><DialogFooter><Button disabled={mut.isPending || !domainId}>创建</Button></DialogFooter></form></DialogContent></Dialog>
}

function DNSPanel({ domain, embedded = false }: { domain?: Domain; embedded?: boolean }) {
  const { toast } = useToast(); const qc = useQueryClient(); const records = useQuery({ queryKey: ["dns-records", domain?.id], queryFn: () => api.dnsRecords(domain!.id), enabled: !!domain })
  const check = useMutation({ mutationFn: () => api.checkDns(domain!.id), onSuccess: (res) => { qc.invalidateQueries({ queryKey: ["admin", "domains"] }); toast({ title: res.status === "ok" ? "DNS 检测通过" : "DNS 检测未通过", description: Object.values(res.checks).map((c) => c.message).join("；") }) } })
  if (!domain) return <Card><CardContent className="p-6 text-muted-foreground">请选择域名</CardContent></Card>
  const content = <>
    <p className="mb-3 text-sm text-muted-foreground">以下为需要在域名 DNS 管理中添加的记录：</p>
    <div className="space-y-3">{records.data?.items.map((r) => <DNSRecordRow key={`${r.type}-${r.name}`} record={r} />)}</div>
    {check.data && <>
      <Separator className="my-4" />
      <div className="flex items-center gap-2 text-sm font-medium text-muted-foreground"><CheckCircle2 className="h-4 w-4" />检测结果</div>
      <div className="mt-2 space-y-2">{Object.entries(check.data.checks).map(([k, v]) => <div key={k} className="flex items-center gap-2 text-sm"><CheckCircle2 className={`h-4 w-4 shrink-0 ${v.ok ? "text-green-600" : "text-destructive"}`} /><span className="font-medium">{k.toUpperCase()}:</span> {v.message}</div>)}</div>
    </>}</>
  const header = <div className="flex items-center justify-between"><CardTitle>DNS 记录</CardTitle><Button variant="outline" size="sm" onClick={() => check.mutate()} disabled={check.isPending}><RefreshCcw className="h-4 w-4" />检测</Button></div>
  if (embedded) return <div className="space-y-4"><div className="flex items-center justify-between"><div className="font-medium">DNS 记录</div><Button variant="outline" size="sm" onClick={() => check.mutate()} disabled={check.isPending}><RefreshCcw className="h-4 w-4" />检测</Button></div>{content}</div>
  return <Card><CardHeader>{header}</CardHeader><CardContent>{content}</CardContent></Card>
}

const dnsDescriptions: Record<string, string> = {
  MX: "指定收件服务器。把邮件投递到该地址指向的服务器。",
  TXT: "", // 具体含义根据内容区分
}

function dnsDescription(record: DNSRecord): string {
  if (record.type === "TXT" && record.name.startsWith("_dmarc")) return "声明域名的 DMARC 策略（如何处理未通过 SPF/DKIM 验证的邮件）。"
  if (record.type === "TXT" && record.value.includes("DKIM1")) return "DKIM 公钥。收件服务器用此密钥验证邮件是否由你发出。"
  if (record.type === "TXT" && record.value.includes("spf1")) return "声明哪些服务器有权使用你的域名发件，防止伪造。"
  if (record.type === "MX") return `确保 ${record.name} 的 A 记录已指向你的服务器 IP，邮件才能到达。`
  return ""
}

function DNSRecordRow({ record }: { record: DNSRecord }) {
  const { toast } = useToast(); const text = `${record.type} ${record.name} ${record.value}`
  const desc = dnsDescription(record)
  return <div className="rounded-lg border bg-card p-3">
    <div className="mb-2 flex items-center justify-between">
      <Badge variant="outline" className="font-mono">{record.type}</Badge>
      <Button size="sm" variant="ghost" className="h-7 gap-1 text-xs" onClick={() => { navigator.clipboard.writeText(text); toast({ title: "已复制" }) }}><Copy className="h-3.5 w-3.5" />复制</Button>
    </div>
    {desc && <p className="mb-2 text-xs text-muted-foreground">{desc}</p>}
    <div className="break-all font-mono text-xs text-muted-foreground">
      <div><span className="text-foreground">Name:</span> {record.name}</div>
      <div><span className="text-foreground">Value:</span> {record.value}</div>
      <div><span className="text-foreground">TTL:</span> {record.ttl}s</div>
    </div>
  </div>
}

function fieldValue(form: FormData, name: string, fallback: string) {
  const value = form.get(name)
  return value === null ? fallback : String(value)
}
function fieldNumber(form: FormData, name: string, fallback: number) {
  const value = form.get(name)
  if (value === null) return fallback
  const n = Number(value)
  return Number.isFinite(n) && n > 0 ? n : fallback
}
function SwitchRow({ label, checked, onCheckedChange, className = "" }: { label: string; checked: boolean; onCheckedChange: (checked: boolean) => void; className?: string }) {
  return (
    <div className={`flex min-h-14 items-center justify-between gap-4 ${className}`}>
      <Label className="text-base font-medium">{label}</Label>
      <Switch checked={checked} onCheckedChange={onCheckedChange} />
    </div>
  )
}
function Field({ label, required = true, ...props }: React.InputHTMLAttributes<HTMLInputElement> & { label: string }) { return <div className="space-y-2"><Label>{label}</Label><Input required={required} {...props} /></div> }
function SelectField({ label, value, onValueChange, items }: { label: string; value: string; onValueChange: (value: string) => void; items: string[][] }) { return <div className="space-y-2"><Label>{label}</Label><Select value={value} onValueChange={onValueChange}><SelectTrigger><SelectValue /></SelectTrigger><SelectContent>{items.map(([value, label]) => <SelectItem key={value} value={value}>{label}</SelectItem>)}</SelectContent></Select></div> }
function DomainSelect({ domains, value, onChange }: { domains: Domain[]; value: string; onChange: (value: string) => void }) { return <div className="space-y-2"><Label>域名</Label><Select value={value} onValueChange={onChange}><SelectTrigger><SelectValue placeholder="选择域名" /></SelectTrigger><SelectContent>{domains.map((d) => <SelectItem key={d.id} value={d.id}>{d.name}</SelectItem>)}</SelectContent></Select></div> }




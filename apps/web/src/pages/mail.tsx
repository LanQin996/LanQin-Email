import * as React from "react"
import DOMPurify from "dompurify"
import { type InfiniteData, useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useNavigate } from "react-router-dom"
import type { ImperativePanelHandle } from "react-resizable-panels"
import { AlignCenter, AlignLeft, AlignRight, Archive, ArrowLeft, Bold, Calendar, Check, ChevronDown, ChevronsUpDown, Code2, Copy, Ellipsis, Eraser, FileText, Forward, Highlighter, Image, Inbox, IndentDecrease, IndentIncrease, Italic, Link, List, ListOrdered, Mail, MailCheck, Moon, PanelLeftClose, PanelLeftOpen, Paperclip, PencilLine, Plus, Quote, Redo2, RefreshCcw, Reply, Search, Send, Settings, Signature, SlidersHorizontal, Smile, Star, Strikethrough, Sun, Tag, Trash2, Type, Underline, Undo2, X } from "lucide-react"
import { api, Mailbox, MailFolder, MailLabel, MailMessage, SendPayload } from "@/lib/api"
import { cn, formatBytes, formatDate, formatDateTime } from "@/lib/utils"
import { applyTheme, getInitialTheme } from "@/lib/theme"
import { useDisplayMode } from "@/lib/display-mode"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Badge } from "@/components/ui/badge"
import { Checkbox } from "@/components/ui/checkbox"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Label } from "@/components/ui/label"
import { DropdownMenu, DropdownMenuCheckboxItem, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu"
import { ScrollArea } from "@/components/ui/scroll-area"
import { Separator } from "@/components/ui/separator"
import { Skeleton } from "@/components/ui/skeleton"
import { Avatar, AvatarFallback } from "@/components/ui/avatar"
import { ResizableHandle, ResizablePanel, ResizablePanelGroup } from "@/components/ui/resizable"
import { ConfirmDialog } from "@/components/confirm-dialog"
import {
  Sidebar,
  SidebarContent,
  SidebarGroup,
  SidebarGroupContent,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarProvider,
} from "@/components/ui/sidebar"
import { useMe } from "@/hooks/use-me"
import { useToast } from "@/hooks/use-toast"

const folderIcons: Record<string, React.ReactNode> = { inbox: <Inbox className="h-4 w-4" />, sent: <Send className="h-4 w-4" />, archive: <Archive className="h-4 w-4" />, trash: <Trash2 className="h-4 w-4" /> }
const folderLabels: Record<string, string> = {
  Inbox: "收件箱",
  Sent: "已发送",
  Drafts: "草稿箱",
  Archive: "归档",
  Spam: "垃圾邮件",
  Trash: "回收站",
}

type ComposeDraft = { key: string; to?: string; cc?: string; bcc?: string; subject?: string; text?: string }
type MailFilter = "all" | "unread" | "starred" | "attachments"
type MailView = "folder" | "starred" | "label"
type MailListResponse = { items?: MailMessage[]; nextCursor?: string }
type PendingConfirm = { title: string; description?: string; confirmText: string; onConfirm: () => void }
type MailNotificationState = { latestId: string; latestReceivedAt: string }
type MailMenuItem =
  | { type: "starred"; key: string; label: string; icon: React.ReactNode; count: number }
  | { type: "folder"; key: string; folderName: string; label: string; icon: React.ReactNode; count: number }

const filterLabels: Record<MailFilter, string> = {
  all: "全部邮件",
  unread: "未读邮件",
  starred: "星标邮件",
  attachments: "有附件",
}

export function MailPage() {
  const qc = useQueryClient()
  const { toast } = useToast()
  const navigate = useNavigate()
  const me = useMe()
  const [folder, setFolder] = React.useState("Inbox")
  const [mailView, setMailView] = React.useState<MailView>("folder")
  const [selectedLabelId, setSelectedLabelId] = React.useState("")
  const [query, setQuery] = React.useState("")
  const [selectedId, setSelectedId] = React.useState<string | null>(null)
  const [compactSelectedIds, setCompactSelectedIds] = React.useState<string[]>([])
  const [composeOpen, setComposeOpen] = React.useState(false)
  const [composeDraft, setComposeDraft] = React.useState<ComposeDraft | undefined>()
  const [sidebarCollapsed, setSidebarCollapsed] = React.useState(false)
  const [mailFilter, setMailFilter] = React.useState<MailFilter>("all")
  const [selectedMailboxId, setSelectedMailboxId] = React.useState(() => localStorage.getItem("lanqin:selected-mailbox") || "")
  const [darkMode, setDarkMode] = React.useState(getInitialTheme)
  const [displayMode] = useDisplayMode()
  const [refreshing, setRefreshing] = React.useState(false)
  const [autoRefreshing, setAutoRefreshing] = React.useState(false)
  const [lastAutoRefreshAt, setLastAutoRefreshAt] = React.useState<Date | null>(null)
  const [bulkPending, setBulkPending] = React.useState(false)
  const [pendingConfirm, setPendingConfirm] = React.useState<PendingConfirm | null>(null)
  const sidebarPanelRef = React.useRef<ImperativePanelHandle>(null)
  const themeMountedRef = React.useRef(false)
  const mailNotifyStateRef = React.useRef<Record<string, MailNotificationState>>({})
  const mailAudioContextRef = React.useRef<AudioContext | null>(null)

  const mailboxList = useQuery({ queryKey: ["mailboxes", "mine"], queryFn: api.myMailboxes })
  const publicSettings = useQuery({ queryKey: ["public-settings"], queryFn: api.publicSettings })
  const selectedMailbox = React.useMemo(() => mailboxList.data?.items.find((item) => item.id === selectedMailboxId), [mailboxList.data?.items, selectedMailboxId])
  const activeMailboxId = selectedMailbox?.id || ""
  const hasMailboxes = (mailboxList.data?.items.length || 0) > 0
  const folders = useQuery({ queryKey: ["folders", activeMailboxId], queryFn: () => api.folders(activeMailboxId), enabled: !!activeMailboxId })
  const labels = useQuery({ queryKey: ["labels", activeMailboxId], queryFn: () => api.labels(activeMailboxId), enabled: !!activeMailboxId })
  const mailStats = useQuery({ queryKey: ["mail-stats", activeMailboxId], queryFn: () => api.mailStats(activeMailboxId), enabled: !!activeMailboxId })
  const mailRefreshInterval = publicSettings.data?.mailAutoRefresh ? Math.max(publicSettings.data.mailRefreshMs || 30000, 5000) : false
  const inboxProbe = useQuery({
    queryKey: ["mail-notifications", activeMailboxId],
    queryFn: () => api.messages("Inbox", "", "", activeMailboxId),
    enabled: !!activeMailboxId,
    refetchInterval: mailRefreshInterval,
    refetchIntervalInBackground: true,
  })
  const messages = useInfiniteQuery({
    queryKey: ["messages", activeMailboxId, mailView, folder, selectedLabelId, query],
    queryFn: ({ pageParam }) => {
      const cursor = typeof pageParam === "string" ? pageParam : ""
      if (mailView === "starred") return api.starredMessages(query, cursor, activeMailboxId)
      if (mailView === "label") return api.labelMessages(selectedLabelId, query, cursor, activeMailboxId)
      return api.messages(folder, query, cursor, activeMailboxId)
    },
    initialPageParam: "",
    getNextPageParam: (lastPage) => lastPage.nextCursor || undefined,
    enabled: !!activeMailboxId && (mailView !== "label" || !!selectedLabelId),
  })
  const detail = useQuery({ queryKey: ["message", selectedId], queryFn: () => api.message(selectedId!, { markRead: false }), enabled: !!selectedId })
  function updateCachedMessage(id: string, patch: Partial<MailMessage>) {
    qc.setQueryData(["message", id], (current: MailMessage | undefined) => current ? { ...current, ...patch } : current)
    qc.setQueriesData({ queryKey: ["messages"] }, (current: InfiniteData<MailListResponse> | undefined) => {
      if (!current?.pages) return current
      return {
        ...current,
        pages: current.pages.map((page) => ({
          ...page,
          items: (page.items || []).map((message) => message.id === id ? { ...message, ...patch } : message),
        })),
      }
    })
  }
  const star = useMutation({
    mutationFn: ({ id, starred }: { id: string; starred: boolean }) => api.star(id, starred),
    onMutate: ({ id, starred }) => updateCachedMessage(id, { isStarred: starred }),
    onSuccess: async (_, variables) => {
      await qc.invalidateQueries({ queryKey: ["messages"] })
      await qc.invalidateQueries({ queryKey: ["message", variables.id] })
      await qc.invalidateQueries({ queryKey: ["mail-stats"] })
      await qc.invalidateQueries({ queryKey: ["labels"] })
    },
    onError: (error) => toast({ title: "操作失败", description: error.message }),
  })
  const markRead = useMutation({
    mutationFn: ({ id, read }: { id: string; read: boolean }) => api.markRead(id, read),
    onMutate: ({ id, read }) => updateCachedMessage(id, { isRead: read }),
    onSuccess: async (_, variables) => {
      await qc.invalidateQueries({ queryKey: ["messages"] })
      await qc.invalidateQueries({ queryKey: ["message", variables.id] })
      await qc.invalidateQueries({ queryKey: ["folders"] })
      await qc.invalidateQueries({ queryKey: ["mail-stats"] })
    },
    onError: (error) => toast({ title: "操作失败", description: error.message }),
  })
  const addLabel = useMutation({
    mutationFn: ({ id, label }: { id: string; label: MailLabel }) => api.addLabel(id, { name: label.name, color: label.color }),
    onSuccess: async (data) => {
      if (selectedId) qc.setQueryData(["message", selectedId], (current: MailMessage | undefined) => current ? { ...current, labels: data.labels } : current)
      await qc.invalidateQueries({ queryKey: ["messages"] })
      await qc.invalidateQueries({ queryKey: ["labels"] })
    },
    onError: (error) => toast({ title: "添加标签失败", description: error.message }),
  })
  const removeLabel = useMutation({
    mutationFn: ({ id, labelId }: { id: string; labelId: string }) => api.removeLabel(id, labelId),
    onSuccess: async (data) => {
      if (selectedId) qc.setQueryData(["message", selectedId], (current: MailMessage | undefined) => current ? { ...current, labels: data.labels } : current)
      await qc.invalidateQueries({ queryKey: ["messages"] })
      await qc.invalidateQueries({ queryKey: ["labels"] })
    },
    onError: (error) => toast({ title: "移除标签失败", description: error.message }),
  })
  const createLabel = useMutation({
    mutationFn: (name: string) => api.createLabel({ mailboxId: selectedMailboxId, name }),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ["labels"] })
      toast({ title: "标签已创建" })
    },
    onError: (error) => toast({ title: "创建标签失败", description: error.message }),
  })
  const del = useMutation({ mutationFn: (id: string) => api.delete(id), onSuccess: async () => { setSelectedId(null); setPendingConfirm(null); await qc.invalidateQueries({ queryKey: ["messages"] }); await qc.invalidateQueries({ queryKey: ["folders"] }); await qc.invalidateQueries({ queryKey: ["mail-stats"] }); await qc.invalidateQueries({ queryKey: ["labels"] }); toast({ title: "已删除" }) }, onError: (error) => toast({ title: "删除失败", description: error.message }) })
  const move = useMutation({ mutationFn: ({ id, folder }: { id: string; folder: string }) => api.move(id, folder), onSuccess: async () => { setSelectedId(null); await qc.invalidateQueries({ queryKey: ["messages"] }); await qc.invalidateQueries({ queryKey: ["folders"] }); await qc.invalidateQueries({ queryKey: ["mail-stats"] }); await qc.invalidateQueries({ queryKey: ["labels"] }); toast({ title: "已移动" }) } })
  const markAllRead = useMutation({
    mutationFn: async (items: MailMessage[]) => {
      const unread = items.filter((message) => !message.isRead)
      await Promise.all(unread.map((message) => api.markRead(message.id, true)))
      return unread.length
    },
    onSuccess: async (count) => {
      await qc.invalidateQueries({ queryKey: ["messages"] })
      await qc.invalidateQueries({ queryKey: ["folders"] })
      await qc.invalidateQueries({ queryKey: ["mail-stats"] })
      await qc.invalidateQueries({ queryKey: ["labels"] })
      toast({ title: count > 0 ? `已标记 ${count} 封邮件为已读` : "当前没有未读邮件" })
    },
    onError: (error) => toast({ title: "操作失败", description: error.message }),
  })

  React.useEffect(() => {
    if (!mailboxList.isSuccess) return
    const items = mailboxList.data?.items || []
    if (items.length === 0) {
      if (selectedMailboxId) {
        setSelectedMailboxId("")
        setSelectedId(null)
      }
      localStorage.removeItem("lanqin:selected-mailbox")
      return
    }
    if (!selectedMailboxId || !items.some((item) => item.id === selectedMailboxId)) {
      setSelectedMailboxId(items[0].id)
    }
  }, [mailboxList.isSuccess, mailboxList.data?.items, selectedMailboxId])

  React.useEffect(() => {
    if (selectedMailboxId) localStorage.setItem("lanqin:selected-mailbox", selectedMailboxId)
    else localStorage.removeItem("lanqin:selected-mailbox")
  }, [selectedMailboxId])

  React.useEffect(() => {
    setSelectedId(null)
    setMailFilter("all")
  }, [mailView])

  React.useEffect(() => {
    setCompactSelectedIds([])
  }, [selectedMailboxId, mailView, folder, selectedLabelId, query, displayMode])

  React.useEffect(() => {
    applyTheme(darkMode, themeMountedRef.current)
    themeMountedRef.current = true
  }, [darkMode])

  React.useEffect(() => {
    const unlock = () => {
      const AudioContextCtor = window.AudioContext || (window as unknown as { webkitAudioContext?: typeof AudioContext }).webkitAudioContext
      if (AudioContextCtor && !mailAudioContextRef.current) {
        const ctx = new AudioContextCtor()
        mailAudioContextRef.current = ctx
        if (ctx.state === "suspended") void ctx.resume()
      }
      if ("Notification" in window && Notification.permission === "default") {
        void Notification.requestPermission()
      }
    }
    window.addEventListener("pointerdown", unlock, { once: true })
    window.addEventListener("keydown", unlock, { once: true })
    return () => {
      window.removeEventListener("pointerdown", unlock)
      window.removeEventListener("keydown", unlock)
    }
  }, [])

  React.useEffect(() => {
    if (!activeMailboxId || !inboxProbe.data?.items) return
    const items = inboxProbe.data.items
    const latest = items[0]
    const nextState = { latestId: latest?.id || "", latestReceivedAt: latest?.receivedAt || "" }
    const prevState = mailNotifyStateRef.current[activeMailboxId]
    if (!prevState) {
      mailNotifyStateRef.current[activeMailboxId] = nextState
      return
    }
    const newMessages = items.filter((item) => item.receivedAt > prevState.latestReceivedAt && item.id !== prevState.latestId)
    mailNotifyStateRef.current[activeMailboxId] = nextState
    if (newMessages.length === 0) return

    const first = newMessages[0]
    const firstSender = senderDisplayName(first)
    const title = newMessages.length > 1 ? `收到 ${newMessages.length} 封新邮件` : `新邮件：${first.subject || "(无主题)"}`
    const description = newMessages.length > 1 ? `${firstSender} 等发来新邮件` : `${firstSender}${first.snippet ? ` · ${first.snippet}` : ""}`
    toast({ title, description })
    playIncomingMailSound(mailAudioContextRef)
    if ("Notification" in window && Notification.permission === "granted") {
      const notification = new Notification(title, {
        body: description,
        tag: `lanqin-mail-${activeMailboxId}`,
      })
      notification.onclick = () => {
        window.focus()
        setMailView("folder")
        setFolder("Inbox")
        setSelectedId(first.id)
        notification.close()
      }
    }
  }, [activeMailboxId, inboxProbe.data?.items, toast])

  React.useEffect(() => {
    const events = new EventSource("/api/events", { withCredentials: true })
    events.addEventListener("sync", () => {
      qc.invalidateQueries({ queryKey: ["folders"] })
      qc.invalidateQueries({ queryKey: ["mail-stats"] })
      qc.invalidateQueries({ queryKey: ["labels"] })
      qc.invalidateQueries({ queryKey: ["mail-notifications"] })
    })
    return () => events.close()
  }, [qc])

  React.useEffect(() => {
    if (!publicSettings.data?.mailAutoRefresh) return
    const timer = window.setInterval(() => {
      setAutoRefreshing(true)
      Promise.all([
        qc.invalidateQueries({ queryKey: ["messages"] }),
        qc.invalidateQueries({ queryKey: ["folders"] }),
        qc.invalidateQueries({ queryKey: ["mail-stats"] }),
        qc.invalidateQueries({ queryKey: ["labels"] }),
        qc.invalidateQueries({ queryKey: ["mail-notifications"] }),
      ]).finally(() => {
        setLastAutoRefreshAt(new Date())
        window.setTimeout(() => setAutoRefreshing(false), 600)
      })
    }, mailRefreshInterval || 30000)
    return () => window.clearInterval(timer)
  }, [mailRefreshInterval, publicSettings.data?.mailAutoRefresh, qc])

  const selected = detail.data
  const allMessages = messages.data?.pages.flatMap((page) => page.items || []) || []
  const visibleMessages = allMessages.filter((message) => {
    if (mailFilter === "unread") return !message.isRead
    if (mailFilter === "starred") return message.isStarred
    if (mailFilter === "attachments") return message.hasAttachments
    return true
  })
  const unreadCount = allMessages.filter((message) => !message.isRead).length
  const starredCount = mailStats.data?.starredMessages ?? (mailView === "starred" ? allMessages.length : 0)
  const mailMenuItems = buildMailMenuItems(folders.data?.items || [], starredCount)
  const labelItems = labels.data?.items || []
  const selectedLabel = labelItems.find((item) => item.id === selectedLabelId)
  const viewTitle = mailView === "starred" ? "星标邮件" : mailView === "label" ? selectedLabel?.name || "标签" : folderLabels[folder] || folder
  const emptyMessage = allMessages.length === 0 ? (mailView === "starred" ? "暂无星标邮件" : mailView === "label" ? "当前标签没有邮件" : "当前文件夹没有邮件") : "当前筛选条件下没有邮件"
  const visibleMessageIds = visibleMessages.map((message) => message.id)
  const selectedCountOnPage = compactSelectedIds.filter((id) => visibleMessageIds.includes(id)).length
  const compactAllSelected = visibleMessageIds.length > 0 && selectedCountOnPage === visibleMessageIds.length
  const compactSomeSelected = selectedCountOnPage > 0 && !compactAllSelected
  const hasMoreMessages = !!messages.hasNextPage
  const canLoadMore = !!messages.hasNextPage && !messages.isFetchingNextPage
  function toggleCompactSelectAll(checked: boolean) {
    setCompactSelectedIds(checked ? visibleMessageIds : [])
  }
  function toggleCompactSelect(messageId: string, checked: boolean) {
    setCompactSelectedIds((ids) => checked ? Array.from(new Set([...ids, messageId])) : ids.filter((id) => id !== messageId))
  }
  async function refreshMailData() {
    await Promise.all([
      qc.invalidateQueries({ queryKey: ["messages"] }),
      qc.invalidateQueries({ queryKey: ["folders"] }),
      qc.invalidateQueries({ queryKey: ["mail-stats"] }),
      qc.invalidateQueries({ queryKey: ["labels"] }),
    ])
  }
  async function runBulkAction(action: BulkAction) {
    const ids = compactSelectedIds.filter((id) => visibleMessageIds.includes(id))
    if (ids.length === 0) return
    if (action === "delete") {
      setPendingConfirm({
        title: "删除所选邮件？",
        description: `将删除当前选中的 ${ids.length} 封邮件，此操作无法从邮件列表中恢复。`,
        confirmText: "删除邮件",
        onConfirm: () => runConfirmedBulkAction("delete", ids),
      })
      return
    }
    await runConfirmedBulkAction(action, ids)
  }
  async function runConfirmedBulkAction(action: BulkAction, ids: string[]) {
    setBulkPending(true)
    try {
      if (action === "read" || action === "unread") {
        const read = action === "read"
        await Promise.all(ids.map((id) => api.markRead(id, read)))
      } else if (action === "star" || action === "unstar") {
        const starred = action === "star"
        await Promise.all(ids.map((id) => api.star(id, starred)))
      } else if (action === "delete") {
        await Promise.all(ids.map((id) => api.delete(id)))
      } else {
        const target = action === "archive" ? "Archive" : action === "trash" ? "Trash" : "Spam"
        await Promise.all(ids.map((id) => api.move(id, target)))
      }
      if (selectedId && ids.includes(selectedId)) setSelectedId(null)
      setCompactSelectedIds([])
      setPendingConfirm(null)
      await refreshMailData()
      toast({ title: `已处理 ${ids.length} 封邮件` })
    } catch (error) {
      toast({ title: "批量操作失败", description: error instanceof Error ? error.message : "请稍后重试" })
    } finally {
      setBulkPending(false)
    }
  }
  function confirmDeleteMessage(message: MailMessage) {
    setPendingConfirm({
      title: "删除这封邮件？",
      description: `邮件“${message.subject || "无主题"}”将被删除。`,
      confirmText: "删除邮件",
      onConfirm: () => del.mutate(message.id),
    })
  }
  function openCompose(draft?: ComposeDraft) { setComposeDraft(draft || { key: `new-${Date.now()}` }); setComposeOpen(true) }
  function openReply(message: MailMessage) { openCompose({ key: `reply-${message.id}-${Date.now()}`, to: message.from, subject: withPrefix(message.subject, "Re:"), text: quoteMessage(message) }) }
  function openForward(message: MailMessage) { openCompose({ key: `forward-${message.id}-${Date.now()}`, subject: withPrefix(message.subject, "Fwd:"), text: quoteMessage(message) }) }
  function switchMailbox(mailboxId: string) {
    setSelectedMailboxId(mailboxId)
    setFolder("Inbox")
    setMailView("folder")
    setSelectedLabelId("")
    setSelectedId(null)
    setMailFilter("all")
  }
  function openFolder(nextFolder: string) {
    setFolder(nextFolder)
    setMailView("folder")
    setSelectedLabelId("")
    setSelectedId(null)
  }
  function openStarred() {
    setMailView("starred")
    setSelectedLabelId("")
    setSelectedId(null)
    setMailFilter("all")
  }
  function openLabel(labelId: string) {
    setSelectedLabelId(labelId)
    setMailView("label")
    setSelectedId(null)
    setMailFilter("all")
  }
  function openMessage(messageId: string | null) {
    setSelectedId(messageId)
    if (!messageId) return
    const message = allMessages.find((item) => item.id === messageId)
    if (message && !message.isRead) {
      markRead.mutate({ id: message.id, read: true })
    }
  }
  async function refreshMail() {
    setRefreshing(true)
    try {
      await refreshMailData()
      setLastAutoRefreshAt(new Date())
    } finally {
      setRefreshing(false)
    }
  }
  async function copyCurrentMailbox() {
    if (!selectedMailbox?.address) return
    await navigator.clipboard.writeText(selectedMailbox.address)
    toast({ title: "邮箱地址已复制" })
  }
  function openSettings() {
    navigate("/profile")
  }
  function toggleSidebar() {
    if (sidebarCollapsed) {
      sidebarPanelRef.current?.expand(14)
      setSidebarCollapsed(false)
    } else {
      sidebarPanelRef.current?.collapse()
      setSidebarCollapsed(true)
    }
  }

  return (
    <div className="h-svh bg-background">
      <SidebarProvider className="h-full min-h-0 w-full">
        <ResizablePanelGroup direction="horizontal" className="h-full min-h-0 w-full">
            <ResizablePanel ref={sidebarPanelRef} collapsible collapsedSize={4} defaultSize={15} minSize={11} maxSize={24} onCollapse={() => setSidebarCollapsed(true)} onExpand={() => setSidebarCollapsed(false)}>
                <Sidebar collapsible="none" className="h-full w-full border-r bg-sidebar">
                  <SidebarHeader className={cn("border-b py-3", sidebarCollapsed ? "px-2" : "px-3")}>
                  <AccountHeader
                    collapsed={sidebarCollapsed}
                    name={me.data?.user.displayName || selectedMailbox?.address || "LanQin"}
                    email={me.data?.user.email || selectedMailbox?.address}
                    darkMode={darkMode}
                    onToggleTheme={() => setDarkMode((value) => !value)}
                    onSettings={openSettings}
                  />
                  <div className={cn("mt-2 flex gap-2", sidebarCollapsed && "justify-center")}>
                    <MailboxSwitcher
                      collapsed={sidebarCollapsed}
                      mailboxes={mailboxList.data?.items || []}
                      selectedMailbox={selectedMailbox}
                      onSelect={switchMailbox}
                    />
                    {!sidebarCollapsed && (
                      <Button type="button" variant="outline" size="icon" className="h-9 w-9 shrink-0 rounded-md" onClick={copyCurrentMailbox} disabled={!selectedMailbox}>
                        <Copy className="h-4 w-4" />
                      </Button>
                    )}
                  </div>
                  <Button className={cn("mt-2 h-10 w-full rounded-md text-sm", sidebarCollapsed && "px-0")} size={sidebarCollapsed ? "icon" : "default"} onClick={() => openCompose()} disabled={!selectedMailbox}>
                    <PencilLine className="h-4 w-4" />
                    {!sidebarCollapsed && <span>写邮件</span>}
                  </Button>
                </SidebarHeader>
                <SidebarContent>
                  <SidebarGroup>
                    {!sidebarCollapsed && <SidebarGroupLabel>邮件夹</SidebarGroupLabel>}
                    <SidebarGroupContent>
                      <SidebarMenu>
                        {mailMenuItems.map((item) => (
                          <SidebarMenuItem key={item.key}>
                            <SidebarMenuButton
                              isActive={item.type === "starred" ? mailView === "starred" : mailView === "folder" && folder === item.folderName}
                              className={cn(sidebarCollapsed && "justify-center px-0")}
                              onClick={() => item.type === "starred" ? openStarred() : openFolder(item.folderName)}
                            >
                              {item.icon}
                              {!sidebarCollapsed && <span>{item.label}</span>}
                              {!sidebarCollapsed && item.count > 0 && <Badge variant="secondary" className="ml-auto">{item.count}</Badge>}
                            </SidebarMenuButton>
                          </SidebarMenuItem>
                        ))}
                      </SidebarMenu>
                      {folders.isLoading && <FolderSkeleton />}
                    </SidebarGroupContent>
                  </SidebarGroup>
                  <SidebarGroup>
                    {!sidebarCollapsed && <SidebarGroupLabel>标签</SidebarGroupLabel>}
                    <SidebarGroupContent>
                      <SidebarMenu>
                        {labelItems.map((label) => (
                          <SidebarMenuItem key={label.id}>
                            <SidebarMenuButton isActive={mailView === "label" && selectedLabelId === label.id} className={cn(sidebarCollapsed && "justify-center px-0")} onClick={() => openLabel(label.id)}>
                              <Tag className="h-4 w-4" style={{ color: label.color }} />
                              {!sidebarCollapsed && <span>{label.name}</span>}
                              {!sidebarCollapsed && !!label.messageCount && <Badge variant="secondary" className="ml-auto">{label.messageCount}</Badge>}
                            </SidebarMenuButton>
                          </SidebarMenuItem>
                        ))}
                        {!sidebarCollapsed && !labels.isLoading && labelItems.length === 0 && <div className="px-2 py-1 text-xs text-muted-foreground">暂无标签</div>}
                        <SidebarMenuItem>
                          <NewLabelButton collapsed={sidebarCollapsed} pending={createLabel.isPending} onCreate={(name) => createLabel.mutate(name)} />
                        </SidebarMenuItem>
                      </SidebarMenu>
                      {labels.isLoading && <FolderSkeleton />}
                    </SidebarGroupContent>
                  </SidebarGroup>
                </SidebarContent>
                <div className={cn("mt-auto border-t p-2", sidebarCollapsed ? "flex justify-center" : "")}>
                  <Button type="button" variant="ghost" size={sidebarCollapsed ? "icon" : "sm"} className={cn(!sidebarCollapsed && "w-full justify-start")} onClick={toggleSidebar}>
                    {sidebarCollapsed ? <PanelLeftOpen className="h-4 w-4" /> : <PanelLeftClose className="h-4 w-4" />}
                    {!sidebarCollapsed && <span>收起侧栏</span>}
                  </Button>
                </div>
              </Sidebar>
            </ResizablePanel>
            <ResizableHandle withHandle />

            <ResizablePanel defaultSize={85} minSize={60}>
              <section className="flex h-full min-h-0 flex-col">
                <header className="flex h-16 shrink-0 items-center justify-between gap-3 border-b px-5">
                  <div className="flex items-center gap-2">
                    <Button size="icon" variant="ghost" onClick={refreshMail} disabled={refreshing || autoRefreshing} className={cn("transition-all", (refreshing || autoRefreshing) && "bg-primary/5 text-primary")} title={autoRefreshing ? "自动刷新中" : "刷新邮件"}>
                      <RefreshCcw className={cn("h-4 w-4", (refreshing || autoRefreshing) && "animate-spin")} />
                    </Button>
                    {(publicSettings.data?.mailAutoRefresh || autoRefreshing) && (
                      <div className="hidden min-w-[118px] text-xs text-muted-foreground sm:block">
                        {autoRefreshing ? "自动刷新中..." : lastAutoRefreshAt ? `已刷新 ${lastAutoRefreshAt.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}` : "自动刷新已开启"}
                      </div>
                    )}
                    <Button variant="outline" size="sm" disabled={!activeMailboxId || markAllRead.isPending || unreadCount === 0} onClick={() => markAllRead.mutate(allMessages)}><MailCheck className="h-4 w-4" />全部已读</Button>
                    <DropdownMenu>
                      <DropdownMenuTrigger asChild>
                        <Button variant="outline" size="sm"><SlidersHorizontal className="h-4 w-4" />{filterLabels[mailFilter]}</Button>
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="start">
                        {(Object.keys(filterLabels) as MailFilter[]).map((value) => (
                          <DropdownMenuItem key={value} onSelect={() => setMailFilter(value)}>
                            {filterLabels[value]}
                          </DropdownMenuItem>
                        ))}
                      </DropdownMenuContent>
                    </DropdownMenu>
                  </div>
                  <div className="relative w-full max-w-md">
                    <Search className="absolute left-3 top-2.5 h-4 w-4 text-muted-foreground" />
                    <Input value={query} onChange={(e) => setQuery(e.target.value)} placeholder="搜索邮件" className="pl-9" />
                  </div>
                </header>

                {!mailboxList.isLoading && !hasMailboxes ? (
                  <NoMailboxState onOpenSettings={openSettings} />
                ) : displayMode === "compact" ? (
                  <CompactMailView
                    title={viewTitle}
                    icon={mailView === "label" && selectedLabel ? <Tag className="h-4 w-4" style={{ color: selectedLabel.color }} /> : undefined}
                    messages={visibleMessages}
                    total={allMessages.length}
                    selectedIds={compactSelectedIds}
                    allSelected={compactAllSelected}
                    someSelected={compactSomeSelected}
                    loading={messages.isLoading}
                    hasMore={hasMoreMessages}
                    loadingMore={messages.isFetchingNextPage}
                    onLoadMore={() => messages.fetchNextPage()}
                    emptyMessage={emptyMessage}
                    selectedId={selectedId}
                    selected={selected}
                    detailLoading={detail.isLoading}
                    labels={labelItems}
                    labelPending={addLabel.isPending || removeLabel.isPending}
                    onSelect={openMessage}
                    onSelectAll={toggleCompactSelectAll}
                    onToggleSelected={toggleCompactSelect}
                    onCloseReader={() => setSelectedId(null)}
                    onStar={(message) => star.mutate({ id: message.id, starred: !message.isStarred })}
                    onReply={openReply}
                    onForward={openForward}
                    onArchive={(message) => move.mutate({ id: message.id, folder: message.folder === "Archive" ? "Inbox" : "Archive" })}
                    onDelete={confirmDeleteMessage}
                    onToggleRead={(message) => markRead.mutate({ id: message.id, read: !message.isRead })}
                    onAddLabel={(message, label) => addLabel.mutate({ id: message.id, label })}
                    onRemoveLabel={(message, labelId) => removeLabel.mutate({ id: message.id, labelId })}
                    bulkPending={bulkPending}
                    onBulkAction={runBulkAction}
                  />
                ) : (
                <ResizablePanelGroup direction="horizontal" className="min-h-0 flex-1">
                  <ResizablePanel defaultSize={32} minSize={24} maxSize={44}>
                    <div className="flex h-full min-h-0 flex-col">
                      <div className="flex h-14 shrink-0 items-center justify-between border-b px-5">
                        <div className="flex min-w-0 items-center gap-3">
                          <Checkbox aria-label="选择当前页邮件" checked={compactAllSelected ? true : compactSomeSelected ? "indeterminate" : false} onCheckedChange={(value) => toggleCompactSelectAll(value === true)} />
                          <div className="min-w-0">
                            <div className="flex items-center gap-2 text-sm font-semibold">{mailView === "label" && selectedLabel && <Tag className="h-4 w-4" style={{ color: selectedLabel.color }} />}{viewTitle}</div>
                            <div className="text-xs text-muted-foreground">{selectedCountOnPage > 0 ? `已选 ${selectedCountOnPage} 封` : `${visibleMessages.length} / ${allMessages.length} 封邮件`}</div>
                          </div>
                        </div>
                        {selectedCountOnPage > 0 && <BulkActionMenu pending={bulkPending} onAction={runBulkAction} />}
                      </div>
                      <ScrollArea className="min-h-0 flex-1">
                        {messages.isLoading && <MessageSkeleton />}
                        {visibleMessages.map((m) => <MessageRow key={m.id} message={m} active={selectedId === m.id} checked={compactSelectedIds.includes(m.id)} onCheckedChange={(checked) => toggleCompactSelect(m.id, checked)} onClick={() => openMessage(m.id)} onStar={() => star.mutate({ id: m.id, starred: !m.isStarred })} />)}
                        {!messages.isLoading && visibleMessages.length === 0 && <div className="p-8 text-center text-sm text-muted-foreground">{emptyMessage}</div>}
                        {!messages.isLoading && hasMoreMessages && (
                          <div className="border-b p-4 text-center">
                            <Button variant="outline" size="sm" disabled={!canLoadMore} onClick={() => messages.fetchNextPage()}>
                              {messages.isFetchingNextPage ? "加载中..." : "加载更多"}
                            </Button>
                          </div>
                        )}
                      </ScrollArea>
                    </div>
                  </ResizablePanel>
                  <ResizableHandle withHandle />

                  <ResizablePanel defaultSize={68} minSize={44}>
                    <section className="h-full min-h-0">
                      {!selectedId && <div className="grid h-full place-items-center text-muted-foreground">选择一封邮件阅读</div>}
                      {detail.isLoading && <div className="space-y-4 p-6"><Skeleton className="h-8 w-2/3" /><Skeleton className="h-4 w-1/3" /><Separator /><Skeleton className="h-40 w-full" /></div>}
                      {selected && <div className="flex h-full min-h-0 flex-col">
                        <div className="border-b p-5">
                          <div className="mb-4 flex items-center justify-between gap-3">
                            <h2 className="text-xl font-semibold">{selected.subject}</h2>
                            <div className="flex flex-wrap justify-end gap-2">
                              <Button variant="outline" size="sm" onClick={() => openReply(selected)}><Reply className="h-4 w-4" />回复</Button>
                              <Button variant="outline" size="sm" onClick={() => openForward(selected)}><Forward className="h-4 w-4" />转发</Button>
                              {selected.folder === "Archive" ? (
                                <Button variant="outline" size="sm" onClick={() => move.mutate({ id: selected.id, folder: "Inbox" })}>取消归档</Button>
                              ) : (
                                <Button variant="outline" size="sm" onClick={() => move.mutate({ id: selected.id, folder: "Archive" })}>归档</Button>
                              )}
                              <Button variant="destructive" size="sm" onClick={() => confirmDeleteMessage(selected)}>删除</Button>
                            </div>
                          </div>
                          <div className="text-sm text-muted-foreground"><span className="font-medium text-foreground" title={senderTitle(selected)}>{senderDisplayName(selected)}</span> 发给 {selected.to.join(", ")} · {formatDateTime(selected.receivedAt)}</div>
                          <MessageLabels
                            messageLabels={selected.labels || []}
                            availableLabels={labelItems}
                            onAdd={(label) => addLabel.mutate({ id: selected.id, label })}
                            onRemove={(labelId) => removeLabel.mutate({ id: selected.id, labelId })}
                            pending={addLabel.isPending || removeLabel.isPending}
                          />
                        </div>
                        <ScrollArea className="min-h-0 flex-1">
                          <div className="p-6">
                            <div className="mail-html prose max-w-none text-sm leading-7" dangerouslySetInnerHTML={{ __html: DOMPurify.sanitize(selected.bodyHtml || `<pre>${selected.bodyText || ""}</pre>`) }} />
                            {selected.attachments && selected.attachments.length > 0 && <div className="mt-8 rounded-lg border p-4"><div className="mb-3 font-medium">附件</div><div className="space-y-2">{selected.attachments.map((a) => <a className="flex items-center justify-between rounded-md border p-3 text-sm hover:bg-accent" href={`/api/mail/attachments/${a.id}`} key={a.id}><span className="flex items-center gap-2"><Paperclip className="h-4 w-4" />{a.filename}</span><span className="text-muted-foreground">{formatBytes(a.sizeBytes)}</span></a>)}</div></div>}
                          </div>
                        </ScrollArea>
                      </div>}
                    </section>
                  </ResizablePanel>
                </ResizablePanelGroup>

                )}
              </section>
            </ResizablePanel>
          </ResizablePanelGroup>
      </SidebarProvider>

      <ComposeDialog mailbox={selectedMailbox} open={composeOpen} draft={composeDraft} onOpenChange={(open) => { setComposeOpen(open); if (!open) setComposeDraft(undefined) }} onSent={() => { setComposeOpen(false); setComposeDraft(undefined); qc.invalidateQueries({ queryKey: ["messages"] }); qc.invalidateQueries({ queryKey: ["folders"] }); qc.invalidateQueries({ queryKey: ["mail-stats"] }); qc.invalidateQueries({ queryKey: ["labels"] }) }} />
      <ConfirmDialog
        open={!!pendingConfirm}
        title={pendingConfirm?.title || ""}
        description={pendingConfirm?.description}
        confirmText={pendingConfirm?.confirmText || "确认"}
        destructive
        pending={del.isPending || bulkPending}
        onOpenChange={(open) => { if (!open) setPendingConfirm(null) }}
        onConfirm={() => pendingConfirm?.onConfirm()}
      />
    </div>
  )
}

function buildMailMenuItems(folders: MailFolder[], starredCount: number): MailMenuItem[] {
  const folderItems: MailMenuItem[] = folders.map((item) => ({
    type: "folder",
    key: item.id,
    folderName: item.name,
    label: folderLabels[item.name] || item.name,
    icon: folderIcons[item.role] || <Inbox className="h-4 w-4" />,
    count: item.unreadCount,
  }))
  const starredItem: MailMenuItem = { type: "starred", key: "starred", label: "星标邮件", icon: <Star className="h-4 w-4" />, count: starredCount }
  const inboxIndex = folderItems.findIndex((item) => item.type === "folder" && item.folderName === "Inbox")
  const insertAt = inboxIndex >= 0 ? inboxIndex + 1 : 0
  return [...folderItems.slice(0, insertAt), starredItem, ...folderItems.slice(insertAt)]
}

function FolderSkeleton() { return <div className="space-y-2 p-2"><Skeleton className="h-8 w-full" /><Skeleton className="h-8 w-4/5" /><Skeleton className="h-8 w-3/4" /></div> }
function MessageSkeleton() { return <div className="space-y-0">{Array.from({ length: 6 }).map((_, i) => <div className="space-y-2 border-b p-4" key={i}><Skeleton className="h-4 w-1/2" /><Skeleton className="h-4 w-4/5" /><Skeleton className="h-3 w-full" /></div>)}</div> }

function NoMailboxState({ onOpenSettings }: { onOpenSettings: () => void }) {
  return (
    <div className="grid min-h-0 flex-1 place-items-center p-6">
      <div className="w-full max-w-md rounded-lg border border-dashed p-8 text-center">
        <div className="mx-auto mb-4 grid size-12 place-items-center rounded-full bg-muted">
          <Mail className="h-5 w-5 text-muted-foreground" />
        </div>
        <div className="text-lg font-semibold">还没有可用邮箱</div>
        <div className="mt-2 text-sm text-muted-foreground">请在个人中心申请邮箱，或联系管理员为当前账号分配邮箱。</div>
        <Button className="mt-5" onClick={onOpenSettings}>
          <Settings className="h-4 w-4" />前往个人中心
        </Button>
      </div>
    </div>
  )
}

type BulkAction = "read" | "unread" | "star" | "unstar" | "archive" | "trash" | "spam" | "delete"

function BulkActionMenu({ pending, onAction }: { pending: boolean; onAction: (action: BulkAction) => void }) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="outline" size="sm" disabled={pending}>
          批量操作
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuItem onSelect={() => onAction("read")}>标为已读</DropdownMenuItem>
        <DropdownMenuItem onSelect={() => onAction("unread")}>标为未读</DropdownMenuItem>
        <DropdownMenuItem onSelect={() => onAction("star")}>添加星标</DropdownMenuItem>
        <DropdownMenuItem onSelect={() => onAction("unstar")}>取消星标</DropdownMenuItem>
        <DropdownMenuItem onSelect={() => onAction("archive")}>归档</DropdownMenuItem>
        <DropdownMenuItem onSelect={() => onAction("trash")}>移入回收站</DropdownMenuItem>
        <DropdownMenuItem onSelect={() => onAction("spam")}>移入垃圾邮件</DropdownMenuItem>
        <DropdownMenuItem onSelect={() => onAction("delete")} className="text-destructive">删除</DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

function CompactMailView({
  title,
  icon,
  messages,
  total,
  selectedIds,
  allSelected,
  someSelected,
  loading,
  hasMore,
  loadingMore,
  emptyMessage,
  selectedId,
  selected,
  detailLoading,
  labels,
  labelPending,
  onSelect,
  onSelectAll,
  onToggleSelected,
  onLoadMore,
  onCloseReader,
  onStar,
  onReply,
  onForward,
  onArchive,
  onDelete,
  onToggleRead,
  onAddLabel,
  onRemoveLabel,
  bulkPending,
  onBulkAction,
}: {
  title: string
  icon?: React.ReactNode
  messages: MailMessage[]
  total: number
  selectedIds: string[]
  allSelected: boolean
  someSelected: boolean
  loading: boolean
  hasMore: boolean
  loadingMore: boolean
  emptyMessage: string
  selectedId: string | null
  selected?: MailMessage
  detailLoading: boolean
  labels: MailLabel[]
  labelPending: boolean
  onSelect: (id: string | null) => void
  onSelectAll: (checked: boolean) => void
  onToggleSelected: (id: string, checked: boolean) => void
  onLoadMore: () => void
  onCloseReader: () => void
  onStar: (message: MailMessage) => void
  onReply: (message: MailMessage) => void
  onForward: (message: MailMessage) => void
  onArchive: (message: MailMessage) => void
  onDelete: (message: MailMessage) => void
  onToggleRead: (message: MailMessage) => void
  onAddLabel: (message: MailMessage, label: MailLabel) => void
  onRemoveLabel: (message: MailMessage, labelId: string) => void
  bulkPending: boolean
  onBulkAction: (action: BulkAction) => void
}) {
  const selectedIndex = selectedId ? messages.findIndex((message) => message.id === selectedId) : -1
  const previousMessage = selectedIndex > 0 ? messages[selectedIndex - 1] : undefined
  const nextMessage = selectedIndex >= 0 && selectedIndex < messages.length - 1 ? messages[selectedIndex + 1] : undefined

  if (selectedId) {
    return (
      <CompactMessageDetail
        selected={selected}
        loading={detailLoading}
        labels={labels}
        labelPending={labelPending}
        previousMessage={previousMessage}
        nextMessage={nextMessage}
        onBack={onCloseReader}
        onSelect={onSelect}
        onStar={onStar}
        onReply={onReply}
        onForward={onForward}
        onArchive={onArchive}
        onDelete={onDelete}
        onToggleRead={onToggleRead}
        onAddLabel={onAddLabel}
        onRemoveLabel={onRemoveLabel}
      />
    )
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col bg-background">
      <div className="flex h-12 shrink-0 items-center justify-between border-b px-4">
        <div className="flex items-center gap-3">
          <Checkbox aria-label="选择当前页邮件" checked={allSelected ? true : someSelected ? "indeterminate" : false} onCheckedChange={(value) => onSelectAll(value === true)} />
          <div className="flex items-center gap-2 text-base font-semibold">{icon}{title}</div>
        </div>
        <div className="flex items-center gap-2">
          {selectedIds.length > 0 ? (
            <>
              <span className="text-sm text-muted-foreground">已选 {selectedIds.length} 封</span>
              <BulkActionMenu pending={bulkPending} onAction={onBulkAction} />
            </>
          ) : (
            <div className="text-sm text-muted-foreground">{messages.length} / {total} 封</div>
          )}
        </div>
      </div>
      <ScrollArea className="min-h-0 flex-1">
        {loading && <MessageSkeleton />}
        {messages.map((message) => <CompactMessageRow key={message.id} message={message} active={selectedId === message.id} checked={selectedIds.includes(message.id)} onCheckedChange={(checked) => onToggleSelected(message.id, checked)} onClick={() => onSelect(message.id)} onStar={() => onStar(message)} />)}
        {!loading && messages.length === 0 && <div className="p-8 text-center text-sm text-muted-foreground">{emptyMessage}</div>}
        {!loading && hasMore && (
          <div className="border-b p-4 text-center">
            <Button variant="outline" size="sm" disabled={loadingMore} onClick={onLoadMore}>
              {loadingMore ? "加载中..." : "加载更多"}
            </Button>
          </div>
        )}
      </ScrollArea>
    </div>
  )
}

function CompactMessageDetail({
  selected,
  loading,
  labels,
  labelPending,
  previousMessage,
  nextMessage,
  onBack,
  onSelect,
  onStar,
  onReply,
  onForward,
  onArchive,
  onDelete,
  onToggleRead,
  onAddLabel,
  onRemoveLabel,
}: {
  selected?: MailMessage
  loading: boolean
  labels: MailLabel[]
  labelPending: boolean
  previousMessage?: MailMessage
  nextMessage?: MailMessage
  onBack: () => void
  onSelect: (id: string | null) => void
  onStar: (message: MailMessage) => void
  onReply: (message: MailMessage) => void
  onForward: (message: MailMessage) => void
  onArchive: (message: MailMessage) => void
  onDelete: (message: MailMessage) => void
  onToggleRead: (message: MailMessage) => void
  onAddLabel: (message: MailMessage, label: MailLabel) => void
  onRemoveLabel: (message: MailMessage, labelId: string) => void
}) {
  return (
    <div className="flex min-h-0 flex-1 flex-col bg-background">
      <div className="flex h-14 shrink-0 items-center justify-between gap-3 border-b px-4">
          <div className="flex flex-wrap items-center gap-2">
            <Button variant="outline" size="sm" onClick={onBack}><ArrowLeft className="h-4 w-4" />返回</Button>
            {selected && <Button variant="outline" size="sm" onClick={() => onReply(selected)}><Reply className="h-4 w-4" />回复</Button>}
            {selected && <Button variant="outline" size="sm" onClick={() => onDelete(selected)}><Trash2 className="h-4 w-4" />删除</Button>}
            {selected && <Button variant="outline" size="sm" onClick={() => onForward(selected)}><Forward className="h-4 w-4" />转发</Button>}
            {selected && <Button variant="outline" size="sm" onClick={() => onArchive(selected)}>{selected.folder === "Archive" ? "取消归档" : "归档"}</Button>}
            {selected && <Button variant="outline" size="sm" onClick={() => onToggleRead(selected)}><MailCheck className="h-4 w-4" />{selected.isRead ? "标为未读" : "标为已读"}</Button>}
            {selected && <Button variant="outline" size="sm" onClick={() => onStar(selected)}><Star className={cn("h-4 w-4", selected.isStarred && "fill-yellow-400 text-yellow-500")} />{selected.isStarred ? "取消星标" : "添加星标"}</Button>}
          </div>
          <div className="flex items-center gap-2">
            <Button variant="ghost" size="sm" disabled={!previousMessage} onClick={() => previousMessage && onSelect(previousMessage.id)}>上一封</Button>
            <Button variant="ghost" size="sm" disabled={!nextMessage} onClick={() => nextMessage && onSelect(nextMessage.id)}>下一封</Button>
          </div>
        </div>
        {loading && <div className="space-y-4 p-8"><Skeleton className="h-8 w-2/3" /><Skeleton className="h-4 w-1/3" /><Separator /><Skeleton className="h-64 w-full" /></div>}
        {!loading && !selected && <div className="grid flex-1 place-items-center text-sm text-muted-foreground">邮件不存在</div>}
        {selected && (
          <ScrollArea className="min-h-0 flex-1">
            <div className="w-full px-8 py-6">
              <div className="space-y-5 border-b pb-5">
                <div className="flex items-center gap-3">
                  <h1 className="min-w-0 flex-1 truncate text-2xl font-semibold tracking-tight">{selected.subject}</h1>
                  <Button type="button" variant="ghost" size="icon" aria-label={selected.isStarred ? "取消星标" : "添加星标"} className="text-muted-foreground hover:text-yellow-500" onClick={() => onStar(selected)}>
                    <Star className={cn("h-5 w-5", selected.isStarred && "fill-yellow-400 text-yellow-500")} />
                  </Button>
                </div>
                <div className="flex items-start justify-between gap-4">
                  <div className="flex min-w-0 items-start gap-3">
                    <Avatar className="size-10 rounded-full"><AvatarFallback className="bg-primary text-sm font-semibold text-primary-foreground">{accountInitial(senderDisplayName(selected), selected.from)}</AvatarFallback></Avatar>
                    <div className="min-w-0 text-sm">
                      <div className="truncate font-medium text-foreground" title={senderTitle(selected)}>{senderDisplayName(selected)}</div>
                      <div className="truncate text-muted-foreground">收件人 {selected.to.join(", ")}</div>
                    </div>
                  </div>
                  <div className="shrink-0 text-right text-sm text-muted-foreground">{formatDateTime(selected.receivedAt)}</div>
                </div>
                <MessageLabels
                  messageLabels={selected.labels || []}
                  availableLabels={labels}
                  onAdd={(label) => onAddLabel(selected, label)}
                  onRemove={(labelId) => onRemoveLabel(selected, labelId)}
                  pending={labelPending}
                />
              </div>
              <div className="py-8">
                <div className="mail-html prose max-w-none text-sm leading-7" dangerouslySetInnerHTML={{ __html: DOMPurify.sanitize(selected.bodyHtml || `<pre>${selected.bodyText || ""}</pre>`) }} />
                {selected.attachments && selected.attachments.length > 0 && <div className="mt-8 rounded-lg border p-4"><div className="mb-3 font-medium">附件</div><div className="space-y-2">{selected.attachments.map((a) => <a className="flex items-center justify-between rounded-md border p-3 text-sm hover:bg-accent" href={`/api/mail/attachments/${a.id}`} key={a.id}><span className="flex items-center gap-2"><Paperclip className="h-4 w-4" />{a.filename}</span><span className="text-muted-foreground">{formatBytes(a.sizeBytes)}</span></a>)}</div></div>}
              </div>
            </div>
          </ScrollArea>
        )}
    </div>
  )
}

function CompactMessageRow({ message, active, checked, onCheckedChange, onClick, onStar }: { message: MailMessage; active: boolean; checked: boolean; onCheckedChange: (checked: boolean) => void; onClick: () => void; onStar: () => void }) {
  const visibleLabels = (message.labels || []).slice(0, 2)
  const senderName = senderDisplayName(message)
  return (
    <div onClick={onClick} className={cn("grid cursor-pointer grid-cols-[32px_28px_minmax(140px,240px)_minmax(0,1fr)_88px_36px] items-center gap-2 border-b px-4 py-2 text-sm transition-colors hover:bg-accent/50", active && "bg-accent", !message.isRead && "font-semibold")}>
      <Checkbox aria-label="选择邮件" checked={checked} onCheckedChange={(value) => onCheckedChange(value === true)} onClick={(event) => event.stopPropagation()} />
      <Mail className={cn("h-4 w-4", message.isRead ? "text-muted-foreground/70" : "fill-yellow-200 text-yellow-500")} />
      <div className="truncate" title={senderTitle(message)}>{senderName}</div>
      <div className="flex min-w-0 items-center gap-2">
        <span className="truncate font-medium">{message.subject}</span>
        <span className="min-w-0 truncate text-muted-foreground">{message.snippet}</span>
        {visibleLabels.map((label) => <MailLabelBadge key={label.id} label={label} compact />)}
        {message.hasAttachments && <Paperclip className="h-3 w-3 shrink-0 text-muted-foreground" />}
      </div>
      <div className="shrink-0 text-right text-xs text-muted-foreground">{formatDate(message.receivedAt)}</div>
      <Button type="button" variant="ghost" size="icon" aria-label={message.isStarred ? "取消星标" : "添加星标"} className="h-7 w-7 text-muted-foreground hover:text-yellow-500" onClick={(event) => { event.stopPropagation(); onStar() }}>
        <Star className={cn("h-4 w-4", message.isStarred && "fill-yellow-400 text-yellow-500")} />
      </Button>
    </div>
  )
}

function NewLabelButton({ collapsed, pending, onCreate }: { collapsed: boolean; pending: boolean; onCreate: (name: string) => void }) {
  const [editing, setEditing] = React.useState(false)
  const [value, setValue] = React.useState("")
  if (collapsed) {
    return (
      <SidebarMenuButton className="justify-center px-0" onClick={() => setEditing(true)}>
        <Plus className="h-4 w-4" />
      </SidebarMenuButton>
    )
  }
  if (editing) {
    return (
      <form
        className="px-2 py-1"
        onSubmit={(event) => {
          event.preventDefault()
          const name = value.trim()
          if (!name) return
          onCreate(name)
          setValue("")
          setEditing(false)
        }}
      >
        <Input autoFocus value={value} onChange={(event) => setValue(event.target.value)} onBlur={() => { if (!value.trim()) setEditing(false) }} placeholder="新建标签" disabled={pending} />
      </form>
    )
  }
  return (
    <SidebarMenuButton className="text-muted-foreground" onClick={() => setEditing(true)}>
      <Plus className="h-4 w-4" />
      <span>新建标签</span>
    </SidebarMenuButton>
  )
}

function AccountHeader({ collapsed, name, email, darkMode, onToggleTheme, onSettings }: { collapsed: boolean; name: string; email?: string; darkMode: boolean; onToggleTheme: () => void; onSettings: () => void }) {
  const displayName = cleanAccountName(name, email)
  if (collapsed) {
    return (
      <div className="flex justify-center">
        <Avatar className="size-8 rounded-full">
          <AvatarFallback className="bg-primary text-xs font-semibold text-primary-foreground">{accountInitial(displayName, email)}</AvatarFallback>
        </Avatar>
      </div>
    )
  }
  return (
    <div className="flex items-center justify-between gap-2">
      <div className="flex min-w-0 items-center gap-2">
        <Avatar className="size-8 rounded-full">
          <AvatarFallback className="bg-primary text-xs font-semibold text-primary-foreground">{accountInitial(displayName, email)}</AvatarFallback>
        </Avatar>
        <div className="min-w-0 text-sm">
          <div className="truncate text-sm font-semibold leading-5">{displayName}</div>
        </div>
      </div>
      <div className="flex shrink-0 items-center gap-1">
        <Button type="button" variant="ghost" size="icon" className="size-8 rounded-md text-muted-foreground" onClick={onToggleTheme}>
          {darkMode ? <Sun className="h-4 w-4" /> : <Moon className="h-4 w-4" />}
        </Button>
        <Button type="button" variant="ghost" size="icon" className="size-8 rounded-md text-muted-foreground" onClick={onSettings}>
          <Settings className="h-4 w-4" />
        </Button>
      </div>
    </div>
  )
}

function MailboxSwitcher({ collapsed, mailboxes, selectedMailbox, onSelect }: { collapsed: boolean; mailboxes: Mailbox[]; selectedMailbox?: Mailbox; onSelect: (mailboxId: string) => void }) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="outline" className={cn("h-9 min-w-0 flex-1 justify-start gap-2 rounded-md bg-background px-2 text-left font-normal", collapsed && "w-8 flex-none justify-center px-0")}>
          <Mail className="h-4 w-4 shrink-0 text-muted-foreground" />
          {!collapsed && (
            <>
              <span className="min-w-0 flex-1 truncate text-sm font-medium">{selectedMailbox?.address || "选择邮箱"}</span>
              <ChevronsUpDown className="h-4 w-4 shrink-0 text-muted-foreground" />
            </>
          )}
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" className="w-72">
        {mailboxes.length === 0 && <DropdownMenuItem disabled>没有可用邮箱</DropdownMenuItem>}
        {mailboxes.map((mailbox) => (
          <DropdownMenuItem key={mailbox.id} onSelect={() => onSelect(mailbox.id)} className="gap-2">
            <Check className={cn("h-4 w-4", selectedMailbox?.id === mailbox.id ? "opacity-100" : "opacity-0")} />
            <span className="min-w-0 flex-1 truncate font-medium">{mailbox.address}</span>
          </DropdownMenuItem>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

function cleanAccountName(name: string, email?: string) {
  const value = name.trim()
  if (!value || (email && value.toLowerCase() === email.toLowerCase())) return email?.split("@")[0] || "用户"
  return value
}

function accountInitial(name: string, email?: string) {
  const source = cleanAccountName(name, email)
  const first = Array.from(source.trim())[0]
  return (first || "蓝").toUpperCase()
}

function senderDisplayName(message: MailMessage) {
  const fromName = message.fromName?.trim()
  if (fromName) return fromName
  return displayNameFromAddress(message.from)
}

function displayNameFromAddress(value: string) {
  const text = value.trim()
  const namedAddress = text.match(/^"?([^"<]+?)"?\s*<[^>]+>$/)
  const name = namedAddress?.[1]?.trim()
  if (name) return name
  const address = text.match(/<([^>]+)>/)?.[1]?.trim() || text
  const localPart = address.split("@")[0]?.trim()
  return localPart || text || "未知发件人"
}

function senderTitle(message: MailMessage) {
  const name = message.fromName?.trim()
  return name ? `${name} <${message.from}>` : message.from
}

function MessageRow({
  message,
  active,
  checked,
  onCheckedChange,
  onClick,
  onStar,
}: {
  message: MailMessage
  active: boolean
  checked: boolean
  onCheckedChange: (checked: boolean) => void
  onClick: () => void
  onStar: () => void
}) {
  const visibleLabels = (message.labels || []).slice(0, 2)
  const hiddenLabelCount = Math.max((message.labels?.length || 0) - visibleLabels.length, 0)
  const senderName = senderDisplayName(message)
  return <div onClick={onClick} className={cn("cursor-pointer border-b p-4 transition-colors hover:bg-accent/50", active && "bg-accent", !message.isRead && "font-semibold")}>
    <div className="flex gap-3">
      <Checkbox
        aria-label="选择邮件"
        checked={checked}
        onCheckedChange={(value) => onCheckedChange(value === true)}
        onClick={(event) => event.stopPropagation()}
        className="mt-0.5 shrink-0"
      />
      <div className="min-w-0 flex-1">
        <div className="mb-1 flex items-center justify-between gap-2">
          <div className="min-w-0 truncate text-sm" title={senderTitle(message)}>{senderName}</div>
          <div className="flex shrink-0 items-center gap-1">
            <Button
              type="button"
              variant="ghost"
              size="icon"
              aria-label={message.isStarred ? "取消星标" : "添加星标"}
              className="h-7 w-7 text-muted-foreground hover:text-yellow-500"
              onClick={(e) => { e.stopPropagation(); onStar() }}
            >
              <Star className={cn("h-4 w-4", message.isStarred && "fill-yellow-400 text-yellow-500")} />
            </Button>
            <div className="text-xs text-muted-foreground">{formatDate(message.receivedAt)}</div>
          </div>
        </div>
        <div className="mb-1 flex min-w-0 items-center gap-2">
          <span className="min-w-0 truncate text-sm">{message.subject}</span>
          {visibleLabels.map((label) => <MailLabelBadge key={label.id} label={label} compact />)}
          {hiddenLabelCount > 0 && <Badge variant="outline" className="h-5 shrink-0 rounded-md px-1.5 text-[11px] font-normal text-muted-foreground">+{hiddenLabelCount}</Badge>}
          {message.hasAttachments && <Paperclip className="h-3 w-3 shrink-0 text-muted-foreground" />}
        </div>
        <div className="line-clamp-2 text-xs text-muted-foreground">{message.snippet}</div>
      </div>
    </div>
  </div>
}

function MailLabelBadge({ label, compact }: { label: MailLabel; compact?: boolean }) {
  return (
    <Badge
      variant="outline"
      className={cn("shrink-0 rounded-md font-normal", compact ? "h-5 px-1.5 text-[11px]" : "h-8 px-2 text-xs")}
      style={{ borderColor: label.color, color: label.color }}
    >
      {label.name}
    </Badge>
  )
}

function MessageLabels({ messageLabels, availableLabels, onAdd, onRemove, pending }: { messageLabels: MailLabel[]; availableLabels: MailLabel[]; onAdd: (label: MailLabel) => void; onRemove: (labelId: string) => void; pending: boolean }) {
  const activeIds = new Set(messageLabels.map((label) => label.id))
  return (
    <div className="mt-4 space-y-2">
      <div className="flex items-center gap-2 text-xs font-medium text-muted-foreground"><Tag className="h-3.5 w-3.5" />标签</div>
      <div className="flex flex-wrap items-center gap-2">
        {messageLabels.map((label) => (
          <Badge key={label.id} variant="outline" className="h-8 gap-1.5 rounded-md px-2 text-xs font-normal" style={{ borderColor: label.color, color: label.color }}>
            <span>{label.name}</span>
            <Button type="button" variant="ghost" size="icon" className="h-4 w-4 rounded-full p-0 hover:bg-black/5" onClick={() => onRemove(label.id)} disabled={pending}>
              <X className="h-3 w-3" />
            </Button>
          </Badge>
        ))}
        {messageLabels.length === 0 && <span className="text-xs text-muted-foreground">无标签</span>}
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button type="button" variant="outline" size="sm" disabled={pending}>
              <Tag className="h-4 w-4" />管理标签
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="start" className="w-52">
            {availableLabels.length === 0 && <DropdownMenuItem disabled>请先在侧栏新建标签</DropdownMenuItem>}
            {availableLabels.map((label) => (
              <DropdownMenuCheckboxItem
                key={label.id}
                checked={activeIds.has(label.id)}
                onSelect={(event) => {
                  event.preventDefault()
                  activeIds.has(label.id) ? onRemove(label.id) : onAdd(label)
                }}
              >
                <span className="mr-2 h-2.5 w-2.5 rounded-full" style={{ backgroundColor: label.color }} />
                <span>{label.name}</span>
              </DropdownMenuCheckboxItem>
            ))}
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
    </div>
  )
}

function ComposeDialog({ mailbox, open, draft, onOpenChange, onSent }: { mailbox?: Mailbox; open: boolean; draft?: ComposeDraft; onOpenChange: (v: boolean) => void; onSent: () => void }) {
  const { toast } = useToast()
  const [files, setFiles] = React.useState<File[]>([])
  const [showCc, setShowCc] = React.useState(Boolean(draft?.cc))
  const [showBcc, setShowBcc] = React.useState(Boolean(draft?.bcc))
  const [sendSeparately, setSendSeparately] = React.useState(false)
  const defaultSignature = useQuery({ queryKey: ["signature", "default", mailbox?.id], queryFn: () => api.defaultSignature(mailbox?.id), enabled: open && !!mailbox?.id })
  const signatureText = defaultSignature.data?.signature?.content || ""
  const composerText = draft?.text !== undefined ? draft.text : signatureText ? `\n\n-- \n${signatureText}` : ""
  const [body, setBody] = React.useState<ComposerValue>(() => plainTextComposerValue(composerText))
  const send = useMutation({
    mutationFn: async (payloads: SendPayload[]) => {
      const sent: MailMessage[] = []
      for (const payload of payloads) sent.push(await api.send(payload))
      return sent
    },
    onSuccess: (_, payloads) => {
      toast({ title: payloads.length > 1 ? `已分别发送 ${payloads.length} 封邮件` : "发送成功" })
      setFiles([])
      onSent()
    },
    onError: (e) => toast({ title: "发送失败", description: e.message }),
  })

  React.useEffect(() => {
    if (!open) return
    setShowCc(Boolean(draft?.cc))
    setShowBcc(Boolean(draft?.bcc))
    setSendSeparately(false)
    setFiles([])
  }, [open, draft?.key, draft?.cc, draft?.bcc])

  async function submit(e: React.FormEvent<HTMLFormElement>) {
    e.preventDefault()
    if (!mailbox) {
      toast({ title: "请选择发件邮箱" })
      return
    }
    const form = new FormData(e.currentTarget)
    const attachments = await Promise.all(files.map(fileToAttachment))
    const to = splitEmails(String(form.get("to") || ""))
    const cc = showCc ? splitEmails(String(form.get("cc") || "")) : []
    const bcc = showBcc ? splitEmails(String(form.get("bcc") || "")) : []
    const text = body.text
    const html = body.html || plainTextToHtml(text)
    const payload: SendPayload = { mailboxId: mailbox.id, to, cc, bcc, subject: String(form.get("subject") || ""), text, html, attachments }
    const separateRecipients = Array.from(new Set([...to, ...cc, ...bcc]))
    const payloads = sendSeparately && separateRecipients.length > 0
      ? separateRecipients.map((recipient): SendPayload => ({ ...payload, to: [recipient], cc: [], bcc: [] }))
      : [payload]
    send.mutate(payloads)
  }
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="w-[min(96vw,82rem)] max-w-none overflow-hidden p-0">
        <form key={draft?.key || "new"} className="flex max-h-[90vh] flex-col" onSubmit={submit}>
          <DialogHeader className="border-b px-6 py-4 text-left">
            <DialogTitle>写信</DialogTitle>
          </DialogHeader>
          <div className="flex-1 overflow-y-auto">
            <ComposeField label="发件邮箱">
              <Input value={mailbox?.address || "未选择"} readOnly className="h-10 flex-1 rounded-none border-0 px-0 shadow-none focus-visible:ring-0" />
            </ComposeField>
            <ComposeField
              label="收件人"
              action={
                <div className="flex shrink-0 flex-wrap items-center justify-end gap-2 text-sm">
                  <Button type="button" variant="ghost" size="sm" className="h-8 px-2 font-normal" onClick={() => setShowCc((value) => !value)}>抄送</Button>
                  <Button type="button" variant="ghost" size="sm" className="h-8 px-2 font-normal" onClick={() => setShowBcc((value) => !value)}>密送</Button>
                  <div className="flex items-center gap-2 rounded-md px-2 py-1">
                    <Checkbox id="compose-send-separately" checked={sendSeparately} onCheckedChange={(value) => setSendSeparately(value === true)} />
                    <Label htmlFor="compose-send-separately" className="cursor-pointer text-sm font-normal">分别发送</Label>
                  </div>
                </div>
              }
            >
              <Input name="to" placeholder="name@example.com，多个地址用逗号或空格分隔" defaultValue={draft?.to || ""} required className="h-10 flex-1 rounded-none border-0 px-0 shadow-none focus-visible:ring-0" />
            </ComposeField>
            {showCc && (
              <ComposeField label="抄送">
                <Input name="cc" placeholder="cc@example.com" defaultValue={draft?.cc || ""} className="h-10 flex-1 rounded-none border-0 px-0 shadow-none focus-visible:ring-0" />
              </ComposeField>
            )}
            {showBcc && (
              <ComposeField label="密送">
                <Input name="bcc" placeholder="bcc@example.com" defaultValue={draft?.bcc || ""} className="h-10 flex-1 rounded-none border-0 px-0 shadow-none focus-visible:ring-0" />
              </ComposeField>
            )}
            <ComposeField label="主　题">
              <Input name="subject" placeholder="输入主题" defaultValue={draft?.subject || ""} className="h-10 flex-1 rounded-none border-0 px-0 shadow-none focus-visible:ring-0" />
            </ComposeField>
            <MailBodyComposer
              defaultValue={composerText}
              files={files}
              signatureText={signatureText}
              onChange={setBody}
              onPickFiles={(nextFiles) => setFiles((current) => [...current, ...nextFiles])}
              onRemoveFile={(index) => setFiles((current) => current.filter((_, itemIndex) => itemIndex !== index))}
            />
          </div>
          <DialogFooter className="border-t bg-background px-6 py-4">
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>取消</Button>
            <Button disabled={send.isPending || !mailbox}><Send className="h-4 w-4" />{send.isPending ? "发送中..." : "发送"}</Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function ComposeField({ label, children, action }: { label: string; children: React.ReactNode; action?: React.ReactNode }) {
  return (
    <div className="flex min-h-14 flex-col gap-2 border-b px-6 py-2 sm:flex-row sm:items-center">
      <Label className="w-20 shrink-0 text-base font-normal text-foreground">{label}</Label>
      <div className="flex min-w-0 flex-1 flex-col gap-2 sm:flex-row sm:items-center">
        {children}
        {action}
      </div>
    </div>
  )
}

type ComposerValue = { text: string; html: string }

function MailBodyComposer({ defaultValue, files, signatureText, onChange, onPickFiles, onRemoveFile }: { defaultValue: string; files: File[]; signatureText: string; onChange: (value: ComposerValue) => void; onPickFiles: (files: File[]) => void; onRemoveFile: (index: number) => void }) {
  const editorRef = React.useRef<HTMLDivElement>(null)
  const fileInputRef = React.useRef<HTMLInputElement>(null)
  const initialHtmlRef = React.useRef(plainTextToHtml(defaultValue))
  const dirtyRef = React.useRef(false)
  const lastDefaultRef = React.useRef(defaultValue)
  const [formatOpen, setFormatOpen] = React.useState(true)
  const [empty, setEmpty] = React.useState(!defaultValue.trim())

  React.useEffect(() => {
    const next = plainTextComposerValue(defaultValue)
    onChange(next)
    setEmpty(!next.text.trim())
  }, [])

  React.useEffect(() => {
    if (defaultValue === lastDefaultRef.current) return
    lastDefaultRef.current = defaultValue
    if (!dirtyRef.current) {
      const next = plainTextComposerValue(defaultValue)
      if (editorRef.current) editorRef.current.innerHTML = next.html
      setEmpty(!next.text.trim())
      onChange(next)
    }
  }, [defaultValue, onChange])

  function focusEditor() {
    window.requestAnimationFrame(() => editorRef.current?.focus())
  }

  function syncEditor() {
    const next = composerValueFromElement(editorRef.current)
    setEmpty(!next.text.trim())
    onChange(next)
  }

  function runCommand(command: string, value?: string) {
    dirtyRef.current = true
    editorRef.current?.focus()
    document.execCommand(command, false, value)
    syncEditor()
  }

  function insertLink() {
    const url = window.prompt("链接地址")
    if (!url) return
    const selected = window.getSelection()?.toString()
    if (selected) runCommand("createLink", url)
    else runCommand("insertHTML", `<a href="${escapeHtml(url)}">${escapeHtml(url)}</a>`)
  }

  function insertImage() {
    const url = window.prompt("图片链接")
    if (url) runCommand("insertImage", url)
  }

  function insertSignature() {
    if (!signatureText.trim()) return
    runCommand("insertHTML", `<br><br>-- <br>${plainTextToHtmlFragment(signatureText)}`)
  }

  function handlePickedFiles(event: React.ChangeEvent<HTMLInputElement>) {
    const nextFiles = Array.from(event.currentTarget.files || [])
    if (nextFiles.length > 0) onPickFiles(nextFiles)
    event.currentTarget.value = ""
  }

  return (
    <div className="min-h-[420px]">
      <Input ref={fileInputRef} type="file" multiple className="hidden" onChange={handlePickedFiles} />
      <div className="flex min-h-11 flex-wrap items-center gap-1 border-b px-6 py-2">
        <ToolbarButton label="撤销" onClick={() => runCommand("undo")}><Undo2 className="h-4 w-4" /></ToolbarButton>
        <ToolbarButton label="重做" onClick={() => runCommand("redo")}><Redo2 className="h-4 w-4" /></ToolbarButton>
        <Separator orientation="vertical" className="mx-2 h-6" />
        <ToolbarTextButton label="图片" icon={<Image className="h-4 w-4" />} onClick={insertImage} />
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button type="button" variant="ghost" size="sm" className="h-8 gap-1 rounded-md px-2 font-normal">
              <Plus className="h-4 w-4" />插入<ChevronDown className="h-3.5 w-3.5" />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="start">
            <DropdownMenuItem onSelect={() => fileInputRef.current?.click()}><Paperclip className="h-4 w-4" />附件</DropdownMenuItem>
            <DropdownMenuItem onSelect={insertLink}><Link className="h-4 w-4" />链接</DropdownMenuItem>
            <DropdownMenuItem onSelect={insertImage}><Image className="h-4 w-4" />图片链接</DropdownMenuItem>
            <DropdownMenuItem onSelect={() => runCommand("insertHorizontalRule")}><span className="h-4 w-4 border-t border-current" aria-hidden />分隔线</DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
        <ToolbarTextButton label="导入文档" icon={<FileText className="h-4 w-4" />} onClick={() => fileInputRef.current?.click()} />
        <ToolbarTextButton label="日程" icon={<Calendar className="h-4 w-4" />} onClick={() => runCommand("insertText", "\n[日程]\n")} />
        <ToolbarTextButton label="表情" icon={<Smile className="h-4 w-4" />} onClick={() => runCommand("insertText", "🙂")} />
        <ToolbarTextButton label="格式" icon={<Type className="h-4 w-4" />} active={formatOpen} onClick={() => setFormatOpen((value) => !value)} />
        <div className="ml-auto flex items-center gap-1">
          <ToolbarTextButton label="签名" icon={<Signature className="h-4 w-4" />} onClick={insertSignature} disabled={!signatureText.trim()} />
          <ToolbarButton label="更多"><Ellipsis className="h-4 w-4" /></ToolbarButton>
        </div>
      </div>
      {formatOpen && (
        <div className="flex min-h-14 flex-wrap items-center gap-1 border-b bg-muted/40 px-6 py-2">
          <ToolbarButton label="清除格式" onClick={() => runCommand("removeFormat")}><Eraser className="h-4 w-4" /></ToolbarButton>
          <Separator orientation="vertical" className="mx-2 h-6" />
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button type="button" variant="ghost" size="sm" className="h-8 min-w-[112px] justify-between rounded-md px-2 font-normal">
                默认字体<ChevronDown className="h-3.5 w-3.5" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="start">
              {["Arial", "Georgia", "Times New Roman", "Courier New"].map((font) => (
                <DropdownMenuItem key={font} onSelect={() => runCommand("fontName", font)}>{font}</DropdownMenuItem>
              ))}
            </DropdownMenuContent>
          </DropdownMenu>
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button type="button" variant="ghost" size="sm" className="h-8 min-w-[84px] justify-between rounded-md px-2 font-normal">
                字号<ChevronDown className="h-3.5 w-3.5" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="start">
              {[["2", "小号"], ["3", "正文"], ["4", "中号"], ["5", "大号"]].map(([size, label]) => (
                <DropdownMenuItem key={size} onSelect={() => runCommand("fontSize", size)}>{label}</DropdownMenuItem>
              ))}
            </DropdownMenuContent>
          </DropdownMenu>
          <Separator orientation="vertical" className="mx-2 h-6" />
          <ToolbarButton label="加粗" onClick={() => runCommand("bold")}><Bold className="h-4 w-4" /></ToolbarButton>
          <ToolbarButton label="斜体" onClick={() => runCommand("italic")}><Italic className="h-4 w-4" /></ToolbarButton>
          <ToolbarButton label="下划线" onClick={() => runCommand("underline")}><Underline className="h-4 w-4" /></ToolbarButton>
          <ToolbarButton label="删除线" onClick={() => runCommand("strikeThrough")}><Strikethrough className="h-4 w-4" /></ToolbarButton>
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button type="button" variant="ghost" size="icon" className="h-8 w-8 rounded-md text-muted-foreground hover:text-foreground" title="文字颜色" aria-label="文字颜色">
                <Type className="h-4 w-4" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="start" className="w-36">
              {[["#111827", "默认"], ["#dc2626", "红色"], ["#2563eb", "蓝色"], ["#16a34a", "绿色"]].map(([color, label]) => (
                <DropdownMenuItem key={color} onSelect={() => runCommand("foreColor", color)}>
                  <span className="h-3 w-3 rounded-full border" style={{ backgroundColor: color }} />{label}
                </DropdownMenuItem>
              ))}
            </DropdownMenuContent>
          </DropdownMenu>
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button type="button" variant="ghost" size="icon" className="h-8 w-8 rounded-md text-muted-foreground hover:text-foreground" title="高亮" aria-label="高亮">
                <Highlighter className="h-4 w-4" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="start">
              {[["transparent", "无高亮"], ["#fef3c7", "黄色"], ["#dcfce7", "绿色"], ["#dbeafe", "蓝色"]].map(([color, label]) => (
                <DropdownMenuItem key={color} onSelect={() => runCommand("backColor", color)}>
                  <span className="h-3 w-3 rounded-sm border" style={{ backgroundColor: color }} />{label}
                </DropdownMenuItem>
              ))}
            </DropdownMenuContent>
          </DropdownMenu>
          <Separator orientation="vertical" className="mx-2 h-6" />
          <ToolbarButton label="无序列表" onClick={() => runCommand("insertUnorderedList")}><List className="h-4 w-4" /></ToolbarButton>
          <ToolbarButton label="有序列表" onClick={() => runCommand("insertOrderedList")}><ListOrdered className="h-4 w-4" /></ToolbarButton>
          <ToolbarButton label="减少缩进" onClick={() => runCommand("outdent")}><IndentDecrease className="h-4 w-4" /></ToolbarButton>
          <ToolbarButton label="增加缩进" onClick={() => runCommand("indent")}><IndentIncrease className="h-4 w-4" /></ToolbarButton>
          <Separator orientation="vertical" className="mx-2 h-6" />
          <ToolbarButton label="左对齐" onClick={() => runCommand("justifyLeft")}><AlignLeft className="h-4 w-4" /></ToolbarButton>
          <ToolbarButton label="居中" onClick={() => runCommand("justifyCenter")}><AlignCenter className="h-4 w-4" /></ToolbarButton>
          <ToolbarButton label="右对齐" onClick={() => runCommand("justifyRight")}><AlignRight className="h-4 w-4" /></ToolbarButton>
          <ToolbarButton label="引用" onClick={() => runCommand("formatBlock", "blockquote")}><Quote className="h-4 w-4" /></ToolbarButton>
          <ToolbarButton label="代码块" onClick={() => runCommand("formatBlock", "pre")}><Code2 className="h-4 w-4" /></ToolbarButton>
        </div>
      )}
      <div className="relative">
        {empty && <div className="pointer-events-none absolute left-6 top-6 text-base text-muted-foreground">输入正文</div>}
        <div
          ref={editorRef}
          role="textbox"
          aria-label="正文"
          contentEditable
          suppressContentEditableWarning
          className="mail-html min-h-[280px] overflow-y-auto px-6 py-5 text-base leading-7 outline-none"
          onFocus={focusEditor}
          onInput={() => { dirtyRef.current = true; syncEditor() }}
          onBlur={syncEditor}
          dangerouslySetInnerHTML={{ __html: initialHtmlRef.current }}
        />
      </div>
      {files.length > 0 && (
        <div className="border-t px-6 py-3">
          <div className="flex flex-wrap gap-2">
            {files.map((file, index) => (
              <Badge key={`${file.name}-${file.size}-${index}`} variant="outline" className="h-8 gap-2 rounded-md px-2 font-normal">
                <Paperclip className="h-3.5 w-3.5" />
                <span className="max-w-48 truncate">{file.name}</span>
                <span className="text-muted-foreground">{formatBytes(file.size)}</span>
                <Button type="button" variant="ghost" size="icon" className="h-5 w-5 rounded-md" onClick={() => onRemoveFile(index)}>
                  <X className="h-3.5 w-3.5" />
                </Button>
              </Badge>
            ))}
          </div>
        </div>
      )}
    </div>
  )
}

function ToolbarTextButton({ label, icon, active, disabled, onClick }: { label: string; icon: React.ReactNode; active?: boolean; disabled?: boolean; onClick?: () => void }) {
  return (
    <Button type="button" variant={active ? "secondary" : "ghost"} size="sm" className="h-8 gap-1.5 rounded-md px-2 font-normal" title={label} aria-label={label} onClick={onClick} disabled={disabled}>
      {icon}{label}
    </Button>
  )
}

function ToolbarButton({ label, children, onClick, disabled }: { label: string; children: React.ReactNode; onClick?: () => void; disabled?: boolean }) {
  return <Button type="button" variant="ghost" size="icon" className="h-8 w-8 rounded-md text-muted-foreground hover:text-foreground" title={label} aria-label={label} onClick={onClick} disabled={disabled}>{children}</Button>
}

function splitEmails(s: string) { return s.split(/[;,，\s]+/).map((v) => v.trim()).filter(Boolean) }
function plainTextComposerValue(value: string): ComposerValue { return { text: value, html: plainTextToHtml(value) } }
function composerValueFromElement(element: HTMLElement | null): ComposerValue {
  const text = (element?.innerText || "").replace(/\u00a0/g, " ").trimEnd()
  if (!text.trim()) return { text: "", html: "" }
  const html = DOMPurify.sanitize(element?.innerHTML || "")
  return { text, html: html || plainTextToHtml(text) }
}
function plainTextToHtml(value: string) {
  const normalized = value.replace(/\r\n/g, "\n")
  if (!normalized.trim()) return ""
  return DOMPurify.sanitize(normalized.split(/\n{2,}/).map((paragraph) => `<p>${plainTextToHtmlFragment(paragraph) || "<br>"}</p>`).join(""))
}
function plainTextToHtmlFragment(value: string) { return value.split("\n").map((line) => escapeHtml(line)).join("<br>") }
function escapeHtml(value: string) {
  return value.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;").replace(/'/g, "&#39;")
}
function playIncomingMailSound(ref: React.MutableRefObject<AudioContext | null>) {
  const AudioContextCtor = window.AudioContext || (window as unknown as { webkitAudioContext?: typeof AudioContext }).webkitAudioContext
  if (!AudioContextCtor) return
  const ctx = ref.current || new AudioContextCtor()
  ref.current = ctx
  if (ctx.state === "suspended") void ctx.resume()
  const now = ctx.currentTime
  const gain = ctx.createGain()
  gain.gain.setValueAtTime(0.0001, now)
  gain.gain.exponentialRampToValueAtTime(0.12, now + 0.02)
  gain.gain.exponentialRampToValueAtTime(0.0001, now + 0.45)
  gain.connect(ctx.destination)
  for (const [index, frequency] of [880, 1175].entries()) {
    const osc = ctx.createOscillator()
    const start = now + index * 0.14
    osc.type = "sine"
    osc.frequency.setValueAtTime(frequency, start)
    osc.connect(gain)
    osc.start(start)
    osc.stop(start + 0.18)
  }
}
function withPrefix(subject: string, prefix: string) { return subject.toLowerCase().startsWith(prefix.toLowerCase()) ? subject : `${prefix} ${subject}` }
function quoteMessage(message: MailMessage) {
  const body = message.bodyText || stripHtml(message.bodyHtml || message.snippet || "")
  const quote = body.split("\n").map((line) => `> ${line}`).join("\n")
  return `\n\n----- 原始邮件 -----\nFrom: ${senderTitle(message)}\nTo: ${message.to.join(", ")}\nDate: ${formatDateTime(message.receivedAt)}\nSubject: ${message.subject}\n\n${quote}`
}
function stripHtml(html: string) { const div = document.createElement("div"); div.innerHTML = DOMPurify.sanitize(html); return div.textContent || div.innerText || "" }
async function fileToAttachment(file: File) {
  const buffer = await file.arrayBuffer()
  let binary = ""
  const bytes = new Uint8Array(buffer)
  for (let i = 0; i < bytes.length; i++) binary += String.fromCharCode(bytes[i])
  return { filename: file.name, contentType: file.type || "application/octet-stream", contentBase64: btoa(binary) }
}

import * as React from "react"
import DOMPurify from "dompurify"
import { type InfiniteData, useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Node, mergeAttributes, type Editor } from "@tiptap/core"
import { DOMParser as ProseMirrorDOMParser } from "@tiptap/pm/model"
import { EditorContent, useEditor } from "@tiptap/react"
import StarterKit from "@tiptap/starter-kit"
import LinkExtension from "@tiptap/extension-link"
import ImageExtension from "@tiptap/extension-image"
import TextAlign from "@tiptap/extension-text-align"
import Placeholder from "@tiptap/extension-placeholder"
import { BackgroundColor, Color, FontFamily, FontSize, TextStyle } from "@tiptap/extension-text-style"
import { useNavigate } from "react-router-dom"
import type { ImperativePanelHandle } from "react-resizable-panels"
import { AlignCenter, AlignLeft, AlignRight, Archive, ArrowLeft, Bold, Calendar, Check, ChevronDown, ChevronsUpDown, Clock3, Code2, Copy, Ellipsis, Eraser, Eye, FileText, Forward, Highlighter, Image, Inbox, IndentDecrease, IndentIncrease, Italic, Link, List, ListOrdered, Mail, MailCheck, Moon, PanelLeftClose, PanelLeftOpen, Paperclip, PencilLine, Plus, Quote, Redo2, RefreshCcw, Reply, Search, Send, Settings, Signature, SlidersHorizontal, Smile, Star, Strikethrough, Sun, Tag, Trash2, Type, Underline, Undo2, X } from "lucide-react"
import { api, Mailbox, MailFolder, MailLabel, MailMessage, SendPayload, DraftPayload, ScheduledSend } from "@/lib/api"
import { cn, decodeMimeHeader, formatBytes, formatDate, formatDateTime } from "@/lib/utils"
import { applyTheme, getInitialTheme } from "@/lib/theme"
import { useDisplayMode } from "@/lib/display-mode"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Badge } from "@/components/ui/badge"
import { Checkbox } from "@/components/ui/checkbox"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Sheet, SheetContent, SheetTitle, SheetTrigger } from "@/components/ui/sheet"
import { Label } from "@/components/ui/label"
import { DropdownMenu, DropdownMenuCheckboxItem, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
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
import { useIsMobile } from "@/hooks/use-mobile"
import { useToast } from "@/hooks/use-toast"

const folderIcons: Record<string, React.ReactNode> = { inbox: <Inbox className="h-4 w-4" />, sent: <Send className="h-4 w-4" />, drafts: <FileText className="h-4 w-4" />, archive: <Archive className="h-4 w-4" />, spam: <Trash2 className="h-4 w-4" />, trash: <Trash2 className="h-4 w-4" /> }
const folderLabels: Record<string, string> = {
  Inbox: "收件箱",
  Sent: "已发送",
  Drafts: "草稿箱",
  Archive: "归档",
  Spam: "垃圾邮件",
  Trash: "回收站",
}

type ComposeDraft = { key: string; id?: string; mailboxId?: string; to?: string; cc?: string; bcc?: string; subject?: string; text?: string; html?: string; files?: File[]; isDraft?: boolean }
type MailFilter = "all" | "unread" | "starred" | "attachments"
type MailView = "folder" | "starred" | "label" | "scheduled"
type MailListResponse = { items?: MailMessage[]; nextCursor?: string }
type PendingConfirm = { title: string; description?: string; confirmText: string; onConfirm: () => void }
type MailNotificationState = { latestId: string; latestReceivedAt: string }
type ComposeSendIntent = { title: string; description: string; confirmText: string; onConfirm: () => void }
type MailMenuItem =
  | { type: "starred"; key: string; label: string; icon: React.ReactNode; count: number }
  | { type: "scheduled"; key: string; label: string; icon: React.ReactNode; count: number }
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
  const isMobile = useIsMobile()
  const [refreshing, setRefreshing] = React.useState(false)
  const [autoRefreshing, setAutoRefreshing] = React.useState(false)
  const [lastAutoRefreshAt, setLastAutoRefreshAt] = React.useState<Date | null>(null)
  const [bulkPending, setBulkPending] = React.useState(false)
  const [pendingConfirm, setPendingConfirm] = React.useState<PendingConfirm | null>(null)
  const [cancelingScheduledId, setCancelingScheduledId] = React.useState("")
  const [mobileSidebarOpen, setMobileSidebarOpen] = React.useState(false)
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
  const scheduledSends = useQuery({ queryKey: ["scheduled-sends", activeMailboxId], queryFn: () => api.scheduledSends(activeMailboxId), enabled: !!activeMailboxId, refetchInterval: 30000 })
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
    enabled: !!activeMailboxId && mailView !== "scheduled" && (mailView !== "label" || !!selectedLabelId),
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
  const cancelScheduledSend = useMutation({
    mutationFn: (item: ScheduledSend) => api.cancelScheduledSend(item.id),
    onMutate: (item) => setCancelingScheduledId(item.id),
    onSuccess: async (_, item) => {
      await qc.invalidateQueries({ queryKey: ["scheduled-sends"] })
      toast({ title: item.status === "failed" ? "已移除失败记录" : "已取消定时发送" })
    },
    onError: (error) => toast({ title: "操作失败", description: error instanceof Error ? error.message : "请稍后重试" }),
    onSettled: () => setCancelingScheduledId(""),
  })
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
        qc.invalidateQueries({ queryKey: ["scheduled-sends"] }),
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
  const scheduledItems = scheduledSends.data?.items || []
  const scheduledDraftIds = new Set(scheduledItems.map((item) => item.draftId).filter((draftId): draftId is string => Boolean(draftId)))
  const scheduledCount = scheduledItems.length
  const scheduledQuery = query.trim().toLowerCase()
  const visibleScheduledItems = scheduledQuery
    ? scheduledItems.filter((item) => [item.subject, item.snippet, ...(item.to || [])].join(" ").toLowerCase().includes(scheduledQuery))
    : scheduledItems
  const mailMenuItems = buildMailMenuItems(folders.data?.items || [], starredCount, scheduledCount)
  const labelItems = labels.data?.items || []
  const selectedLabel = labelItems.find((item) => item.id === selectedLabelId)
  const viewTitle = mailView === "scheduled" ? "待发送" : mailView === "starred" ? "星标邮件" : mailView === "label" ? selectedLabel?.name || "标签" : folderLabels[folder] || folder
  const emptyMessage = getEmptyMessage(mailView, folder, allMessages.length)
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
      qc.invalidateQueries({ queryKey: ["scheduled-sends"] }),
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
  async function openDraft(message: MailMessage) {
    if (scheduledDraftIds.has(message.id)) {
      toast({ title: "这封草稿已在待发送队列中", description: "请先取消定时发送，再继续编辑。" })
      openScheduled()
      return
    }
    try {
      const detail = await api.message(message.id, { markRead: false })
      openCompose({
        key: `draft-${detail.id}-${Date.now()}`,
        id: detail.id,
        mailboxId: detail.mailboxId,
        to: detail.to.join(", "),
        cc: detail.cc.join(", "),
        bcc: (detail.bcc || []).join(", "),
        subject: detail.subject === "(无主题)" ? "" : detail.subject,
        text: detail.bodyText || "",
        html: detail.bodyHtml || "",
        files: await attachmentFilesFromMessage(detail),
        isDraft: true,
      })
      setSelectedId(null)
    } catch (error) {
      toast({ title: "打开草稿失败", description: error instanceof Error ? error.message : "请稍后重试" })
    }
  }
  function switchMailbox(mailboxId: string) {
    setSelectedMailboxId(mailboxId)
    setFolder("Inbox")
    setMailView("folder")
    setSelectedLabelId("")
    setSelectedId(null)
    setMailFilter("all")
    setMobileSidebarOpen(false)
  }
  function openFolder(nextFolder: string) {
    setFolder(nextFolder)
    setMailView("folder")
    setSelectedLabelId("")
    setSelectedId(null)
    setMailFilter("all")
    setMobileSidebarOpen(false)
  }
  function openStarred() {
    setMailView("starred")
    setSelectedLabelId("")
    setSelectedId(null)
    setMailFilter("all")
    setMobileSidebarOpen(false)
  }
  function openScheduled() {
    setMailView("scheduled")
    setSelectedLabelId("")
    setSelectedId(null)
    setMailFilter("all")
    setMobileSidebarOpen(false)
  }
  function openLabel(labelId: string) {
    setSelectedLabelId(labelId)
    setMailView("label")
    setSelectedId(null)
    setMailFilter("all")
    setMobileSidebarOpen(false)
  }
  function openMessage(messageId: string | null) {
    if (!messageId) {
      setSelectedId(null)
      return
    }
    const message = allMessages.find((item) => item.id === messageId)
    if (message?.folder === "Drafts") {
      void openDraft(message)
      return
    }
    setSelectedId(messageId)
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
  const sidebarContent = (
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
                    isActive={item.type === "starred" ? mailView === "starred" : item.type === "scheduled" ? mailView === "scheduled" : mailView === "folder" && folder === item.folderName}
                    className={cn(sidebarCollapsed && "justify-center px-0")}
                    onClick={() => item.type === "starred" ? openStarred() : item.type === "scheduled" ? openScheduled() : openFolder(item.folderName)}
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
      {!isMobile && (
        <div className={cn("mt-auto border-t p-2", sidebarCollapsed ? "flex justify-center" : "")}>
          <Button type="button" variant="ghost" size={sidebarCollapsed ? "icon" : "sm"} className={cn(!sidebarCollapsed && "w-full justify-start")} onClick={toggleSidebar}>
            {sidebarCollapsed ? <PanelLeftOpen className="h-4 w-4" /> : <PanelLeftClose className="h-4 w-4" />}
            {!sidebarCollapsed && <span>收起侧栏</span>}
          </Button>
        </div>
      )}
    </Sidebar>
  )
  function toggleSidebar() {
    if (sidebarCollapsed) {
      sidebarPanelRef.current?.expand(14)
      setSidebarCollapsed(false)
    } else {
      sidebarPanelRef.current?.collapse()
      setSidebarCollapsed(true)
    }
  }

  const contentView = !mailboxList.isLoading && !hasMailboxes ? (
    <NoMailboxState onOpenSettings={openSettings} />
  ) : mailView === "scheduled" ? (
    <ScheduledSendView
      compact={isMobile || displayMode === "compact"}
      items={visibleScheduledItems}
      total={scheduledItems.length}
      loading={scheduledSends.isLoading}
      query={query}
      cancelingId={cancelingScheduledId}
      onCancel={(item) => cancelScheduledSend.mutate(item)}
    />
  ) : isMobile || displayMode === "compact" ? (
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
      scheduledDraftIds={scheduledDraftIds}
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
            {visibleMessages.map((m) => <MessageRow key={m.id} message={m} active={selectedId === m.id} checked={compactSelectedIds.includes(m.id)} scheduled={scheduledDraftIds.has(m.id)} onCheckedChange={(checked) => toggleCompactSelect(m.id, checked)} onClick={() => openMessage(m.id)} onStar={() => star.mutate({ id: m.id, starred: !m.isStarred })} />)}
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
                <div className="mail-html prose max-w-none text-sm leading-7" dangerouslySetInnerHTML={{ __html: DOMPurify.sanitize(selected.bodyHtml || `<pre>${escapeHtml(selected.bodyText || "")}</pre>`) }} />
                {selected.attachments && selected.attachments.length > 0 && <div className="mt-8 rounded-lg border p-4"><div className="mb-3 font-medium">附件</div><div className="space-y-2">{selected.attachments.map((a) => <a className="flex items-center justify-between rounded-md border p-3 text-sm hover:bg-accent" href={`/api/mail/attachments/${a.id}`} key={a.id}><span className="flex items-center gap-2"><Paperclip className="h-4 w-4" />{a.filename}</span><span className="text-muted-foreground">{formatBytes(a.sizeBytes)}</span></a>)}</div></div>}
              </div>
            </ScrollArea>
          </div>}
        </section>
      </ResizablePanel>
    </ResizablePanelGroup>
  )

  return (
    <div className="h-svh overflow-hidden bg-background">
      <SidebarProvider className="h-full min-h-0 w-full">
        {isMobile ? (
          <div className="flex h-full min-h-0 flex-col">
            {!selectedId && (
              <header className="flex min-h-14 shrink-0 flex-wrap items-center gap-2 border-b px-3 py-2">
                <Sheet open={mobileSidebarOpen} onOpenChange={setMobileSidebarOpen}>
                  <SheetTrigger asChild>
                    <Button size="icon" variant="ghost" aria-label="打开导航"><PanelLeftOpen className="h-4 w-4" /></Button>
                  </SheetTrigger>
                  <SheetContent side="left" className="w-[86vw] max-w-80 p-0 [&>button]:hidden" aria-describedby={undefined}>
                    <SheetTitle className="sr-only">邮箱导航</SheetTitle>
                    <div className="h-svh">{sidebarContent}</div>
                  </SheetContent>
                </Sheet>
                <Button size="icon" variant="ghost" onClick={refreshMail} disabled={refreshing || autoRefreshing} className={cn("transition-all", (refreshing || autoRefreshing) && "bg-primary/5 text-primary")} title={autoRefreshing ? "自动刷新中" : "刷新邮件"}>
                  <RefreshCcw className={cn("h-4 w-4", (refreshing || autoRefreshing) && "animate-spin")} />
                </Button>
                <div className="min-w-0 flex-1 text-sm font-semibold">{viewTitle}</div>
                <Button type="button" size="icon" onClick={() => openCompose()} disabled={!selectedMailbox} aria-label="写邮件"><PencilLine className="h-4 w-4" /></Button>
                <div className="relative basis-full">
                  <Search className="absolute left-3 top-2.5 h-4 w-4 text-muted-foreground" />
                  <Input value={query} onChange={(e) => setQuery(e.target.value)} placeholder={mailView === "scheduled" ? "搜索待发送" : "搜索邮件"} className="h-10 pl-9" />
                </div>
              </header>
            )}
            <section className="min-h-0 flex-1">{contentView}</section>
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
                  <div className="flex items-center gap-2">
                    <Button size="icon" variant="ghost" onClick={refreshMail} disabled={refreshing || autoRefreshing} className={cn("transition-all", (refreshing || autoRefreshing) && "bg-primary/5 text-primary")} title={autoRefreshing ? "自动刷新中" : "刷新邮件"}>
                      <RefreshCcw className={cn("h-4 w-4", (refreshing || autoRefreshing) && "animate-spin")} />
                    </Button>
                    {(publicSettings.data?.mailAutoRefresh || autoRefreshing) && (
                      <div className="hidden min-w-[118px] text-xs text-muted-foreground sm:block">
                        {autoRefreshing ? "自动刷新中..." : lastAutoRefreshAt ? `已刷新 ${lastAutoRefreshAt.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}` : "自动刷新已开启"}
                      </div>
                    )}
                    {mailView !== "scheduled" && (
                      <>
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
                      </>
                    )}
                  </div>
                  <div className="relative w-full max-w-md">
                    <Search className="absolute left-3 top-2.5 h-4 w-4 text-muted-foreground" />
                    <Input value={query} onChange={(e) => setQuery(e.target.value)} placeholder={mailView === "scheduled" ? "搜索待发送" : "搜索邮件"} className="pl-9" />
                  </div>
                </header>
                {contentView}
              </section>
            </ResizablePanel>
          </ResizablePanelGroup>
        )}
      </SidebarProvider>

      <ComposeDialog mailbox={selectedMailbox} open={composeOpen} draft={composeDraft} onOpenChange={(open) => { setComposeOpen(open); if (!open) setComposeDraft(undefined) }} onSent={() => { setComposeOpen(false); setComposeDraft(undefined); qc.invalidateQueries({ queryKey: ["messages"] }); qc.invalidateQueries({ queryKey: ["folders"] }); qc.invalidateQueries({ queryKey: ["mail-stats"] }); qc.invalidateQueries({ queryKey: ["labels"] }); qc.invalidateQueries({ queryKey: ["scheduled-sends"] }) }} />
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

function buildMailMenuItems(folders: MailFolder[], starredCount: number, scheduledCount: number): MailMenuItem[] {
  const byName = new Map(folders.map((item) => [item.name, item]))
  const normalizedFolders = ["Inbox", "Drafts", "Sent", "Archive", "Spam", "Trash"].map((name) => byName.get(name) || { id: `virtual-${name}`, name, role: name.toLowerCase(), unreadCount: 0, totalCount: 0 })
  for (const item of folders) {
    if (!normalizedFolders.some((folder) => folder.name === item.name)) normalizedFolders.push(item)
  }
  const folderItems: MailMenuItem[] = normalizedFolders.map((item) => ({
    type: "folder",
    key: item.id,
    folderName: item.name,
    label: folderLabels[item.name] || item.name,
    icon: folderIcons[item.role] || <Inbox className="h-4 w-4" />,
    count: item.name === "Drafts" ? item.totalCount : item.unreadCount,
  }))
  const starredItem: MailMenuItem = { type: "starred", key: "starred", label: "星标邮件", icon: <Star className="h-4 w-4" />, count: starredCount }
  const scheduledItem: MailMenuItem = { type: "scheduled", key: "scheduled", label: "待发送", icon: <Clock3 className="h-4 w-4" />, count: scheduledCount }
  const inboxIndex = folderItems.findIndex((item) => item.type === "folder" && item.folderName === "Inbox")
  const insertAt = inboxIndex >= 0 ? inboxIndex + 1 : 0
  return [...folderItems.slice(0, insertAt), starredItem, scheduledItem, ...folderItems.slice(insertAt)]
}

function FolderSkeleton() { return <div className="space-y-2 p-2"><Skeleton className="h-8 w-full" /><Skeleton className="h-8 w-4/5" /><Skeleton className="h-8 w-3/4" /></div> }
function MessageSkeleton() { return <div className="space-y-0">{Array.from({ length: 6 }).map((_, i) => <div className="space-y-2 border-b p-4" key={i}><Skeleton className="h-4 w-1/2" /><Skeleton className="h-4 w-4/5" /><Skeleton className="h-3 w-full" /></div>)}</div> }

function getEmptyMessage(mailView: MailView, folder: string, total: number) {
  if (mailView === "scheduled") return total === 0 ? "没有待发送邮件" : "当前搜索没有匹配的定时邮件"
  if (total > 0) return "当前筛选条件下没有邮件"
  if (mailView === "starred") return "暂无星标邮件"
  if (mailView === "label") return "当前标签没有邮件"
  if (folder === "Inbox") return "收件箱暂时为空"
  if (folder === "Drafts") return "还没有草稿"
  if (folder === "Sent") return "还没有已发送邮件"
  if (folder === "Trash") return "回收站是空的"
  if (folder === "Spam") return "暂无垃圾邮件"
  return "当前文件夹没有邮件"
}

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

function ScheduledSendView({ compact, items, total, loading, query, cancelingId, onCancel }: { compact: boolean; items: ScheduledSend[]; total: number; loading: boolean; query: string; cancelingId: string; onCancel: (item: ScheduledSend) => void }) {
  const empty = query.trim() ? "当前搜索没有匹配的定时邮件" : "没有待发送邮件"
  return (
    <div className="flex min-h-0 flex-1 flex-col bg-background">
      <div className={cn("flex shrink-0 items-center justify-between gap-3 border-b", compact ? "h-12 px-4" : "h-14 px-5")}>
        <div className="min-w-0">
          <div className="flex items-center gap-2 text-sm font-semibold"><Clock3 className="h-4 w-4" />待发送</div>
          <div className="text-xs text-muted-foreground">{items.length} / {total} 封定时邮件</div>
        </div>
      </div>
      <ScrollArea className="min-h-0 flex-1">
        {loading && <ScheduledSendSkeleton />}
        {!loading && items.length === 0 && <div className="p-8 text-center text-sm text-muted-foreground">{empty}</div>}
        {!loading && items.map((item) => (
          <ScheduledSendRow key={item.id} item={item} compact={compact} pending={cancelingId === item.id} onCancel={() => onCancel(item)} />
        ))}
      </ScrollArea>
    </div>
  )
}

function ScheduledSendSkeleton() {
  return (
    <div className="space-y-0">
      {Array.from({ length: 4 }).map((_, index) => (
        <div className="space-y-3 border-b p-4" key={index}>
          <Skeleton className="h-4 w-1/2" />
          <Skeleton className="h-3 w-3/4" />
          <Skeleton className="h-8 w-32" />
        </div>
      ))}
    </div>
  )
}

function ScheduledSendRow({ item, compact, pending, onCancel }: { item: ScheduledSend; compact: boolean; pending: boolean; onCancel: () => void }) {
  const recipients = item.to?.length ? item.to.join(", ") : "未填写收件人"
  const failed = item.status === "failed"
  return (
    <div className={cn("border-b transition-colors hover:bg-accent/40", compact ? "p-4" : "px-5 py-4")}>
      <div className={cn("gap-4", compact ? "space-y-3" : "grid grid-cols-[minmax(0,1fr)_180px_116px] items-center")}>
        <div className="min-w-0">
          <div className="mb-1 flex min-w-0 items-center gap-2">
            <span className="truncate text-sm font-semibold">{item.subject || "(无主题)"}</span>
            <ScheduledStatusBadge status={item.status} />
          </div>
          <div className="truncate text-xs text-muted-foreground">发给 {recipients}</div>
          {item.snippet && <div className="mt-1 line-clamp-1 text-xs text-muted-foreground">{item.snippet}</div>}
          {failed && item.error && <div className="mt-2 text-xs text-destructive">{item.error}</div>}
        </div>
        <div className="text-sm">
          <div className="text-xs text-muted-foreground">发送时间</div>
          <div className="mt-1 font-medium">{formatDateTime(item.sendAt)}</div>
        </div>
        <div className={cn("flex", compact ? "justify-start" : "justify-end")}>
          <Button type="button" variant={failed ? "outline" : "destructive"} size="sm" disabled={pending || item.status === "sending"} onClick={onCancel}>
            {pending ? "处理中..." : failed ? "移除记录" : "取消发送"}
          </Button>
        </div>
      </div>
    </div>
  )
}

function ScheduledStatusBadge({ status }: { status: ScheduledSend["status"] }) {
  const label = status === "pending" ? "等待发送" : status === "sending" ? "发送中" : status === "failed" ? "发送失败" : status === "sent" ? "已发送" : "已取消"
  return (
    <Badge variant={status === "failed" ? "destructive" : status === "sending" ? "secondary" : "outline"} className="h-5 shrink-0 rounded-md px-1.5 text-[11px] font-normal">
      {label}
    </Badge>
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
  scheduledDraftIds,
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
  scheduledDraftIds: Set<string>
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
      <div className="flex min-h-12 shrink-0 items-center justify-between gap-2 border-b px-3 sm:px-4">
        <div className="flex min-w-0 flex-1 items-center gap-3">
          <Checkbox aria-label="选择当前页邮件" checked={allSelected ? true : someSelected ? "indeterminate" : false} onCheckedChange={(value) => onSelectAll(value === true)} />
          <div className="flex min-w-0 items-center gap-2 text-base font-semibold">
            {icon}
            <span className="truncate">{title}</span>
          </div>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          {selectedIds.length > 0 ? (
            <>
              <span className="hidden text-sm text-muted-foreground min-[380px]:inline">已选 {selectedIds.length} 封</span>
              <BulkActionMenu pending={bulkPending} onAction={onBulkAction} />
            </>
          ) : (
            <div className="text-sm text-muted-foreground">{messages.length} / {total} 封</div>
          )}
        </div>
      </div>
      <ScrollArea className="min-h-0 flex-1">
        {loading && <MessageSkeleton />}
        {messages.map((message) => <CompactMessageRow key={message.id} message={message} active={selectedId === message.id} checked={selectedIds.includes(message.id)} scheduled={scheduledDraftIds.has(message.id)} onCheckedChange={(checked) => onToggleSelected(message.id, checked)} onClick={() => onSelect(message.id)} onStar={() => onStar(message)} />)}
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
      <div className="shrink-0 border-b px-3 py-2 sm:px-4">
        <div className="flex min-h-10 items-center gap-2 sm:hidden">
          <Button variant="ghost" size="icon" onClick={onBack} aria-label="返回">
            <ArrowLeft className="h-4 w-4" />
          </Button>
          <div className="min-w-0 flex-1 truncate text-sm font-semibold">{selected?.subject || "邮件详情"}</div>
          <Button variant="ghost" size="icon" disabled={!previousMessage} onClick={() => previousMessage && onSelect(previousMessage.id)} aria-label="上一封">
            <ArrowLeft className="h-4 w-4" />
          </Button>
          <Button variant="ghost" size="icon" disabled={!nextMessage} onClick={() => nextMessage && onSelect(nextMessage.id)} aria-label="下一封">
            <ArrowLeft className="h-4 w-4 rotate-180" />
          </Button>
          {selected && (
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="ghost" size="icon" aria-label="更多操作">
                  <Ellipsis className="h-4 w-4" />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                {selected.folder === "Drafts" ? (
                  <DropdownMenuItem onSelect={() => onSelect(selected.id)}><PencilLine className="h-4 w-4" />编辑草稿</DropdownMenuItem>
                ) : (
                  <>
                    <DropdownMenuItem onSelect={() => onReply(selected)}><Reply className="h-4 w-4" />回复</DropdownMenuItem>
                    <DropdownMenuItem onSelect={() => onForward(selected)}><Forward className="h-4 w-4" />转发</DropdownMenuItem>
                    <DropdownMenuItem onSelect={() => onArchive(selected)}><Archive className="h-4 w-4" />{selected.folder === "Archive" ? "取消归档" : "归档"}</DropdownMenuItem>
                    <DropdownMenuItem onSelect={() => onToggleRead(selected)}><MailCheck className="h-4 w-4" />{selected.isRead ? "标为未读" : "标为已读"}</DropdownMenuItem>
                    <DropdownMenuItem onSelect={() => onStar(selected)}><Star className={cn("h-4 w-4", selected.isStarred && "fill-yellow-400 text-yellow-500")} />{selected.isStarred ? "取消星标" : "添加星标"}</DropdownMenuItem>
                  </>
                )}
                <DropdownMenuItem onSelect={() => onDelete(selected)} className="text-destructive"><Trash2 className="h-4 w-4" />删除</DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          )}
        </div>
        <div className="hidden min-h-10 items-center justify-between gap-3 sm:flex">
          <div className="flex flex-wrap items-center gap-2">
            <Button variant="outline" size="sm" onClick={onBack}><ArrowLeft className="h-4 w-4" />返回</Button>
            {selected?.folder === "Drafts" ? (
              <Button variant="outline" size="sm" onClick={() => onSelect(selected.id)}><PencilLine className="h-4 w-4" />编辑草稿</Button>
            ) : (
              <>
                {selected && <Button variant="outline" size="sm" onClick={() => onReply(selected)}><Reply className="h-4 w-4" />回复</Button>}
                {selected && <Button variant="outline" size="sm" onClick={() => onForward(selected)}><Forward className="h-4 w-4" />转发</Button>}
                {selected && <Button variant="outline" size="sm" onClick={() => onArchive(selected)}>{selected.folder === "Archive" ? "取消归档" : "归档"}</Button>}
                {selected && <Button variant="outline" size="sm" onClick={() => onToggleRead(selected)}><MailCheck className="h-4 w-4" />{selected.isRead ? "标为未读" : "标为已读"}</Button>}
                {selected && <Button variant="outline" size="sm" onClick={() => onStar(selected)}><Star className={cn("h-4 w-4", selected.isStarred && "fill-yellow-400 text-yellow-500")} />{selected.isStarred ? "取消星标" : "添加星标"}</Button>}
              </>
            )}
            {selected && <Button variant="outline" size="sm" onClick={() => onDelete(selected)}><Trash2 className="h-4 w-4" />删除</Button>}
          </div>
          <div className="flex items-center gap-2">
            <Button variant="ghost" size="sm" disabled={!previousMessage} onClick={() => previousMessage && onSelect(previousMessage.id)}>上一封</Button>
            <Button variant="ghost" size="sm" disabled={!nextMessage} onClick={() => nextMessage && onSelect(nextMessage.id)}>下一封</Button>
          </div>
        </div>
      </div>
        {loading && <div className="space-y-4 p-8"><Skeleton className="h-8 w-2/3" /><Skeleton className="h-4 w-1/3" /><Separator /><Skeleton className="h-64 w-full" /></div>}
        {!loading && !selected && <div className="grid flex-1 place-items-center text-sm text-muted-foreground">邮件不存在</div>}
        {selected && (
          <ScrollArea className="min-h-0 flex-1">
            <div className="w-full px-4 py-4 sm:px-8 sm:py-6">
              <div className="space-y-5 border-b pb-5">
                <div className="flex items-start gap-3">
                  <h1 className="min-w-0 flex-1 break-words text-xl font-semibold tracking-tight sm:text-2xl">{selected.subject}</h1>
                  <Button type="button" variant="ghost" size="icon" aria-label={selected.isStarred ? "取消星标" : "添加星标"} className="text-muted-foreground hover:text-yellow-500" onClick={() => onStar(selected)}>
                    <Star className={cn("h-5 w-5", selected.isStarred && "fill-yellow-400 text-yellow-500")} />
                  </Button>
                </div>
                <div className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
                  <div className="flex min-w-0 items-start gap-3">
                    <Avatar className="size-10 rounded-full"><AvatarFallback className="bg-primary text-sm font-semibold text-primary-foreground">{accountInitial(senderDisplayName(selected), selected.from)}</AvatarFallback></Avatar>
                    <div className="min-w-0 text-sm">
                      <div className="truncate font-medium text-foreground" title={senderTitle(selected)}>{senderDisplayName(selected)}</div>
                      <div className="truncate text-muted-foreground">收件人 {selected.to.join(", ")}</div>
                    </div>
                  </div>
                  <div className="shrink-0 text-left text-sm text-muted-foreground sm:text-right">{formatDateTime(selected.receivedAt)}</div>
                </div>
                <MessageLabels
                  messageLabels={selected.labels || []}
                  availableLabels={labels}
                  onAdd={(label) => onAddLabel(selected, label)}
                  onRemove={(labelId) => onRemoveLabel(selected, labelId)}
                  pending={labelPending}
                />
              </div>
              <div className="py-6 sm:py-8">
                <div className="mail-html prose max-w-none text-sm leading-7" dangerouslySetInnerHTML={{ __html: DOMPurify.sanitize(selected.bodyHtml || `<pre>${escapeHtml(selected.bodyText || "")}</pre>`) }} />
                {selected.attachments && selected.attachments.length > 0 && <div className="mt-8 rounded-lg border p-4"><div className="mb-3 font-medium">附件</div><div className="space-y-2">{selected.attachments.map((a) => <a className="flex flex-col gap-1 rounded-md border p-3 text-sm hover:bg-accent sm:flex-row sm:items-center sm:justify-between" href={`/api/mail/attachments/${a.id}`} key={a.id}><span className="flex min-w-0 items-center gap-2"><Paperclip className="h-4 w-4 shrink-0" /><span className="truncate">{a.filename}</span></span><span className="text-muted-foreground">{formatBytes(a.sizeBytes)}</span></a>)}</div></div>}
              </div>
            </div>
          </ScrollArea>
        )}
    </div>
  )
}

function CompactMessageRow({ message, active, checked, scheduled, onCheckedChange, onClick, onStar }: { message: MailMessage; active: boolean; checked: boolean; scheduled?: boolean; onCheckedChange: (checked: boolean) => void; onClick: () => void; onStar: () => void }) {
  const visibleLabels = (message.labels || []).slice(0, 2)
  const hiddenLabelCount = Math.max((message.labels?.length || 0) - visibleLabels.length, 0)
  const senderName = senderDisplayName(message)
  return (
    <div onClick={onClick} className={cn("cursor-pointer border-b px-3 py-3 text-sm transition-colors hover:bg-accent/50 sm:grid sm:grid-cols-[32px_28px_minmax(140px,220px)_minmax(0,1fr)_104px_36px] sm:items-center sm:gap-2 sm:px-4 sm:py-2", active && "bg-accent", !message.isRead && "font-semibold")}>
      <div className="flex gap-3 sm:contents">
        <Checkbox aria-label="选择邮件" checked={checked} onCheckedChange={(value) => onCheckedChange(value === true)} onClick={(event) => event.stopPropagation()} className="mt-0.5 shrink-0 sm:mt-0" />
        {message.isRead ? (
          <MailCheck className="mt-0.5 h-4 w-4 shrink-0 text-muted-foreground/70 sm:mt-0" />
        ) : (
          <Mail className="mt-0.5 h-4 w-4 shrink-0 fill-yellow-200 text-yellow-500 sm:mt-0" />
        )}
        <div className="min-w-0 flex-1 sm:contents">
          <div className="flex min-w-0 items-center justify-between gap-2 sm:block">
            <div className="min-w-0 truncate" title={senderTitle(message)}>{senderName}</div>
            <div className="flex shrink-0 items-center gap-1 sm:hidden">
              <span className="text-xs text-muted-foreground">{formatDate(message.receivedAt)}</span>
              <Button type="button" variant="ghost" size="icon" aria-label={message.isStarred ? "取消星标" : "添加星标"} className="h-7 w-7 text-muted-foreground hover:text-yellow-500" onClick={(event) => { event.stopPropagation(); onStar() }}>
                <Star className={cn("h-4 w-4", message.isStarred && "fill-yellow-400 text-yellow-500")} />
              </Button>
            </div>
          </div>
          <div className="mt-1 flex min-w-0 items-center gap-2 sm:mt-0">
            <span className="truncate font-medium">{message.subject}</span>
            <span className="hidden min-w-0 truncate text-muted-foreground sm:block">{message.snippet}</span>
            {scheduled && <Badge variant="secondary" className="h-5 shrink-0 rounded-md px-1.5 text-[11px] font-normal">已定时</Badge>}
            {visibleLabels.map((label) => <MailLabelBadge key={label.id} label={label} compact />)}
            {hiddenLabelCount > 0 && <Badge variant="outline" className="h-5 shrink-0 rounded-md px-1.5 text-[11px] font-normal text-muted-foreground">+{hiddenLabelCount}</Badge>}
            {message.hasAttachments && <Paperclip className="h-3 w-3 shrink-0 text-muted-foreground" />}
          </div>
          <div className="mt-1 line-clamp-2 text-xs text-muted-foreground sm:hidden">{message.snippet}</div>
        </div>
      </div>
      <div className="hidden shrink-0 text-right text-xs text-muted-foreground sm:block">{formatDate(message.receivedAt)}</div>
      <Button type="button" variant="ghost" size="icon" aria-label={message.isStarred ? "取消星标" : "添加星标"} className="hidden h-7 w-7 text-muted-foreground hover:text-yellow-500 sm:inline-flex" onClick={(event) => { event.stopPropagation(); onStar() }}>
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
  const fromName = decodeMimeHeader(message.fromName?.trim() || "")
  if (fromName) return fromName
  return displayNameFromAddress(message.from)
}

function displayNameFromAddress(value: string) {
  const text = decodeMimeHeader(value.trim())
  const namedAddress = text.match(/^"?([^"<]+?)"?\s*<[^>]+>$/)
  const name = namedAddress?.[1]?.trim()
  if (name) return name
  const address = text.match(/<([^>]+)>/)?.[1]?.trim() || text
  const localPart = address.split("@")[0]?.trim()
  return localPart || text || "未知发件人"
}

function senderTitle(message: MailMessage) {
  const name = decodeMimeHeader(message.fromName?.trim() || "")
  const from = decodeMimeHeader(message.from)
  return name ? `${name} <${from}>` : from
}

function MessageRow({
  message,
  active,
  checked,
  scheduled,
  onCheckedChange,
  onClick,
  onStar,
}: {
  message: MailMessage
  active: boolean
  checked: boolean
  scheduled?: boolean
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
          {scheduled && <Badge variant="secondary" className="h-5 shrink-0 rounded-md px-1.5 text-[11px] font-normal">已定时</Badge>}
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
  const qc = useQueryClient()
  const [files, setFiles] = React.useState<File[]>([])
  const [draftAttachments, setDraftAttachments] = React.useState<SendPayload["attachments"]>([])
  const [attachmentsTouched, setAttachmentsTouched] = React.useState(false)
  const [draftId, setDraftId] = React.useState(draft?.id || "")
  const [toValue, setToValue] = React.useState(draft?.to || "")
  const [ccValue, setCcValue] = React.useState(draft?.cc || "")
  const [bccValue, setBccValue] = React.useState(draft?.bcc || "")
  const [subjectValue, setSubjectValue] = React.useState(draft?.subject || "")
  const [draftStatus, setDraftStatus] = React.useState<"idle" | "saving" | "saved" | "error">("idle")
  const [lastSavedAt, setLastSavedAt] = React.useState<Date | null>(null)
  const [scheduleDialogOpen, setScheduleDialogOpen] = React.useState(false)
  const [sendIntent, setSendIntent] = React.useState<ComposeSendIntent | null>(null)
  const sendStartedRef = React.useRef(false)
  const lastSavedPayloadRef = React.useRef("")
  const [showCc, setShowCc] = React.useState(Boolean(draft?.cc))
  const [showBcc, setShowBcc] = React.useState(Boolean(draft?.bcc))
  const [sendSeparately, setSendSeparately] = React.useState(false)
  const defaultSignature = useQuery({ queryKey: ["signature", "default", mailbox?.id], queryFn: () => api.defaultSignature(mailbox?.id), enabled: open && !!mailbox?.id })
  const signatureText = defaultSignature.data?.signature?.content || ""
  const composerText = draft?.html || (draft?.text !== undefined ? draft.text : signatureText ? `\n\n-- \n${signatureText}` : "")
  const [body, setBody] = React.useState<ComposerValue>(() => draft?.html !== undefined ? htmlComposerValue(draft.html) : plainTextComposerValue(composerText))
  const activeMailboxId = draft?.mailboxId || mailbox?.id || ""
  const composePayload = React.useMemo<DraftPayload>(() => ({
    mailboxId: activeMailboxId,
    to: splitEmails(toValue),
    cc: showCc ? splitEmails(ccValue) : [],
    bcc: showBcc ? splitEmails(bccValue) : [],
    subject: subjectValue,
    text: body.text,
    html: body.html || plainTextToHtml(body.text),
    ...(attachmentsTouched ? { attachments: draftAttachments } : {}),
  }), [activeMailboxId, toValue, showCc, ccValue, showBcc, bccValue, subjectValue, body, attachmentsTouched, draftAttachments])
  const hasDraftContent = open && !!activeMailboxId && (toValue.trim() || ccValue.trim() || bccValue.trim() || subjectValue.trim() || body.text.trim() || body.html.trim())
  const send = useMutation({
    mutationFn: async (payloads: SendPayload[]) => {
      const sent: MailMessage[] = []
      for (const payload of payloads) sent.push(await api.send(payload))
      return sent
    },
    onSuccess: async (_, payloads) => {
      if (draftId) {
        try {
          await api.deleteDraft(draftId)
        } catch {}
      }
      toast({ title: payloads.length > 1 ? `已分别发送 ${payloads.length} 封邮件` : "发送成功" })
      setFiles([])
      setDraftId("")
      onSent()
    },
    onError: (e) => toast({ title: "发送失败", description: e.message }),
  })
  const scheduleSend = useMutation({
    mutationFn: (payload: SendPayload & { draftId?: string; sendAt: string }) => api.scheduleSend(payload),
    onSuccess: (scheduled) => {
      sendStartedRef.current = true
      toast({ title: `已定时发送 ${formatDateTime(scheduled.sendAt)}` })
      setScheduleDialogOpen(false)
      setFiles([])
      void Promise.all([
        qc.invalidateQueries({ queryKey: ["messages"] }),
        qc.invalidateQueries({ queryKey: ["folders"] }),
        qc.invalidateQueries({ queryKey: ["mail-stats"] }),
        qc.invalidateQueries({ queryKey: ["scheduled-sends"] }),
      ])
      onSent()
    },
    onError: (e) => toast({ title: "定时发送失败", description: e.message }),
  })

  React.useEffect(() => {
    if (!open) return
    sendStartedRef.current = false
    const nextShowCc = Boolean(draft?.cc)
    const nextShowBcc = Boolean(draft?.bcc)
    const nextBody = draft?.html !== undefined ? htmlComposerValue(draft.html) : plainTextComposerValue(composerText)
    lastSavedPayloadRef.current = JSON.stringify({
      mailboxId: draft?.mailboxId || mailbox?.id || "",
      to: splitEmails(draft?.to || ""),
      cc: nextShowCc ? splitEmails(draft?.cc || "") : [],
      bcc: nextShowBcc ? splitEmails(draft?.bcc || "") : [],
      subject: draft?.subject || "",
      text: nextBody.text,
      html: nextBody.html || plainTextToHtml(nextBody.text),
      draftId: draft?.id || "",
    })
    setDraftId(draft?.id || "")
    setToValue(draft?.to || "")
    setCcValue(draft?.cc || "")
    setBccValue(draft?.bcc || "")
    setSubjectValue(draft?.subject || "")
    setBody(nextBody)
    setDraftStatus("idle")
    setLastSavedAt(null)
    setShowCc(nextShowCc)
    setShowBcc(nextShowBcc)
    setSendSeparately(false)
    setFiles(draft?.files || [])
    setDraftAttachments([])
    setAttachmentsTouched(false)
  }, [open, draft?.key, draft?.id, draft?.mailboxId, draft?.to, draft?.cc, draft?.bcc, draft?.subject, draft?.html, draft?.files, mailbox?.id, composerText])

  React.useEffect(() => {
    let cancelled = false
    Promise.all(files.map(fileToAttachment)).then((attachments) => {
      if (!cancelled) setDraftAttachments(attachments)
    })
    return () => { cancelled = true }
  }, [files])

  React.useEffect(() => {
    if (!open || sendStartedRef.current || !hasDraftContent) return
    const payloadKey = JSON.stringify({ ...composePayload, draftId })
    if (payloadKey === lastSavedPayloadRef.current) return
    const timer = window.setTimeout(async () => {
      try {
        setDraftStatus("saving")
        const saved = await api.saveDraft(composePayload, draftId || undefined)
        setDraftId(saved.id)
        lastSavedPayloadRef.current = JSON.stringify({ ...composePayload, draftId: saved.id })
        setLastSavedAt(new Date())
        setDraftStatus("saved")
        await Promise.all([
          qc.invalidateQueries({ queryKey: ["messages"] }),
          qc.invalidateQueries({ queryKey: ["folders"] }),
          qc.invalidateQueries({ queryKey: ["mail-stats"] }),
        ])
      } catch {
        setDraftStatus("error")
      }
    }, 5000)
    return () => window.clearTimeout(timer)
  }, [open, hasDraftContent, composePayload, draftId, qc])

  function buildSendWarnings(attachmentsCount: number) {
    const warnings: string[] = []
    const normalizedBody = `${body.text}\n${stripHtml(body.html)}`.toLowerCase()
    if (!subjectValue.trim()) warnings.push("这封邮件还没有主题。")
    if (!body.text.trim() && !htmlContainsMeaningfulContent(body.html)) warnings.push("正文还是空的。")
    if (/(附件|附上|见附件|attached|attachment)/i.test(normalizedBody) && attachmentsCount === 0) warnings.push("正文提到了附件，但还没有添加附件。")
    return warnings
  }

  function confirmOrRun(intent: Omit<ComposeSendIntent, "description"> & { warnings: string[]; defaultDescription?: string }) {
    if (intent.warnings.length === 0) {
      intent.onConfirm()
      return
    }
    setSendIntent({
      title: intent.title,
      description: intent.defaultDescription ? `${intent.defaultDescription}\n${intent.warnings.join("\n")}` : intent.warnings.join("\n"),
      confirmText: intent.confirmText,
      onConfirm: intent.onConfirm,
    })
  }

  async function prepareSend() {
    if (!mailbox) return
    const attachments = await Promise.all(files.map(fileToAttachment))
    const to = splitEmails(toValue)
    const cc = showCc ? splitEmails(ccValue) : []
    const bcc = showBcc ? splitEmails(bccValue) : []
    const text = body.text
    const html = body.html || plainTextToHtml(text)
    const payload: SendPayload = { mailboxId: mailbox.id, to, cc, bcc, subject: subjectValue, text, html, attachments }
    const separateRecipients = Array.from(new Set([...to, ...cc, ...bcc]))
    const payloads = sendSeparately && separateRecipients.length > 0
      ? separateRecipients.map((recipient): SendPayload => ({ ...payload, to: [recipient], cc: [], bcc: [] }))
      : [payload]
    confirmOrRun({
      title: "确认发送这封邮件？",
      confirmText: sendSeparately && payloads.length > 1 ? "继续分别发送" : "继续发送",
      warnings: buildSendWarnings(attachments.length),
      onConfirm: () => {
        sendStartedRef.current = true
        setSendIntent(null)
        send.mutate(payloads)
      },
    })
  }

  async function submit(e: React.FormEvent<HTMLFormElement>) {
    e.preventDefault()
    if (!mailbox) {
      toast({ title: "请选择发件邮箱" })
      return
    }
    await prepareSend()
  }
  async function scheduleAt(sendAt: string) {
    if (!mailbox) {
      toast({ title: "请选择发件邮箱" })
      return
    }
    const attachments = await Promise.all(files.map(fileToAttachment))
    const payload: SendPayload & { draftId?: string; sendAt: string } = {
      mailboxId: mailbox.id,
      to: splitEmails(toValue),
      cc: showCc ? splitEmails(ccValue) : [],
      bcc: showBcc ? splitEmails(bccValue) : [],
      subject: subjectValue,
      text: body.text,
      html: body.html || plainTextToHtml(body.text),
      attachments,
      draftId: draftId || undefined,
      sendAt,
    }
    confirmOrRun({
      title: "确认定时发送？",
      confirmText: "继续定时发送",
      defaultDescription: `发送时间：${formatDateTime(sendAt)}`,
      warnings: buildSendWarnings(attachments.length),
      onConfirm: () => {
        sendStartedRef.current = true
        setSendIntent(null)
        scheduleSend.mutate(payload)
      },
    })
  }
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        className="flex h-svh w-screen max-w-none overflow-hidden p-0 sm:h-auto sm:max-h-[92vh] sm:w-[min(96vw,82rem)]"
        onInteractOutside={(event) => event.preventDefault()}
        onPointerDownOutside={(event) => event.preventDefault()}
      >
        <form key={draft?.key || "new"} className="flex min-h-0 flex-1 flex-col sm:max-h-[90vh]" onSubmit={submit}>
          <DialogHeader className="border-b px-4 py-3 text-left sm:px-6 sm:py-4">
            <DialogTitle className="flex min-w-0 flex-col gap-1 pr-8 sm:flex-row sm:items-center sm:justify-between sm:gap-4 sm:pr-6">
              <span>{draftId ? "编辑草稿" : "写信"}</span>
              <span className={cn("text-xs font-normal", draftStatus === "error" ? "text-destructive" : "text-muted-foreground")}>
                {draftStatus === "saving" ? "正在保存草稿..." : draftStatus === "saved" && lastSavedAt ? `草稿已保存 ${lastSavedAt.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}` : draftStatus === "error" ? "草稿保存失败" : ""}
              </span>
            </DialogTitle>
          </DialogHeader>
          <div className="flex min-h-0 flex-1 flex-col overflow-y-auto">
            <ComposeField label="发件邮箱">
              <Input value={mailbox?.address || "未选择"} readOnly className="h-10 flex-1 rounded-none border-0 px-0 shadow-none focus-visible:ring-0" />
            </ComposeField>
            <ComposeField
              label="收件人"
              action={
                <div className="flex shrink-0 flex-wrap items-center justify-start gap-1 text-sm sm:justify-end sm:gap-2">
                  <Button type="button" variant="ghost" size="sm" className="h-8 px-2 font-normal" onClick={() => setShowCc((value) => !value)}>抄送</Button>
                  <Button type="button" variant="ghost" size="sm" className="h-8 px-2 font-normal" onClick={() => setShowBcc((value) => !value)}>密送</Button>
                  <div className="flex items-center gap-2 rounded-md px-2 py-1">
                    <Checkbox id="compose-send-separately" checked={sendSeparately} onCheckedChange={(value) => setSendSeparately(value === true)} />
                    <Label htmlFor="compose-send-separately" className="cursor-pointer text-sm font-normal">分别发送</Label>
                  </div>
                </div>
              }
            >
              <Input name="to" placeholder="name@example.com，多个地址用逗号或空格分隔" value={toValue} onChange={(event) => setToValue(event.target.value)} required className="h-10 flex-1 rounded-none border-0 px-0 shadow-none focus-visible:ring-0" />
            </ComposeField>
            {showCc && (
              <ComposeField label="抄送">
                <Input name="cc" placeholder="cc@example.com" value={ccValue} onChange={(event) => setCcValue(event.target.value)} className="h-10 flex-1 rounded-none border-0 px-0 shadow-none focus-visible:ring-0" />
              </ComposeField>
            )}
            {showBcc && (
              <ComposeField label="密送">
                <Input name="bcc" placeholder="bcc@example.com" value={bccValue} onChange={(event) => setBccValue(event.target.value)} className="h-10 flex-1 rounded-none border-0 px-0 shadow-none focus-visible:ring-0" />
              </ComposeField>
            )}
            <ComposeField label="主　题">
              <Input name="subject" placeholder="输入主题" value={subjectValue} onChange={(event) => setSubjectValue(event.target.value)} className="h-10 flex-1 rounded-none border-0 px-0 shadow-none focus-visible:ring-0" />
            </ComposeField>
            <MailBodyComposer
              defaultValue={composerText}
              defaultHtml={draft?.html}
              files={files}
              signatureText={signatureText}
              onChange={setBody}
              onPickFiles={(nextFiles) => { setAttachmentsTouched(true); setFiles((current) => [...current, ...nextFiles]) }}
              onRemoveFile={(index) => { setAttachmentsTouched(true); setFiles((current) => current.filter((_, itemIndex) => itemIndex !== index)) }}
            />
          </div>
          <DialogFooter className="grid grid-cols-3 gap-2 border-t bg-background px-4 py-3 sm:flex sm:flex-row sm:justify-end sm:px-6 sm:py-4">
            <Button type="button" variant="outline" className="min-h-10 px-3" onClick={() => onOpenChange(false)}>取消</Button>
            <Button type="button" variant="outline" className="min-h-10 px-3" disabled={send.isPending || scheduleSend.isPending || !mailbox} onClick={() => setScheduleDialogOpen(true)}><Calendar className="h-4 w-4" />定时</Button>
            <Button className="min-h-10 px-4" disabled={send.isPending || !mailbox}><Send className="h-4 w-4" />{send.isPending ? "发送中..." : "发送"}</Button>
          </DialogFooter>
        </form>
        <ScheduleSendDialog open={scheduleDialogOpen} pending={scheduleSend.isPending} onOpenChange={setScheduleDialogOpen} onConfirm={scheduleAt} />
        <ConfirmDialog
          open={!!sendIntent}
          title={sendIntent?.title || ""}
          description={sendIntent?.description}
          confirmText={sendIntent?.confirmText || "继续"}
          pending={send.isPending || scheduleSend.isPending}
          onOpenChange={(nextOpen) => { if (!nextOpen) setSendIntent(null) }}
          onConfirm={() => sendIntent?.onConfirm()}
        />
      </DialogContent>
    </Dialog>
  )
}

function ComposeField({ label, children, action }: { label: string; children: React.ReactNode; action?: React.ReactNode }) {
  return (
    <div className="flex min-h-14 flex-col gap-2 border-b px-4 py-2 sm:flex-row sm:items-center sm:px-6">
      <Label className="shrink-0 text-base font-normal text-foreground sm:w-20">{label}</Label>
      <div className="flex min-w-0 flex-1 flex-col gap-2 sm:flex-row sm:items-center">
        {children}
        {action}
      </div>
    </div>
  )
}

function ScheduleSendDialog({ open, pending, onOpenChange, onConfirm }: { open: boolean; pending: boolean; onOpenChange: (open: boolean) => void; onConfirm: (sendAt: string) => void }) {
  const [value, setValue] = React.useState("")
  const { toast } = useToast()
  const presets = React.useMemo(() => scheduledSendPresets(), [open])

  React.useEffect(() => {
    if (open) setValue(defaultScheduledSendValue())
  }, [open])

  function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const date = new Date(value)
    if (!value || Number.isNaN(date.getTime()) || !date.getTime()) {
      toast({ title: "请选择发送时间" })
      return
    }
    if (date.getTime() <= Date.now() + 30_000) {
      toast({ title: "发送时间需要晚于当前时间" })
      return
    }
    onConfirm(date.toISOString())
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <form className="grid gap-4" onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>定时发送</DialogTitle>
          </DialogHeader>
          <div className="grid grid-cols-2 gap-2">
            {presets.map((preset) => (
              <Button
                type="button"
                key={preset.label}
                variant={value === preset.value ? "secondary" : "outline"}
                className="justify-start font-normal"
                onClick={() => setValue(preset.value)}
              >
                <Clock3 className="h-4 w-4" />{preset.label}
              </Button>
            ))}
          </div>
          <div className="grid gap-2">
            <Label htmlFor="schedule-send-at">发送时间</Label>
            <Input id="schedule-send-at" type="datetime-local" value={value} min={toDateTimeLocalValue(new Date(Date.now() + 60_000))} onChange={(event) => setValue(event.target.value)} />
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)} disabled={pending}>取消</Button>
            <Button type="submit" disabled={pending}>{pending ? "正在设置..." : "确认定时"}</Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

type ComposerValue = { text: string; html: string }
type InsertDialogState = { kind: "link" | "image"; selectedText: string; url?: string; alt?: string; editing?: boolean }
type InsertDialogValue = { url: string; text: string; alt: string }
const composerFontOptions = ["Arial", "Georgia", "Times New Roman", "Courier New", "Microsoft YaHei"]
const composerFontSizeOptions = [
  ["2", "小号"],
  ["3", "正文"],
  ["4", "中号"],
  ["5", "大号"],
] as const
const composerFontSizeValueByKey: Record<string, string> = { "2": "13px", "3": "16px", "4": "20px", "5": "24px" }
const composerTextColors = [["#111827", "默认"], ["#dc2626", "红色"], ["#2563eb", "蓝色"], ["#16a34a", "绿色"], ["#9333ea", "紫色"]] as const
const composerHighlightColors = [["transparent", "无高亮"], ["#fef3c7", "黄色"], ["#dcfce7", "绿色"], ["#dbeafe", "蓝色"], ["#fce7f3", "粉色"]] as const
const composerEmojiOptions = ["😀", "😄", "😊", "🙂", "😉", "😍", "😘", "😎", "🤔", "👍", "👏", "🙏", "💪", "🎉", "🔥", "✨", "❤️", "✅", "📌", "📅", "☕", "💡", "🚀", "⭐"]
const composerMenuItemClass = "min-h-9 rounded-md px-3 text-sm transition-colors data-[highlighted]:bg-primary/10 data-[highlighted]:font-semibold data-[highlighted]:text-foreground hover:bg-primary/10 hover:font-semibold hover:text-foreground"

function normalizeFontName(value: string) {
  const cleaned = value.replace(/["']/g, "").split(",")[0]?.trim() || ""
  if (!cleaned || cleaned === "默认字体") return ""
  if (/microsoft yahei/i.test(cleaned) || cleaned.includes("微软雅黑")) return "Microsoft YaHei"
  const lower = cleaned.toLowerCase()
  return composerFontOptions.find((font) => {
    const option = font.toLowerCase()
    return lower === option || lower.includes(option) || option.includes(lower)
  }) || ""
}

function normalizeFontSize(value: string) {
  const cleaned = value.trim().toLowerCase()
  if (!cleaned) return ""
  if (composerFontSizeOptions.some(([size]) => size === cleaned)) return cleaned
  const px = Number(cleaned.replace("px", ""))
  if (Number.isFinite(px)) {
    if (px <= 13) return "2"
    if (px <= 17) return "3"
    if (px <= 22) return "4"
    return "5"
  }
  if (cleaned.includes("small")) return "2"
  if (cleaned.includes("large") || cleaned.includes("x-large")) return "5"
  if (cleaned.includes("medium") || cleaned.includes("normal")) return "3"
  return ""
}

function fontLabel(value: string) {
  const normalized = normalizeFontName(value)
  if (!normalized) return "默认字体"
  return normalized === "Microsoft YaHei" ? "微软雅黑" : normalized
}

function fontSizeLabel(value: string) {
  const normalized = normalizeFontSize(value) || "3"
  return composerFontSizeOptions.find(([size]) => size === normalized)?.[1] || "正文"
}

function normalizeInsertUrl(value: string, kind: InsertDialogState["kind"]) {
  const trimmed = value.trim()
  if (!trimmed) return ""
  const allowed = kind === "image" ? /^(https?:|cid:|data:image\/|\/)/i : /^(https?:|mailto:|tel:|#|\/)/i
  return allowed.test(trimmed) ? trimmed : `https://${trimmed}`
}

const ScheduleCardNode = Node.create({
  name: "scheduleCard",
  group: "block",
  atom: true,
  selectable: true,
  draggable: false,
  addAttributes() {
    return {
      title: { default: "" },
      time: { default: "" },
      duration: { default: "" },
      reminder: { default: "" },
      repeat: { default: "" },
      location: { default: "" },
      description: { default: "" },
    }
  },
  parseHTML() {
    return [{ tag: "div[data-schedule-card]" }]
  },
  renderHTML({ HTMLAttributes }) {
    const { title, time, duration, reminder, repeat, location, description } = HTMLAttributes
    const rows = [
      ["时间", time],
      ["持续", duration],
      ["提醒", reminder],
      ["重复", repeat],
      location ? ["位置", location] : undefined,
      description ? ["描述", description] : undefined,
    ].filter(Boolean) as string[][]
    return [
      "div",
      mergeAttributes(HTMLAttributes, {
        "data-schedule-card": "true",
        style: "border:1px solid #d4d4d8;border-radius:8px;padding:14px 16px;margin:16px 0;background:#fafafa;",
      }),
      ["div", { style: "font-weight:600;font-size:16px;margin-bottom:10px;" }, title || "日程"],
      ...rows.map(([label, value]) => ["div", { style: "margin:6px 0;" }, ["span", { style: "color:#71717a;" }, `${label}：`], value]),
    ]
  },
})

function composerInitialHtml(defaultValue: string, defaultHtml?: string) {
  return sanitizeComposerHtml(defaultHtml !== undefined ? defaultHtml : plainTextToHtml(defaultValue)) || "<p></p>"
}

function composerValueFromEditor(editor: Editor): ComposerValue {
  const text = editor.getText({ blockSeparator: "\n" }).replace(/\u00a0/g, " ").trimEnd()
  const html = sanitizeComposerHtml(editor.getHTML())
  if (!text.trim() && !htmlContainsMeaningfulContent(html)) return { text: "", html: "" }
  return { text, html: html || plainTextToHtml(text) }
}

function editorTextSelection(editor: Editor) {
  const { from, to, empty } = editor.state.selection
  if (empty) return ""
  return editor.state.doc.textBetween(from, to, " ").trim()
}

function selectedImageAttributes(editor: Editor) {
  const attrs = editor.getAttributes("image") as { src?: string; alt?: string }
  return attrs.src ? attrs : null
}

function scheduleToNodeAttributes(schedule: ScheduleDraft) {
  const start = parseScheduleStart(schedule)
  const end = schedule.allDay ? new Date(start.getTime() + 24 * 60 * 60 * 1000) : new Date(start.getTime() + schedule.durationMinutes * 60 * 1000)
  return {
    title: schedule.title,
    time: schedule.allDay ? formatDate(start.toISOString()) : `${formatDateTime(start.toISOString())} - ${formatTimeOnly(end)}`,
    duration: schedule.allDay ? "全天" : durationLabel(schedule.durationMinutes),
    reminder: reminderLabel(schedule.reminderMinutes),
    repeat: repeatLabel(schedule.repeat),
    location: schedule.location,
    description: schedule.description,
  }
}

function MailBodyComposer({ defaultValue, defaultHtml, files, signatureText, onChange, onPickFiles, onRemoveFile }: { defaultValue: string; defaultHtml?: string; files: File[]; signatureText: string; onChange: (value: ComposerValue) => void; onPickFiles: (files: File[]) => void; onRemoveFile: (index: number) => void }) {
  const fileInputRef = React.useRef<HTMLInputElement>(null)
  const dirtyRef = React.useRef(false)
  const lastDefaultRef = React.useRef(`${defaultValue}\n${defaultHtml || ""}`)
  const isMobile = useIsMobile()
  const [formatOpen, setFormatOpen] = React.useState(() => typeof window === "undefined" ? true : window.innerWidth >= 768)
  const [scheduleOpen, setScheduleOpen] = React.useState(false)
  const [emojiOpen, setEmojiOpen] = React.useState(false)
  const [insertDialog, setInsertDialog] = React.useState<InsertDialogState | null>(null)
  const [previewOpen, setPreviewOpen] = React.useState(false)
  const [empty, setEmpty] = React.useState(!defaultValue.trim())
  const [selectionVersion, setSelectionVersion] = React.useState(0)

  React.useEffect(() => {
    setFormatOpen(!isMobile)
  }, [isMobile])

  const editor = useEditor({
    extensions: [
      StarterKit.configure({ link: false }),
      TextStyle,
      Color,
      BackgroundColor,
      FontFamily,
      FontSize,
      LinkExtension.configure({
        openOnClick: false,
        enableClickSelection: true,
        HTMLAttributes: { target: "_blank", rel: "noopener noreferrer" },
      }),
      ImageExtension.configure({ allowBase64: true, HTMLAttributes: { style: "max-width:100%;height:auto;border-radius:8px;margin:12px 0;" } }),
      TextAlign.configure({ types: ["heading", "paragraph"] }),
      Placeholder.configure({ placeholder: "输入正文" }),
      ScheduleCardNode,
    ],
    content: composerInitialHtml(defaultValue, defaultHtml),
    editorProps: {
      attributes: {
        class: "mail-html min-h-[240px] min-w-0 flex-1 overflow-y-auto px-4 py-4 text-base leading-7 outline-none sm:min-h-[280px] sm:px-6 sm:py-5",
        "aria-label": "正文",
      },
      handlePaste(view, event) {
        const clipboard = event.clipboardData
        if (!clipboard) return false
        const html = clipboard.getData("text/html")
        const text = clipboard.getData("text/plain")
        if (!html && !text) return false
        event.preventDefault()
        const content = html ? sanitizeComposerHtml(html) : plainTextToHtml(text)
        const container = document.createElement("div")
        container.innerHTML = content || plainTextToHtml(text)
        const slice = ProseMirrorDOMParser.fromSchema(view.state.schema).parseSlice(container)
        view.dispatch(view.state.tr.replaceSelection(slice).scrollIntoView())
        return true
      },
    },
    onCreate({ editor }) {
      const next = composerValueFromEditor(editor)
      onChange(next)
      setEmpty(!next.text.trim() && !htmlContainsMeaningfulContent(next.html))
    },
    onUpdate({ editor }) {
      dirtyRef.current = true
      const next = composerValueFromEditor(editor)
      onChange(next)
      setEmpty(!next.text.trim() && !htmlContainsMeaningfulContent(next.html))
    },
    onSelectionUpdate() {
      setSelectionVersion((value) => value + 1)
    },
    onTransaction() {
      setSelectionVersion((value) => value + 1)
    },
  })

  React.useEffect(() => {
    if (!editor) return
    const defaultKey = `${defaultValue}\n${defaultHtml || ""}`
    if (defaultKey === lastDefaultRef.current) return
    lastDefaultRef.current = defaultKey
    if (!dirtyRef.current || editor.isEmpty) {
      const next = defaultHtml !== undefined ? htmlComposerValue(defaultHtml) : plainTextComposerValue(defaultValue)
      editor.commands.setContent(next.html || "<p></p>", { emitUpdate: false })
      onChange(next)
      setEmpty(!next.text.trim() && !htmlContainsMeaningfulContent(next.html))
    }
  }, [editor, defaultValue, defaultHtml, onChange])

  const textStyleAttributes = editor?.getAttributes("textStyle") as { fontFamily?: string; fontSize?: string; color?: string; backgroundColor?: string } | undefined
  const activeFont = normalizeFontName(textStyleAttributes?.fontFamily || "")
  const activeFontSize = normalizeFontSize(textStyleAttributes?.fontSize || "") || "3"
  const activeColor = textStyleAttributes?.color || ""
  const activeHighlight = textStyleAttributes?.backgroundColor || ""
  void selectionVersion

  function applyFont(font: string) {
    editor?.chain().focus().setFontFamily(font).run()
  }

  function applyFontSize(size: string) {
    const value = composerFontSizeValueByKey[size]
    if (value) editor?.chain().focus().setFontSize(value).run()
  }

  function openInsertDialog(kind: InsertDialogState["kind"]) {
    if (!editor) return
    if (kind === "link") {
      const attrs = editor.getAttributes("link") as { href?: string }
      setInsertDialog({ kind, selectedText: editorTextSelection(editor), url: attrs.href || "", editing: Boolean(attrs.href) })
      return
    }
    const imageAttrs = selectedImageAttributes(editor)
    setInsertDialog({ kind, selectedText: "", url: imageAttrs?.src || "", alt: imageAttrs?.alt || "", editing: Boolean(imageAttrs?.src) })
  }

  function confirmInsert(value: InsertDialogValue) {
    if (!editor || !insertDialog) return
    const url = normalizeInsertUrl(value.url, insertDialog.kind)
    if (!url) return
    if (insertDialog.kind === "link") {
      const text = value.text.trim() || insertDialog.selectedText || value.url.trim()
      if (editor.state.selection.empty && !insertDialog.editing) {
        editor.chain().focus().insertContent(`<a href="${escapeHtml(url)}" target="_blank" rel="noopener noreferrer">${escapeHtml(text)}</a>`).run()
      } else {
        if (value.text.trim() && value.text.trim() !== insertDialog.selectedText) editor.chain().focus().insertContent(escapeHtml(text)).run()
        editor.chain().focus().extendMarkRange("link").setLink({ href: url, target: "_blank", rel: "noopener noreferrer" }).run()
      }
      return
    }
    if (insertDialog.editing) {
      editor.chain().focus().updateAttributes("image", { src: url, alt: value.alt.trim() }).run()
      return
    }
    editor.chain().focus().setImage({ src: url, alt: value.alt.trim() }).run()
  }

  function insertSignature() {
    if (!editor || !signatureText.trim()) return
    editor.chain().focus().insertContent(`<p><br></p><p>-- <br>${plainTextToHtmlFragment(signatureText)}</p>`).run()
  }

  function insertSchedule(schedule: ScheduleDraft) {
    if (!editor) return
    const normalized = normalizeSchedule(schedule)
    editor.chain().focus().insertContent({ type: "scheduleCard", attrs: scheduleToNodeAttributes(normalized) }).run()
    onPickFiles([scheduleToFile(normalized)])
  }

  function insertEmoji(emoji: string) {
    editor?.chain().focus().insertContent(emoji).run()
    setEmojiOpen(false)
  }

  function handlePickedFiles(event: React.ChangeEvent<HTMLInputElement>) {
    const nextFiles = Array.from(event.currentTarget.files || [])
    if (nextFiles.length > 0) onPickFiles(nextFiles)
    event.currentTarget.value = ""
  }

  return (
    <div className="flex min-h-[330px] flex-1 flex-col bg-background sm:min-h-[420px]">
      <Input ref={fileInputRef} type="file" multiple className="hidden" onChange={handlePickedFiles} />
      <div className="flex min-h-11 flex-wrap items-center gap-1 overflow-visible border-b px-3 py-2 sm:px-6">
        <ToolbarButton label="撤销" disabled={!editor?.can().undo()} onClick={() => editor?.chain().focus().undo().run()}><Undo2 className="h-4 w-4" /></ToolbarButton>
        <ToolbarButton label="重做" disabled={!editor?.can().redo()} onClick={() => editor?.chain().focus().redo().run()}><Redo2 className="h-4 w-4" /></ToolbarButton>
        <Separator orientation="vertical" className="mx-2 h-6" />
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button type="button" variant="ghost" size="sm" className="h-8 gap-1 rounded-md px-2 font-normal hover:bg-accent hover:shadow-sm" onMouseDown={(event) => event.preventDefault()}>
              <Plus className="h-4 w-4" />插入<ChevronDown className="h-3.5 w-3.5" />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="start">
            <DropdownMenuItem className={composerMenuItemClass} onSelect={() => fileInputRef.current?.click()}><Paperclip className="h-4 w-4" />附件</DropdownMenuItem>
            <DropdownMenuItem className={composerMenuItemClass} onSelect={() => openInsertDialog("link")}><Link className="h-4 w-4" />链接</DropdownMenuItem>
            <DropdownMenuItem className={composerMenuItemClass} onSelect={() => openInsertDialog("image")}><Image className="h-4 w-4" />图片链接</DropdownMenuItem>
            <DropdownMenuItem className={composerMenuItemClass} onSelect={() => editor?.chain().focus().setHorizontalRule().run()}><span className="h-4 w-4 border-t border-current" aria-hidden />分隔线</DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
        <ToolbarTextButton label="日程" icon={<Calendar className="h-4 w-4" />} onClick={() => setScheduleOpen(true)} />
        <DropdownMenu open={emojiOpen} onOpenChange={setEmojiOpen}>
          <DropdownMenuTrigger asChild>
            <Button type="button" variant={emojiOpen ? "secondary" : "ghost"} size="sm" className={cn("h-8 gap-1.5 rounded-md px-2 font-normal hover:bg-accent hover:shadow-sm", emojiOpen && "border border-primary/30 bg-primary/10 text-primary")} onMouseDown={(event) => event.preventDefault()}>
              <Smile className="h-4 w-4" />表情
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="start" className="w-64 p-2">
            <div className="grid grid-cols-8 gap-1">
              {composerEmojiOptions.map((emoji) => (
                <Button key={emoji} type="button" variant="ghost" size="icon" className="h-8 w-8 rounded-md text-lg" onClick={() => insertEmoji(emoji)}>
                  {emoji}
                </Button>
              ))}
            </div>
          </DropdownMenuContent>
        </DropdownMenu>
        <ToolbarTextButton label="格式" icon={<Type className="h-4 w-4" />} active={formatOpen} onClick={() => setFormatOpen((value) => !value)} />
        <div className="flex items-center gap-1">
          <ToolbarTextButton label="预览" icon={<Eye className="h-4 w-4" />} active={previewOpen} onClick={() => setPreviewOpen(true)} />
          <ToolbarTextButton label="签名" icon={<Signature className="h-4 w-4" />} onClick={insertSignature} disabled={!signatureText.trim()} />
        </div>
      </div>
      {formatOpen && (
        <div className="flex min-h-14 flex-wrap items-center gap-1 overflow-visible border-b bg-muted/40 px-3 py-2 sm:px-6">
          <ToolbarButton label="清除格式" disabled={!editor} onClick={() => editor?.chain().focus().unsetAllMarks().clearNodes().run()}><Eraser className="h-4 w-4" /></ToolbarButton>
          <Separator orientation="vertical" className="mx-2 h-6" />
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button type="button" variant="ghost" size="sm" className={cn("h-8 min-w-[112px] justify-between rounded-md border border-transparent px-2 font-normal hover:border-border hover:bg-accent hover:shadow-sm", activeFont && "border-primary/35 bg-primary/10 text-primary shadow-sm")} onMouseDown={(event) => event.preventDefault()} disabled={!editor}>
                <span className="truncate">{fontLabel(activeFont)}</span><ChevronDown className="h-3.5 w-3.5" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="start">
              {composerFontOptions.map((font) => (
                <DropdownMenuItem key={font} className={composerMenuItemClass} onSelect={() => applyFont(font)}>
                  <Check className={cn("h-4 w-4", activeFont === font ? "opacity-100" : "opacity-0")} />
                  <span style={{ fontFamily: font }}>{font}</span>
                </DropdownMenuItem>
              ))}
            </DropdownMenuContent>
          </DropdownMenu>
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button type="button" variant="ghost" size="sm" className={cn("h-8 min-w-[84px] justify-between rounded-md border border-transparent px-2 font-normal hover:border-border hover:bg-accent hover:shadow-sm", activeFontSize !== "3" && "border-primary/35 bg-primary/10 text-primary shadow-sm")} onMouseDown={(event) => event.preventDefault()} disabled={!editor}>
                {fontSizeLabel(activeFontSize)}<ChevronDown className="h-3.5 w-3.5" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="start">
              {composerFontSizeOptions.map(([size, label]) => (
                <DropdownMenuItem key={size} className={composerMenuItemClass} onSelect={() => applyFontSize(size)}>
                  <Check className={cn("h-4 w-4", activeFontSize === size ? "opacity-100" : "opacity-0")} />
                  {label}
                </DropdownMenuItem>
              ))}
            </DropdownMenuContent>
          </DropdownMenu>
          <Separator orientation="vertical" className="mx-2 h-6" />
          <ToolbarButton label="加粗" active={editor?.isActive("bold")} disabled={!editor} onClick={() => editor?.chain().focus().toggleBold().run()}><Bold className="h-4 w-4" /></ToolbarButton>
          <ToolbarButton label="斜体" active={editor?.isActive("italic")} disabled={!editor} onClick={() => editor?.chain().focus().toggleItalic().run()}><Italic className="h-4 w-4" /></ToolbarButton>
          <ToolbarButton label="下划线" active={editor?.isActive("underline")} disabled={!editor} onClick={() => editor?.chain().focus().toggleUnderline().run()}><Underline className="h-4 w-4" /></ToolbarButton>
          <ToolbarButton label="删除线" active={editor?.isActive("strike")} disabled={!editor} onClick={() => editor?.chain().focus().toggleStrike().run()}><Strikethrough className="h-4 w-4" /></ToolbarButton>
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button type="button" variant="ghost" size="icon" className={cn("h-8 w-8 rounded-md border border-transparent text-muted-foreground hover:border-border hover:bg-accent hover:text-foreground hover:shadow-sm", activeColor && "border-primary/35 bg-primary/10 text-primary shadow-sm")} title="文字颜色" aria-label="文字颜色" onMouseDown={(event) => event.preventDefault()} disabled={!editor}>
                <Type className="h-4 w-4" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="start" className="w-36">
              {composerTextColors.map(([color, label]) => (
                <DropdownMenuItem key={color} className={composerMenuItemClass} onSelect={() => color === "#111827" ? editor?.chain().focus().unsetColor().run() : editor?.chain().focus().setColor(color).run()}>
                  <Check className={cn("h-4 w-4", activeColor === color || (!activeColor && color === "#111827") ? "opacity-100" : "opacity-0")} />
                  <span className="h-3 w-3 rounded-full border" style={{ backgroundColor: color }} />{label}
                </DropdownMenuItem>
              ))}
            </DropdownMenuContent>
          </DropdownMenu>
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button type="button" variant="ghost" size="icon" className={cn("h-8 w-8 rounded-md border border-transparent text-muted-foreground hover:border-border hover:bg-accent hover:text-foreground hover:shadow-sm", activeHighlight && "border-primary/35 bg-primary/10 text-primary shadow-sm")} title="高亮" aria-label="高亮" onMouseDown={(event) => event.preventDefault()} disabled={!editor}>
                <Highlighter className="h-4 w-4" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="start">
              {composerHighlightColors.map(([color, label]) => (
                <DropdownMenuItem key={color} className={composerMenuItemClass} onSelect={() => color === "transparent" ? editor?.chain().focus().unsetBackgroundColor().run() : editor?.chain().focus().setBackgroundColor(color).run()}>
                  <Check className={cn("h-4 w-4", activeHighlight === color || (!activeHighlight && color === "transparent") ? "opacity-100" : "opacity-0")} />
                  <span className="h-3 w-3 rounded-sm border" style={{ backgroundColor: color }} />{label}
                </DropdownMenuItem>
              ))}
            </DropdownMenuContent>
          </DropdownMenu>
          <Separator orientation="vertical" className="mx-2 h-6" />
          <ToolbarButton label="无序列表" active={editor?.isActive("bulletList")} disabled={!editor} onClick={() => editor?.chain().focus().toggleBulletList().run()}><List className="h-4 w-4" /></ToolbarButton>
          <ToolbarButton label="有序列表" active={editor?.isActive("orderedList")} disabled={!editor} onClick={() => editor?.chain().focus().toggleOrderedList().run()}><ListOrdered className="h-4 w-4" /></ToolbarButton>
          <ToolbarButton label="减少缩进" disabled={!editor?.can().liftListItem("listItem")} onClick={() => editor?.chain().focus().liftListItem("listItem").run()}><IndentDecrease className="h-4 w-4" /></ToolbarButton>
          <ToolbarButton label="增加缩进" disabled={!editor?.can().sinkListItem("listItem")} onClick={() => editor?.chain().focus().sinkListItem("listItem").run()}><IndentIncrease className="h-4 w-4" /></ToolbarButton>
          <Separator orientation="vertical" className="mx-2 h-6" />
          <ToolbarButton label="左对齐" active={editor?.isActive({ textAlign: "left" })} disabled={!editor} onClick={() => editor?.chain().focus().setTextAlign("left").run()}><AlignLeft className="h-4 w-4" /></ToolbarButton>
          <ToolbarButton label="居中" active={editor?.isActive({ textAlign: "center" })} disabled={!editor} onClick={() => editor?.chain().focus().setTextAlign("center").run()}><AlignCenter className="h-4 w-4" /></ToolbarButton>
          <ToolbarButton label="右对齐" active={editor?.isActive({ textAlign: "right" })} disabled={!editor} onClick={() => editor?.chain().focus().setTextAlign("right").run()}><AlignRight className="h-4 w-4" /></ToolbarButton>
          <ToolbarButton label="引用" active={editor?.isActive("blockquote")} disabled={!editor} onClick={() => editor?.chain().focus().toggleBlockquote().run()}><Quote className="h-4 w-4" /></ToolbarButton>
          <ToolbarButton label="代码块" active={editor?.isActive("codeBlock")} disabled={!editor} onClick={() => editor?.chain().focus().toggleCodeBlock().run()}><Code2 className="h-4 w-4" /></ToolbarButton>
        </div>
      )}
      <div className={cn(
        "composer-editor relative flex min-h-[240px] flex-1 border-b focus-within:bg-card/40 sm:min-h-[280px]",
        "[&_.ProseMirror]:min-h-[240px] [&_.ProseMirror]:w-full [&_.ProseMirror]:flex-1 [&_.ProseMirror]:overflow-y-auto [&_.ProseMirror]:px-4 [&_.ProseMirror]:py-4 [&_.ProseMirror]:text-base [&_.ProseMirror]:leading-7 [&_.ProseMirror]:outline-none sm:[&_.ProseMirror]:min-h-[280px] sm:[&_.ProseMirror]:px-6 sm:[&_.ProseMirror]:py-5",
        "[&_.ProseMirror_p.is-editor-empty:first-child::before]:pointer-events-none [&_.ProseMirror_p.is-editor-empty:first-child::before]:float-left [&_.ProseMirror_p.is-editor-empty:first-child::before]:h-0 [&_.ProseMirror_p.is-editor-empty:first-child::before]:text-muted-foreground [&_.ProseMirror_p.is-editor-empty:first-child::before]:content-[attr(data-placeholder)]",
        "[&_.ProseMirror_ul]:list-disc [&_.ProseMirror_ol]:list-decimal [&_.ProseMirror_ul]:pl-6 [&_.ProseMirror_ol]:pl-6 [&_.ProseMirror_blockquote]:border-l-4 [&_.ProseMirror_blockquote]:border-border [&_.ProseMirror_blockquote]:pl-4 [&_.ProseMirror_blockquote]:text-muted-foreground [&_.ProseMirror_pre]:rounded-md [&_.ProseMirror_pre]:bg-muted [&_.ProseMirror_pre]:p-3",
        empty && "bg-background"
      )}>
        <EditorContent editor={editor} className="flex min-h-0 flex-1" />
      </div>
      {files.length > 0 && (
        <div className="border-t px-4 py-3 sm:px-6">
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
      <InsertContentDialog state={insertDialog} onOpenChange={(open) => { if (!open) setInsertDialog(null) }} onConfirm={confirmInsert} />
      <ScheduleDialog open={scheduleOpen} onOpenChange={setScheduleOpen} onConfirm={(schedule) => { insertSchedule(schedule); setScheduleOpen(false) }} />
      <Dialog open={previewOpen} onOpenChange={setPreviewOpen}>
        <DialogContent className="w-[min(92vw,44rem)] max-w-none">
          <DialogHeader>
            <DialogTitle>邮件预览</DialogTitle>
          </DialogHeader>
          <div className="mail-html max-h-[60vh] overflow-y-auto rounded-md border bg-background p-5 text-sm leading-7" dangerouslySetInnerHTML={{ __html: sanitizeComposerHtml(editor?.getHTML() || "") || "<p></p>" }} />
        </DialogContent>
      </Dialog>
    </div>
  )
}

function ToolbarTextButton({ label, icon, active, disabled, onClick }: { label: string; icon: React.ReactNode; active?: boolean; disabled?: boolean; onClick?: () => void }) {
  return (
    <Button type="button" variant={active ? "secondary" : "ghost"} size="sm" className={cn("h-8 gap-1.5 rounded-md px-2 font-normal transition-all hover:bg-accent hover:text-foreground hover:shadow-sm", active && "border border-primary/30 bg-primary/10 text-primary shadow-sm")} title={label} aria-label={label} aria-pressed={active || undefined} onMouseDown={(event) => event.preventDefault()} onClick={onClick} disabled={disabled}>
      {icon}{label}
    </Button>
  )
}

function InsertContentDialog({ state, onOpenChange, onConfirm }: { state: InsertDialogState | null; onOpenChange: (open: boolean) => void; onConfirm: (value: InsertDialogValue) => void }) {
  const kind = state?.kind || "link"
  const [url, setUrl] = React.useState("")
  const [text, setText] = React.useState("")
  const [alt, setAlt] = React.useState("")

  React.useEffect(() => {
    if (!state) return
    setUrl(state.url || "")
    setText(state.kind === "link" ? state.selectedText : "")
    setAlt(state.alt || "")
  }, [state])

  function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!url.trim()) return
    onConfirm({ url, text, alt })
    onOpenChange(false)
  }

  return (
    <Dialog open={!!state} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <form className="grid gap-4" onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>{kind === "link" ? (state?.editing ? "编辑链接" : "插入链接") : (state?.editing ? "编辑图片" : "插入图片")}</DialogTitle>
          </DialogHeader>
          <div className="grid gap-2">
            <Label htmlFor="composer-insert-url">{kind === "link" ? "链接地址" : "图片地址"}</Label>
            <Input id="composer-insert-url" value={url} onChange={(event) => setUrl(event.target.value)} placeholder={kind === "link" ? "https://example.com" : "https://example.com/image.png"} autoFocus />
          </div>
          {kind === "link" ? (
            <div className="grid gap-2">
              <Label htmlFor="composer-insert-text">显示文字</Label>
              <Input id="composer-insert-text" value={text} onChange={(event) => setText(event.target.value)} placeholder="默认使用链接地址" />
            </div>
          ) : (
            <div className="grid gap-2">
              <Label htmlFor="composer-insert-alt">替代文字</Label>
              <Input id="composer-insert-alt" value={alt} onChange={(event) => setAlt(event.target.value)} placeholder="图片说明" />
            </div>
          )}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>取消</Button>
            <Button type="submit">{state?.editing ? "更新" : "插入"}</Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

type ScheduleDraft = {
  title: string
  start: string
  durationMinutes: number
  reminderMinutes: number
  repeat: "none" | "daily" | "weekly" | "monthly" | "yearly"
  allDay: boolean
  customDuration: boolean
  customReminder: boolean
  lunar: boolean
  location: string
  description: string
}

const durationOptions = [
  { value: "15", label: "15分钟" },
  { value: "30", label: "30分钟" },
  { value: "60", label: "1小时" },
  { value: "120", label: "2小时" },
  { value: "1440", label: "1天" },
]
const reminderOptions = [
  { value: "0", label: "准时" },
  { value: "5", label: "5分钟前" },
  { value: "15", label: "15分钟前" },
  { value: "30", label: "30分钟前" },
  { value: "60", label: "1小时前" },
  { value: "1440", label: "1天前" },
]
const repeatOptions = [
  { value: "none", label: "永不" },
  { value: "daily", label: "每天" },
  { value: "weekly", label: "每周" },
  { value: "monthly", label: "每月" },
  { value: "yearly", label: "每年" },
] as const

function ScheduleDialog({ open, onOpenChange, onConfirm }: { open: boolean; onOpenChange: (open: boolean) => void; onConfirm: (schedule: ScheduleDraft) => void }) {
  const [duration, setDuration] = React.useState("60")
  const [reminder, setReminder] = React.useState("15")
  const [repeat, setRepeat] = React.useState<ScheduleDraft["repeat"]>("none")
  const [allDay, setAllDay] = React.useState(false)
  const [customDuration, setCustomDuration] = React.useState(false)
  const [customReminder, setCustomReminder] = React.useState(false)
  const [lunar, setLunar] = React.useState(false)
  const defaultStart = React.useMemo(() => defaultScheduleStartValue(), [open])
  const { toast } = useToast()

  function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const form = new FormData(event.currentTarget)
    const title = String(form.get("title") || "").trim()
    if (!title) {
      toast({ title: "请输入日程主题" })
      return
    }
    const durationMinutes = customDuration ? Number(form.get("customDuration") || 60) : Number(duration)
    const reminderMinutes = customReminder ? Number(form.get("customReminder") || 15) : Number(reminder)
    onConfirm({
      title,
      start: String(form.get("start") || defaultStart),
      durationMinutes: Math.max(1, durationMinutes || 60),
      reminderMinutes: Math.max(0, reminderMinutes || 0),
      repeat,
      allDay,
      customDuration,
      customReminder,
      lunar,
      location: String(form.get("location") || ""),
      description: String(form.get("description") || ""),
    })
    event.currentTarget.reset()
    setDuration("60")
    setReminder("15")
    setRepeat("none")
    setAllDay(false)
    setCustomDuration(false)
    setCustomReminder(false)
    setLunar(false)
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="w-[min(92vw,36rem)] max-w-none">
        <DialogHeader>
          <DialogTitle>新建日程</DialogTitle>
        </DialogHeader>
        <form className="space-y-5" onSubmit={submit}>
          <Input name="title" placeholder="输入日程主题" className="h-11 border-0 border-b px-0 text-lg shadow-none focus-visible:ring-0" />
          <div className="grid gap-4">
            <ScheduleRow label="开始">
              <Input name="start" type={allDay ? "date" : "datetime-local"} defaultValue={allDay ? defaultStart.slice(0, 10) : defaultStart} className="h-11" />
              <CheckLabel id="schedule-all-day" label="全天" checked={allDay} onCheckedChange={setAllDay} />
            </ScheduleRow>
            <ScheduleRow label="持续">
              {customDuration ? (
                <Input name="customDuration" type="number" min={1} defaultValue="60" className="h-11" />
              ) : (
                <Select value={duration} onValueChange={setDuration}>
                  <SelectTrigger className="h-11"><SelectValue /></SelectTrigger>
                  <SelectContent>{durationOptions.map((item) => <SelectItem key={item.value} value={item.value}>{item.label}</SelectItem>)}</SelectContent>
                </Select>
              )}
              <CheckLabel id="schedule-custom-duration" label="自定义" checked={customDuration} onCheckedChange={setCustomDuration} />
            </ScheduleRow>
            <ScheduleRow label="提醒">
              {customReminder ? (
                <Input name="customReminder" type="number" min={0} defaultValue="15" className="h-11" />
              ) : (
                <Select value={reminder} onValueChange={setReminder}>
                  <SelectTrigger className="h-11"><SelectValue /></SelectTrigger>
                  <SelectContent>{reminderOptions.map((item) => <SelectItem key={item.value} value={item.value}>{item.label}</SelectItem>)}</SelectContent>
                </Select>
              )}
              <CheckLabel id="schedule-custom-reminder" label="自定义" checked={customReminder} onCheckedChange={setCustomReminder} />
            </ScheduleRow>
            <ScheduleRow label="重复">
              <Select value={repeat} onValueChange={(value) => setRepeat(value as ScheduleDraft["repeat"])}>
                <SelectTrigger className="h-11"><SelectValue /></SelectTrigger>
                <SelectContent>{repeatOptions.map((item) => <SelectItem key={item.value} value={item.value}>{item.label}</SelectItem>)}</SelectContent>
              </Select>
              <CheckLabel id="schedule-lunar" label="农历" checked={lunar} onCheckedChange={setLunar} />
            </ScheduleRow>
            <ScheduleRow label="位置">
              <Input name="location" placeholder="请输入位置" className="h-11" />
            </ScheduleRow>
            <ScheduleRow label="描述">
              <Input name="description" placeholder="输入描述" className="h-11" />
            </ScheduleRow>
          </div>
          <DialogFooter>
            <Button type="submit">确定</Button>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>取消</Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function ScheduleRow({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="grid gap-2 sm:grid-cols-[3rem_minmax(0,1fr)_5.5rem] sm:items-center">
      <Label className="text-base font-normal">{label}</Label>
      {children}
    </div>
  )
}

function CheckLabel({ id, label, checked, onCheckedChange }: { id: string; label: string; checked: boolean; onCheckedChange: (checked: boolean) => void }) {
  return (
    <div className="flex items-center gap-2">
      <Checkbox id={id} checked={checked} onCheckedChange={(value) => onCheckedChange(value === true)} />
      <Label htmlFor={id} className="cursor-pointer text-sm font-normal">{label}</Label>
    </div>
  )
}

function ToolbarButton({ label, children, active, onClick, disabled }: { label: string; children: React.ReactNode; active?: boolean; onClick?: () => void; disabled?: boolean }) {
  return (
    <Button
      type="button"
      variant="ghost"
      size="icon"
      className={cn(
        "h-8 w-8 rounded-md border border-transparent text-muted-foreground transition-all hover:border-border hover:bg-accent hover:text-foreground hover:shadow-sm",
        active && "border-primary/35 bg-primary/10 text-primary shadow-sm hover:bg-primary/15 hover:text-primary"
      )}
      title={label}
      aria-label={label}
      aria-pressed={active || undefined}
      onMouseDown={(event) => event.preventDefault()}
      onClick={onClick}
      disabled={disabled}
    >
      {children}
    </Button>
  )
}

function splitEmails(s: string) { return s.split(/[;,，\s]+/).map((v) => v.trim()).filter(Boolean) }
function defaultScheduledSendValue() {
  const date = new Date(Date.now() + 30 * 60 * 1000)
  const minute = date.getMinutes()
  date.setMinutes(minute + (5 - (minute % 5 || 5)))
  return toDateTimeLocalValue(date)
}
function scheduledSendPresets() {
  return [
    { label: "30 分钟后", value: toDateTimeLocalValue(new Date(Date.now() + 30 * 60 * 1000)) },
    { label: "2 小时后", value: toDateTimeLocalValue(new Date(Date.now() + 2 * 60 * 60 * 1000)) },
    { label: "明早 9 点", value: toDateTimeLocalValue(nextMorningAtNine()) },
    { label: "下周一 9 点", value: toDateTimeLocalValue(nextMondayAtNine()) },
  ]
}
function nextMorningAtNine() {
  const date = new Date()
  date.setDate(date.getDate() + 1)
  date.setHours(9, 0, 0, 0)
  return date
}
function nextMondayAtNine() {
  const date = new Date()
  const day = date.getDay()
  const daysUntilMonday = (8 - day) % 7 || 7
  date.setDate(date.getDate() + daysUntilMonday)
  date.setHours(9, 0, 0, 0)
  return date
}
function defaultScheduleStartValue() {
  const date = new Date()
  date.setMinutes(date.getMinutes() + (60 - (date.getMinutes() % 60 || 60)))
  return toDateTimeLocalValue(date)
}
function toDateTimeLocalValue(date: Date) {
  const pad = (value: number) => String(value).padStart(2, "0")
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`
}
function normalizeSchedule(schedule: ScheduleDraft): ScheduleDraft {
  return { ...schedule, title: schedule.title.trim(), location: schedule.location.trim(), description: schedule.description.trim() }
}
function scheduleToHtml(schedule: ScheduleDraft) {
  const start = parseScheduleStart(schedule)
  const end = schedule.allDay ? new Date(start.getTime() + 24 * 60 * 60 * 1000) : new Date(start.getTime() + schedule.durationMinutes * 60 * 1000)
  const rows = [
    ["时间", schedule.allDay ? formatDate(start.toISOString()) : `${formatDateTime(start.toISOString())} - ${formatTimeOnly(end)}`],
    ["持续", schedule.allDay ? "全天" : durationLabel(schedule.durationMinutes)],
    ["提醒", reminderLabel(schedule.reminderMinutes)],
    ["重复", repeatLabel(schedule.repeat)],
    schedule.location ? ["位置", schedule.location] : undefined,
    schedule.description ? ["描述", schedule.description] : undefined,
  ].filter(Boolean) as string[][]
  return DOMPurify.sanitize(`
    <div style="border:1px solid #d4d4d8;border-radius:8px;padding:14px 16px;margin:16px 0;background:#fafafa;">
      <div style="font-weight:600;font-size:16px;margin-bottom:10px;">${escapeHtml(schedule.title)}</div>
      ${rows.map(([label, value]) => `<div style="margin:6px 0;"><span style="color:#71717a;">${label}：</span>${escapeHtml(value)}</div>`).join("")}
    </div>
  `)
}
function scheduleToFile(schedule: ScheduleDraft) {
  const ics = scheduleToIcs(schedule)
  const filename = `${safeFilename(schedule.title || "schedule")}.ics`
  return new File([ics], filename, { type: "text/calendar;charset=utf-8" })
}
function scheduleToIcs(schedule: ScheduleDraft) {
  const start = parseScheduleStart(schedule)
  const end = schedule.allDay ? new Date(start.getTime() + 24 * 60 * 60 * 1000) : new Date(start.getTime() + schedule.durationMinutes * 60 * 1000)
  const uid = `${Date.now()}-${Math.random().toString(36).slice(2)}@lanqin-email`
  const lines = [
    "BEGIN:VCALENDAR",
    "VERSION:2.0",
    "PRODID:-//LanQin Email//Webmail//CN",
    "CALSCALE:GREGORIAN",
    "METHOD:PUBLISH",
    "BEGIN:VEVENT",
    `UID:${uid}`,
    `DTSTAMP:${toIcsDateTime(new Date())}`,
    schedule.allDay ? `DTSTART;VALUE=DATE:${toIcsDate(start)}` : `DTSTART:${toIcsDateTime(start)}`,
    schedule.allDay ? `DTEND;VALUE=DATE:${toIcsDate(end)}` : `DTEND:${toIcsDateTime(end)}`,
    `SUMMARY:${escapeIcs(schedule.title)}`,
    schedule.location ? `LOCATION:${escapeIcs(schedule.location)}` : "",
    schedule.description ? `DESCRIPTION:${escapeIcs(schedule.description)}` : "",
    schedule.repeat !== "none" ? `RRULE:FREQ=${schedule.repeat.toUpperCase()}` : "",
  ].filter(Boolean)
  if (schedule.reminderMinutes > 0) {
    lines.push("BEGIN:VALARM", `TRIGGER:-PT${schedule.reminderMinutes}M`, "ACTION:DISPLAY", `DESCRIPTION:${escapeIcs(schedule.title)}`, "END:VALARM")
  }
  lines.push("END:VEVENT", "END:VCALENDAR")
  return `${lines.join("\r\n")}\r\n`
}
function parseScheduleStart(schedule: ScheduleDraft) {
  const value = schedule.allDay ? `${schedule.start.slice(0, 10)}T00:00` : schedule.start
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? new Date() : date
}
function toIcsDateTime(date: Date) {
  const pad = (value: number) => String(value).padStart(2, "0")
  return `${date.getUTCFullYear()}${pad(date.getUTCMonth() + 1)}${pad(date.getUTCDate())}T${pad(date.getUTCHours())}${pad(date.getUTCMinutes())}${pad(date.getUTCSeconds())}Z`
}
function toIcsDate(date: Date) {
  const pad = (value: number) => String(value).padStart(2, "0")
  return `${date.getFullYear()}${pad(date.getMonth() + 1)}${pad(date.getDate())}`
}
function formatTimeOnly(date: Date) {
  return date.toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit" })
}
function durationLabel(minutes: number) {
  if (minutes % 1440 === 0) return `${minutes / 1440}天`
  if (minutes % 60 === 0) return `${minutes / 60}小时`
  return `${minutes}分钟`
}
function reminderLabel(minutes: number) {
  if (minutes <= 0) return "准时"
  return `${durationLabel(minutes)}前`
}
function repeatLabel(repeat: ScheduleDraft["repeat"]) {
  return ({ none: "永不", daily: "每天", weekly: "每周", monthly: "每月", yearly: "每年" } as Record<ScheduleDraft["repeat"], string>)[repeat]
}
function safeFilename(value: string) {
  return value.trim().replace(/[\\/:*?"<>|]+/g, "-").replace(/\s+/g, "-").slice(0, 64) || "schedule"
}
function escapeIcs(value: string) {
  return value.replace(/\\/g, "\\\\").replace(/\n/g, "\\n").replace(/,/g, "\\,").replace(/;/g, "\\;")
}
function plainTextComposerValue(value: string): ComposerValue { return { text: value, html: plainTextToHtml(value) } }
function htmlComposerValue(value: string): ComposerValue {
  const html = sanitizeComposerHtml(value || "")
  const text = stripHtml(html)
  return { text, html: html || plainTextToHtml(text) }
}
function plainTextToHtml(value: string) {
  const normalized = value.replace(/\r\n/g, "\n")
  if (!normalized.trim()) return ""
  return sanitizeComposerHtml(normalized.split(/\n{2,}/).map((paragraph) => `<p>${plainTextToHtmlFragment(paragraph) || "<br>"}</p>`).join(""))
}
function plainTextToHtmlFragment(value: string) { return value.split("\n").map((line) => escapeHtml(line)).join("<br>") }
function escapeHtml(value: string) {
  return value.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;").replace(/'/g, "&#39;")
}
function sanitizeComposerHtml(value: string) {
  return DOMPurify.sanitize(value || "")
}
function htmlContainsMeaningfulContent(html: string) {
  return /<(img|hr|table|ul|ol|li|blockquote|pre|div)[\s>]/i.test(html) || stripHtml(html).trim().length > 0
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
async function attachmentFilesFromMessage(message: MailMessage) {
  if (!message.attachments?.length) return []
  return Promise.all(message.attachments.map(async (attachment) => {
    const response = await fetch(`/api/mail/attachments/${attachment.id}`, { credentials: "include" })
    const blob = await response.blob()
    return new File([blob], attachment.filename, { type: attachment.contentType || blob.type || "application/octet-stream" })
  }))
}

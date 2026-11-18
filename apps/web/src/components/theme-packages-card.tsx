import * as React from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { ConfirmDialog } from "@/components/confirm-dialog"
import { LayoutPreviewDialog } from "@/components/layout-preview-dialog"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { uiMessage, uiText, useLanguage } from "@/lib/language"
import {
  themePackageApi,
  themePackagesKey,
  type ThemePackage,
  type ThemePackageCatalog,
} from "@/lib/theme-packages"
import { activeAdminLayoutKey, publicLayoutKey } from "@/lib/template-layout"
import { ApiError, errorMessage } from "@/lib/ui-errors"
import { CLASSIC_LAYOUT, type LayoutSurface } from "@/themes/layouts/catalog"

const maxPackageBytes = 1024 * 1024

const surfaces = [
  { id: "front", label: "用户前台" },
  { id: "admin", label: "管理后台" },
] as const

type PackageAction =
  | { kind: "upload"; file: File }
  | { kind: "github"; url: string }
  | { kind: "activate"; surface: LayoutSurface; id: string; version?: string }
  | { kind: "update"; pack: ThemePackage; source: string }
  | { kind: "rollback"; pack: ThemePackage; previousVersion: string }
  | { kind: "delete"; pack: ThemePackage }

type ConfirmAction = Extract<PackageAction, { pack: ThemePackage }>

async function runAction(action: PackageAction) {
  switch (action.kind) {
    case "upload":
      return themePackageApi.upload(action.file)
    case "github":
      return themePackageApi.github(action.url)
    case "activate":
      return themePackageApi.activate(action.surface, action.id, action.version)
    case "update":
      return themePackageApi.github(action.source, {
        id: action.pack.id,
        expectedVersion: action.pack.version,
      })
    case "rollback":
      return themePackageApi.rollback({
        id: action.pack.id,
        expectedVersion: action.pack.version,
        previousVersion: action.previousVersion,
      })
    case "delete":
      return themePackageApi.remove(action.pack.id)
  }
}

function validGithubUrl(raw: string) {
  try {
    const url = new URL(raw)
    const paths = decodeURIComponent(url.pathname).replace(/\/$/, "").split("/").slice(1)
    const [owner, repo, releases, mode, tag, filename] = paths
    const validLocation =
      !!owner && !!repo && /^[A-Za-z0-9_.-]+$/.test(owner) && /^[A-Za-z0-9_.-]+$/.test(repo)
    const validTag = !!tag && /^[A-Za-z0-9._+-]{1,128}$/.test(tag) && tag !== "." && tag !== ".."
    const isSource =
      paths.length === 2 ||
      (releases === "releases" &&
        validTag &&
        ((paths.length === 5 && mode === "tag") ||
          (paths.length === 6 && mode === "download" && filename === "theme.tar.gz")))
    return (
      url.protocol === "https:" &&
      url.hostname === "github.com" &&
      !url.username &&
      !url.password &&
      !url.port &&
      !url.search &&
      !url.hash &&
      validLocation &&
      isSource
    )
  } catch {
    return false
  }
}

function receipt(action: PackageAction) {
  switch (action.kind) {
    case "upload":
    case "github":
      return "主题包已安装，尚未启用"
    case "activate":
      return "主题包启用设置已保存。重新打开页面以应用新布局。"
    case "update":
      return "主题包已更新，已启用的版本保持不变"
    case "rollback":
      return "主题包已回退，已启用的版本保持不变"
    case "delete":
      return "主题包已删除"
  }
}

export function ThemePackagesCard({ canUpdate }: { canUpdate: boolean }) {
  useLanguage()
  const client = useQueryClient()
  const query = useQuery({
    queryKey: themePackagesKey,
    queryFn: themePackageApi.list,
    refetchOnWindowFocus: true,
  })
  const [file, setFile] = React.useState<File>()
  const fileInput = React.useRef<HTMLInputElement>(null)
  const [githubUrl, setGithubUrl] = React.useState("")
  const [confirm, setConfirm] = React.useState<ConfirmAction>()
  const [preview, setPreview] = React.useState<{
    surface: LayoutSurface
    pack: ThemePackage
    focusReturn: HTMLElement
  }>()
  const [feedback, setFeedback] = React.useState<string>()
  const [inputError, setInputError] = React.useState<string>()
  const action = useMutation({
    mutationFn: runAction,
    onSuccess: async (catalog, submitted) => {
      await client.cancelQueries({ queryKey: themePackagesKey, exact: true })
      client.setQueryData(themePackagesKey, catalog)
      if (submitted.kind === "activate") {
        void client.invalidateQueries({ queryKey: publicLayoutKey, exact: true })
        void client.invalidateQueries({ queryKey: activeAdminLayoutKey, exact: true })
      }
      if (submitted.kind === "upload") {
        setFile(undefined)
        if (fileInput.current) fileInput.current.value = ""
      }
      if (submitted.kind === "github") setGithubUrl("")
      setConfirm(undefined)
      setFeedback(receipt(submitted))
    },
    onError: (error) => {
      if (error instanceof ApiError && (error.status === 404 || error.status === 409)) {
        void client.invalidateQueries({ queryKey: themePackagesKey, exact: true })
      }
    },
  })
  const actionError =
    action.error instanceof ApiError && action.error.status === 413
      ? "主题包不得超过 1 MiB"
      : action.isError
        ? errorMessage(action.error)
        : undefined
  const catalog = query.data
  const disabled = !canUpdate || !catalog || action.isPending

  function submit(value: PackageAction) {
    if (disabled) return
    setInputError(undefined)
    setFeedback(undefined)
    action.mutate(value)
  }

  function openConfirmation(value: ConfirmAction) {
    if (disabled) return
    action.reset()
    setConfirm(value)
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>{uiText("主题管理")}</CardTitle>
      </CardHeader>
      <CardContent className="space-y-5">
        {!catalog && query.isPending && <p role="status">{uiText("正在读取主题包")}</p>}
        {!catalog && query.isError && (
          <div className="space-y-2">
            <p role="alert">{uiText("无法读取主题包")}</p>
            <Button variant="outline" type="button" onClick={() => void query.refetch()}>
              {uiText("重试")}
            </Button>
          </div>
        )}
        {catalog && (
          <>
            <div className="flex flex-wrap gap-2">
              {surfaces.map(({ id, label }) => {
                const active = catalog.packages.find((item) => item.id === catalog.active[id])
                const version = catalog.activeVersions?.[id]
                return (
                  <Badge key={id} variant="outline" data-surface-status={id}>
                    {uiText(label)}：{" "}
                    {active ? (
                      <span data-lanqin-i18n-ignore>
                        {active.name}
                        {version ? ` · ${version}` : ""}
                      </span>
                    ) : (
                      uiText("站点默认外观")
                    )}
                  </Badge>
                )
              })}
            </div>
            {canUpdate ? (
              <div className="grid gap-3 lg:grid-cols-2">
                <form
                  className="space-y-2 rounded-md border p-3"
                  onSubmit={(event) => {
                    event.preventDefault()
                    if (file) submit({ kind: "upload", file })
                  }}
                >
                  <Label htmlFor="theme-package-upload">{uiText("本地压缩包")}</Label>
                  <Input
                    id="theme-package-upload"
                    ref={fileInput}
                    type="file"
                    accept=".tar.gz,.tgz,application/gzip"
                    disabled={disabled}
                    onChange={(event) => {
                      const selected = event.currentTarget.files?.[0]
                      action.reset()
                      setFeedback(undefined)
                      if (selected && !/\.(tar\.gz|tgz)$/i.test(selected.name)) {
                        event.currentTarget.value = ""
                        setFile(undefined)
                        setInputError("请选择 .tar.gz 或 .tgz 主题包")
                      } else if (selected && selected.size > maxPackageBytes) {
                        event.currentTarget.value = ""
                        setFile(undefined)
                        setInputError("主题包不得超过 1 MiB")
                      } else {
                        setFile(selected)
                        setInputError(undefined)
                      }
                    }}
                  />
                  <Button type="submit" disabled={disabled || !file}>
                    {uiText("安装本地主题包")}
                  </Button>
                </form>
                <form
                  className="space-y-2 rounded-md border p-3"
                  onSubmit={(event) => {
                    event.preventDefault()
                    const url = githubUrl.trim()
                    if (!validGithubUrl(url)) {
                      action.reset()
                      setFeedback(undefined)
                      setInputError("请输入公开 GitHub 仓库或发布链接")
                      return
                    }
                    submit({ kind: "github", url })
                  }}
                >
                  <Label htmlFor="theme-package-github">{uiText("GitHub 仓库或发布地址")}</Label>
                  <Input
                    id="theme-package-github"
                    type="url"
                    inputMode="url"
                    placeholder={uiText("GitHub 地址示例")}
                    value={githubUrl}
                    disabled={disabled}
                    required
                    onChange={(event) => {
                      setGithubUrl(event.currentTarget.value)
                      setInputError(undefined)
                      action.reset()
                      setFeedback(undefined)
                    }}
                  />
                  <Button type="submit" disabled={disabled || !githubUrl.trim()}>
                    {uiText("从 GitHub 安装")}
                  </Button>
                </form>
              </div>
            ) : (
              <p>{uiText("你没有修改主题的权限")}</p>
            )}
            {inputError && <p role="alert">{uiText(inputError)}</p>}
            {feedback && <p role="status">{uiText(feedback)}</p>}
            {action.isPending && <p role="status">{uiText("处理中...")}</p>}
            {actionError && !confirm && <p role="alert">{uiText(actionError)}</p>}

            {catalog.packages.length === 0 ? (
              <p>{uiText("尚未安装主题包")}</p>
            ) : (
              <div className="space-y-3">
                {catalog.packages.map((pack) => (
                  <PackageRow
                    key={pack.id}
                    pack={pack}
                    catalog={catalog}
                    canUpdate={canUpdate}
                    disabled={disabled}
                    onPreview={(surface, focusReturn) => setPreview({ surface, pack, focusReturn })}
                    onAction={submit}
                    onConfirm={openConfirmation}
                  />
                ))}
              </div>
            )}
          </>
        )}
        {preview && preview.pack.surfaces[preview.surface] && (
          <LayoutPreviewDialog
            surface={preview.surface}
            id={preview.pack.surfaces[preview.surface]!.layout}
            packagePreview={{
              id: preview.pack.id,
              name: preview.pack.name,
              appearance: preview.pack.surfaces[preview.surface]!,
              focusReturn: preview.focusReturn,
            }}
            onClose={() => setPreview(undefined)}
          />
        )}
        {confirm && (
          <ConfirmDialog
            open
            title={uiMessage(
              confirm.kind === "update"
                ? "更新主题包 {0}？"
                : confirm.kind === "rollback"
                  ? "回退主题包 {0}？"
                  : "删除主题包 {0}？",
              [confirm.pack.name]
            )}
            description={
              confirm.kind === "update"
                ? uiMessage("将从 {0} 重新获取主题包；不会切换正在使用的版本。", [confirm.source])
                : confirm.kind === "rollback"
                  ? uiMessage("将可用版本回退到 {0}；不会切换正在使用的版本。", [
                      confirm.previousVersion,
                    ])
                  : "删除后需重新安装才可使用。"
            }
            destructive={confirm.kind === "delete"}
            error={actionError}
            pending={action.isPending}
            onOpenChange={(open) => {
              if (!open && !action.isPending) setConfirm(undefined)
            }}
            onConfirm={() => submit(confirm)}
          />
        )}
      </CardContent>
    </Card>
  )
}

function PackageRow({
  pack,
  catalog,
  canUpdate,
  disabled,
  onPreview,
  onAction,
  onConfirm,
}: {
  pack: ThemePackage
  catalog: ThemePackageCatalog
  canUpdate: boolean
  disabled: boolean
  onPreview: (surface: LayoutSurface, focusReturn: HTMLElement) => void
  onAction: (action: PackageAction) => void
  onConfirm: (action: ConfirmAction) => void
}) {
  const inUse = surfaces.some(({ id }) => catalog.active[id] === pack.id)
  const { source, previousVersion } = pack
  return (
    <div
      role="group"
      aria-label={pack.name}
      className="space-y-3 rounded-lg border p-3"
      data-theme-package-id={pack.id}
    >
      <div className="flex flex-wrap items-center gap-2">
        <strong className="min-w-0 break-all" data-lanqin-i18n-ignore>
          {pack.name}
        </strong>
        <Badge variant="secondary">{uiText(uiMessage("主题包版本 {0}", [pack.version]))}</Badge>
        <Badge variant="outline">{uiText(uiMessage("API v{0}", [pack.apiVersion]))}</Badge>
        <Badge variant="outline">{uiText(pack.source ? "GitHub" : "本地上传")}</Badge>
      </div>
      {pack.source && (
        <div className="break-all text-xs text-muted-foreground" data-lanqin-i18n-ignore>
          {pack.source}
          {pack.release && <div>{pack.release}</div>}
        </div>
      )}
      <div className="grid gap-2 sm:grid-cols-2">
        {surfaces.map(({ id, label }) => {
          const installed = pack.surfaces[id]
          const active = catalog.active[id] === pack.id
          const activeVersion = catalog.activeVersions?.[id]
          const isCurrent = active && activeVersion === pack.version
          return (
            <div
              key={id}
              data-package-surface={id}
              className="space-y-2 rounded-md border p-2"
              role="group"
              aria-label={uiText(label)}
            >
              <div className="flex flex-wrap items-center gap-2">
                <strong>{uiText(label)}</strong>
                <Badge variant={active ? "default" : "outline"}>
                  {active && activeVersion
                    ? uiText(uiMessage("已启用版本 {0}", [activeVersion]))
                    : uiText(active ? "已启用" : installed ? "可启用" : "不支持")}
                </Badge>
              </div>
              {(installed || active) && (
                <div className="flex flex-wrap gap-2">
                  {installed && (
                    <Button
                      type="button"
                      size="sm"
                      variant="outline"
                      onClick={(event) => onPreview(id, event.currentTarget)}
                    >
                      {uiText("预览")}
                    </Button>
                  )}
                  {canUpdate && installed && !isCurrent && (
                    <Button
                      type="button"
                      size="sm"
                      disabled={disabled}
                      onClick={() =>
                        onAction({
                          kind: "activate",
                          surface: id,
                          id: pack.id,
                          version: pack.version,
                        })
                      }
                    >
                      {uiText("启用")}
                    </Button>
                  )}
                  {canUpdate && active && (
                    <Button
                      type="button"
                      size="sm"
                      variant="outline"
                      disabled={disabled}
                      onClick={() =>
                        onAction({ kind: "activate", surface: id, id: CLASSIC_LAYOUT })
                      }
                    >
                      {uiText("停用")}
                    </Button>
                  )}
                </div>
              )}
            </div>
          )
        })}
      </div>
      {canUpdate && (
        <div className="flex flex-wrap gap-2">
          {source && (
            <Button
              type="button"
              size="sm"
              variant="outline"
              disabled={disabled}
              onClick={() => onConfirm({ kind: "update", pack, source })}
            >
              {uiText("从 GitHub 更新")}
            </Button>
          )}
          {previousVersion && (
            <Button
              type="button"
              size="sm"
              variant="outline"
              disabled={disabled}
              onClick={() => onConfirm({ kind: "rollback", pack, previousVersion })}
            >
              {uiText("回退主题包")}
            </Button>
          )}
          <Button
            type="button"
            size="sm"
            variant="outline"
            disabled={disabled || inUse}
            onClick={() => onConfirm({ kind: "delete", pack })}
          >
            {uiText("删除主题包")}
          </Button>
        </div>
      )}
      {inUse && <p className="text-xs text-muted-foreground">{uiText("停用后可删除此主题包")}</p>}
    </div>
  )
}

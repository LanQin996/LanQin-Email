import { uiText, useLanguage as useUiLanguage } from "@/lib/language"
import { Button } from "@/components/ui/button"

export function AuthLoading() {
  useUiLanguage()

  return (
    <div className="grid min-h-screen place-items-center text-muted-foreground">
      {uiText("加载中...")}
    </div>
  )
}

export function AuthError({ message, onRetry }: { message: string; onRetry: () => void }) {
  useUiLanguage()

  return (
    <div className="grid min-h-screen place-items-center bg-background px-4">
      <div className="w-full max-w-sm space-y-4 text-center">
        <div className="text-sm font-medium">{uiText("无法连接后端服务")}</div>
        <div className="text-sm text-muted-foreground">{uiText(message)}</div>
        <Button type="button" variant="outline" onClick={onRetry}>
          {uiText("重新加载")}
        </Button>
      </div>
    </div>
  )
}

import { Button } from "@/components/ui/button"

export function AuthLoading() {
  return <div className="grid min-h-screen place-items-center text-muted-foreground">加载中...</div>
}

export function AuthError({ message, onRetry }: { message: string; onRetry: () => void }) {
  return (
    <div className="grid min-h-screen place-items-center bg-background px-4">
      <div className="w-full max-w-sm space-y-4 text-center">
        <div className="text-sm font-medium">无法连接后端服务</div>
        <div className="text-sm text-muted-foreground">{message}</div>
        <Button type="button" variant="outline" onClick={onRetry}>重新加载</Button>
      </div>
    </div>
  )
}

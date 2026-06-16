import { Link } from "react-router-dom"
import { Button } from "@/components/ui/button"
import { Home, MailQuestion } from "lucide-react"

export function NotFoundPage() {
  return (
    <div className="grid min-h-screen place-items-center bg-background px-4">
      <div className="w-full max-w-sm text-center">
        <div className="mb-6 flex justify-center">
          <div className="flex h-20 w-20 items-center justify-center rounded-full bg-muted">
            <MailQuestion className="h-10 w-10 text-muted-foreground" />
          </div>
        </div>
        <h1 className="mb-2 text-6xl font-bold tracking-tight">404</h1>
        <p className="mb-8 text-lg text-muted-foreground">页面不存在</p>
        <div className="flex justify-center gap-3">
          <Button asChild>
            <Link to="/">
              <Home className="mr-2 h-4 w-4" />返回首页
            </Link>
          </Button>
          <Button variant="outline" asChild>
            <Link to="/login">去登录</Link>
          </Button>
        </div>
      </div>
    </div>
  )
}

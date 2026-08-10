import React from "react"
import ReactDOM from "react-dom/client"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { Navigate, RouterProvider, createBrowserRouter } from "react-router-dom"
import { Toaster } from "@/components/ui/toaster"
import { LanguageDomSync } from "@/lib/language"
import { ProtectedLayout } from "@/components/protected-layout"
import { AdminOnly } from "@/components/admin-only"
import "./index.css"

const LoginPage = React.lazy(() => import("@/pages/login").then((module) => ({ default: module.LoginPage })))
const RegisterPage = React.lazy(() => import("@/pages/register").then((module) => ({ default: module.RegisterPage })))
const MailPage = React.lazy(() => import("@/pages/mail").then((module) => ({ default: module.MailPage })))
const AdminPage = React.lazy(() => import("@/pages/admin").then((module) => ({ default: module.AdminPage })))
const ProfilePage = React.lazy(() => import("@/pages/profile").then((module) => ({ default: module.ProfilePage })))
const NotFoundPage = React.lazy(() => import("@/pages/not-found").then((module) => ({ default: module.NotFoundPage })))

function lazyPage(element: React.ReactNode) {
  return <React.Suspense fallback={<div className="min-h-screen bg-background" />}>{element}</React.Suspense>
}

const queryClient = new QueryClient({ defaultOptions: { queries: { refetchOnWindowFocus: false, staleTime: 10_000 } } })
const router = createBrowserRouter([
  { path: "/login", element: lazyPage(<LoginPage />) },
  { path: "/register", element: lazyPage(<RegisterPage />) },
  { path: "/", element: <ProtectedLayout />, children: [
    { index: true, element: lazyPage(<MailPage />) },
    { path: "mail", element: <Navigate to="/" replace /> },
    { path: "mail/starred", element: <Navigate to="/" replace /> },
    { path: "profile", element: lazyPage(<ProfilePage />) },
    { path: "admin", element: <AdminOnly>{lazyPage(<AdminPage />)}</AdminOnly> },
  ] },
  { path: "*", element: lazyPage(<NotFoundPage />) },
])

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
      <Toaster />
      <LanguageDomSync />
    </QueryClientProvider>
  </React.StrictMode>,
)

import React from "react"
import ReactDOM from "react-dom/client"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { Navigate, RouterProvider, createBrowserRouter } from "react-router-dom"
import { Toaster } from "@/components/ui/toaster"
import { ProtectedLayout } from "@/components/protected-layout"
import { AdminOnly } from "@/components/admin-only"
import { LoginPage } from "@/pages/login"
import { RegisterPage } from "@/pages/register"
import { MailPage } from "@/pages/mail"
import { AdminPage } from "@/pages/admin"
import { ProfilePage } from "@/pages/profile"
import { NotFoundPage } from "@/pages/not-found"
import "./index.css"

const queryClient = new QueryClient({ defaultOptions: { queries: { refetchOnWindowFocus: false, staleTime: 10_000 } } })
const router = createBrowserRouter([
  { path: "/login", element: <LoginPage /> },
  { path: "/register", element: <RegisterPage /> },
  { path: "/", element: <ProtectedLayout />, children: [
    { index: true, element: <MailPage /> },
    { path: "mail", element: <Navigate to="/" replace /> },
    { path: "mail/starred", element: <Navigate to="/" replace /> },
    { path: "profile", element: <ProfilePage /> },
    { path: "admin", element: <AdminOnly><AdminPage /></AdminOnly> },
  ] },
  { path: "*", element: <NotFoundPage /> },
])

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
      <Toaster />
    </QueryClientProvider>
  </React.StrictMode>,
)

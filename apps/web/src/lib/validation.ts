export function validatePasswordConfirm(
  password: string,
  confirmPassword: string,
  message?: string,
): void {
  if (password !== confirmPassword) {
    throw new Error(message || "两次输入的密码不一致")
  }
}

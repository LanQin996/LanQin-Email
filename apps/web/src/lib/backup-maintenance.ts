// Stable opaque ID shared with the host handoff protocol; it is not authorization.
export async function backupMaintenanceRequestId(actorId: string, nonce: string) {
  const digest = await crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(`${actorId}:${nonce}`)
  )
  return Array.from(new Uint8Array(digest).slice(0, 16), (byte) =>
    byte.toString(16).padStart(2, "0")
  ).join("")
}

// Count Unicode code points, matching Go's utf8.RuneCountInString. Do not trim
// or normalize: recovery must use the exact password originally supplied.
export function validNewBackupPassword(value: string): boolean {
  const characters = Array.from(value).length
  return (
    characters >= 8 &&
    characters <= 128 &&
    !/^\p{White_Space}*$/u.test(value) &&
    !/[\p{Cc}\p{Cs}]/u.test(value)
  )
}

/** Expiry math shared by the cluster Certificates page and Settings → Authorities. */

export function daysLeft(notAfter: string): number {
  return Math.floor((new Date(notAfter).getTime() - Date.now()) / 86_400_000)
}

export function expiry(notAfter: string): string {
  const d = daysLeft(notAfter)
  if (d < 0) return 'EXPIRED'
  if (d === 0) return 'expires today'
  return `${d}d left`
}

/** Expiry math shared by the cluster Certificates page and Settings → Authorities.
 *  daysLeft comes from the shared kit (identical math); expiry stays app-local. */
export { daysLeft } from '@mnshahawy/daffa-console-ui'
import { daysLeft } from '@mnshahawy/daffa-console-ui'

export function expiry(notAfter: string): string {
  const d = daysLeft(notAfter)
  if (d < 0) return 'EXPIRED'
  if (d === 0) return 'expires today'
  return `${d}d left`
}

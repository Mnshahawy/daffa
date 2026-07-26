/**
 * The capability check every console shares. The `Cap` / `Ns` tables themselves are
 * code-generated per app (from its Go caps registry) and stay app-local; only the masking
 * function is common — and it is common precisely because it is the part that was once
 * wrong in a way that took a day to find.
 */
export interface CapValue {
  ns: string
  /** A single set bit as a NUMBER (1 << n). Zero is "no capability" and fails closed. */
  bit: number
}

export type CapSet = Record<string, number | string>

/**
 * BigInt on purpose. JavaScript's bitwise operators coerce to a SIGNED 32-BIT integer, so
 * `mask & (1 << 31)` goes negative and bits ≥ 32 vanish entirely — a capability registry
 * that grows past bit 31 would silently deny everyone, and the last person in the registry
 * would be told they could not do the thing they were just granted.
 *
 * Namespacing means no area is anywhere near bit 31 today. That is not a reason to use the
 * broken operator; it is a reason the bug would take even longer to find next time.
 */
export function hasCap(set: CapSet | undefined, cap: CapValue): boolean {
  if (!set || !cap?.bit) return false
  return (BigInt(set[cap.ns] ?? 0) & BigInt(cap.bit)) !== 0n
}

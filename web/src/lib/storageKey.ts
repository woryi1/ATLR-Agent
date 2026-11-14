// localStorage key namespace for this build.
//
// Two builds of this tree (upstream Octo and ATLR-Agent) can be installed side
// by side, and each keeps its own hub on its own port — a different origin. The
// prefix is the belt to that origin's braces: it keeps the keys legible in
// devtools and keeps them from colliding if a future build ever serves both
// from one origin. Every persisted UI preference goes through storageKey; the
// inline boot path in index.html cannot import this module, so it repeats the
// literal `atlr.startupAnim` and must be kept in sync by hand.
export const STORAGE_PREFIX = 'atlr.'

export function storageKey(name: string): string {
  return STORAGE_PREFIX + name
}

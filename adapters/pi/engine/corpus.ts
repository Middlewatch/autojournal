// Corpus layout: where one validated payload lands below the journal root.
//
// Grows into the full containment walk and atomic publication in the store
// slice; the layout derivation lives here so identity, publication, and the
// parse-boundary properties all share one definition.

import type { Payload } from "./contracts.ts";

const pad = (n: number, width: number): string => String(n).padStart(width, "0");

/**
 * The directory components an episode shards into, in order: world and
 * scope and lane only when they differ from their defaults, then the UTC
 * date of the event time. Validate is where implausible times are refused;
 * this function trusts it.
 */
export function layoutComponents(payload: Pick<Payload, "world" | "scope" | "lane" | "eventTimeMs">): string[] {
  const components: string[] = [];
  if (payload.world !== "main") components.push("worlds", payload.world);
  if (payload.scope !== "default") components.push("scopes", payload.scope);
  if (payload.lane !== "conversation") components.push("lanes", payload.lane);
  const t = new Date(payload.eventTimeMs);
  components.push(pad(t.getUTCFullYear(), 4), pad(t.getUTCMonth() + 1, 2), pad(t.getUTCDate(), 2));
  return components;
}

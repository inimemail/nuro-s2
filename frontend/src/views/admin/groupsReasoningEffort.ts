import type { GroupPlatform, ReasoningEffortMapping } from "@/types";

export const OPENAI_REASONING_EFFORT_VALUES = [
  "minimal", "low", "medium", "high", "xhigh", "max",
] as const;

export function normalizeReasoningEffortForPlatform(
  platform: GroupPlatform,
  value: string | null | undefined,
): string {
  const normalized = value?.trim().toLowerCase() ?? "";
  return platform === "openai" && (OPENAI_REASONING_EFFORT_VALUES as readonly string[]).includes(normalized)
    ? normalized
    : "";
}

export function reasoningEffortMappingsToAPI(
  mappings: ReasoningEffortMapping[],
): ReasoningEffortMapping[] {
  return mappings
    .map((item) => ({ from: item.from.trim().toLowerCase(), to: item.to.trim().toLowerCase() }))
    .filter((item) => item.from && item.to);
}

export function reasoningEffortMappingsToRows(
  mappings: ReasoningEffortMapping[] | null | undefined,
  platform: GroupPlatform,
): ReasoningEffortMapping[] {
  return reasoningEffortMappingsToAPI(mappings ?? []).filter(
    (item) => normalizeReasoningEffortForPlatform(platform, item.from) && normalizeReasoningEffortForPlatform(platform, item.to),
  );
}

export function validateReasoningEffortMappings(
  mappings: ReasoningEffortMapping[],
  platform: GroupPlatform,
): boolean {
  const seen = new Set<string>();
  return mappings.every((item) => {
    const from = normalizeReasoningEffortForPlatform(platform, item.from);
    const to = normalizeReasoningEffortForPlatform(platform, item.to);
    if (!from || !to || seen.has(from)) return false;
    seen.add(from);
    return true;
  });
}

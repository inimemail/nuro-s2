import { describe, expect, it } from "vitest";
import {
  normalizeReasoningEffortForPlatform,
  reasoningEffortMappingsToAPI,
  validateReasoningEffortMappings,
} from "../groupsReasoningEffort";

describe("group reasoning effort policy", () => {
  it("accepts only OpenAI supported values", () => {
    expect(normalizeReasoningEffortForPlatform("openai", " XHIGH ")).toBe("xhigh");
    expect(normalizeReasoningEffortForPlatform("composite", "high")).toBe("high");
    expect(normalizeReasoningEffortForPlatform("grok", "high")).toBe("");
  });

  it("rejects empty, unsupported and duplicate sources", () => {
    const rows = [{ from: "high", to: "medium" }, { from: "high", to: "low" }, { from: "", to: "max" }];
    expect(validateReasoningEffortMappings(rows, "openai")).toBe(false);
    expect(reasoningEffortMappingsToAPI(rows)).toEqual([{ from: "high", to: "medium" }, { from: "high", to: "low" }]);
  });

  it("preserves and validates model-scoped mappings", () => {
    const rows = [
      { from: "HIGH", to: "medium", match_type: "prefix" as const, model: " gpt-5. " },
      { from: "high", to: "low", match_type: "suffix" as const, model: "-pro" },
    ];
    expect(validateReasoningEffortMappings(rows, "composite")).toBe(true);
    expect(reasoningEffortMappingsToAPI(rows)).toEqual([
      { from: "high", to: "medium", match_type: "prefix", model: "gpt-5." },
      { from: "high", to: "low", match_type: "suffix", model: "-pro" },
    ]);
  });
});

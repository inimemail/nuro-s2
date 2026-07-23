import { describe, expect, it } from "vitest";
import {
  normalizeReasoningEffortForPlatform,
  reasoningEffortMappingsToAPI,
  validateReasoningEffortMappings,
} from "../groupsReasoningEffort";

describe("group reasoning effort policy", () => {
  it("accepts only OpenAI supported values", () => {
    expect(normalizeReasoningEffortForPlatform("openai", " XHIGH ")).toBe("xhigh");
    expect(normalizeReasoningEffortForPlatform("grok", "high")).toBe("");
  });

  it("rejects empty, unsupported and duplicate sources", () => {
    const rows = [{ from: "high", to: "medium" }, { from: "high", to: "low" }, { from: "", to: "max" }];
    expect(validateReasoningEffortMappings(rows, "openai")).toBe(false);
    expect(reasoningEffortMappingsToAPI(rows)).toEqual([{ from: "high", to: "medium" }, { from: "high", to: "low" }]);
  });
});

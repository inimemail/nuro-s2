import { describe, expect, it } from "vitest";
import { supportsImagePricingPlatform } from "../groupsImagePricing";

describe("groupsImagePricing", () => {
  it("supports Grok image pricing configuration", () => {
    expect(supportsImagePricingPlatform("grok")).toBe(true);
    expect(supportsImagePricingPlatform("antigravity")).toBe(true);
    expect(supportsImagePricingPlatform("gemini")).toBe(true);
    expect(supportsImagePricingPlatform("openai")).toBe(true);
    expect(supportsImagePricingPlatform("anthropic")).toBe(false);
  });
});

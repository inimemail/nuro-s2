import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";
import { supportsImagePricingPlatform } from "../groupsImagePricing";

const currentDir = dirname(fileURLToPath(import.meta.url));
const groupsViewSource = readFileSync(resolve(currentDir, "../GroupsView.vue"), "utf8");

describe("groupsImagePricing", () => {
  it("supports Grok image pricing configuration", () => {
    expect(supportsImagePricingPlatform("grok")).toBe(true);
    expect(supportsImagePricingPlatform("antigravity")).toBe(true);
    expect(supportsImagePricingPlatform("gemini")).toBe(true);
    expect(supportsImagePricingPlatform("openai")).toBe(true);
    expect(supportsImagePricingPlatform("anthropic")).toBe(false);
  });

  it("keeps the Grok video pricing JSON placeholder out of vue-i18n parsing", () => {
    expect(groupsViewSource).toContain(
      `const grokVideoModelPricesPlaceholder = '{"grok-imagine-video-1.5-preview":{"480p":0.14,"720p":0.24}}';`,
    );
    expect(groupsViewSource.match(/:placeholder="grokVideoModelPricesPlaceholder"/g)).toHaveLength(2);
    expect(groupsViewSource).not.toContain("grokPricing.videoModelPricesPlaceholder");
  });
});

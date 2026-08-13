export const imagePricingPlatforms = new Set([
  "antigravity",
  "gemini",
  "grok",
  "composite",
  "openai",
]);

export const supportsImagePricingPlatform = (platform: string): boolean =>
  imagePricingPlatforms.has(platform);

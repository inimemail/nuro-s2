import { mount } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";
import GroupReasoningEffortControl from "../GroupReasoningEffortControl.vue";

vi.mock("vue-i18n", () => {
  return {
    useI18n: () => ({ t: (key: string) => key }),
  };
});

const values = ["minimal", "low", "medium", "high", "xhigh", "max"];

describe("GroupReasoningEffortControl", () => {
  it("adds an empty mapping without mutating the input array", async () => {
    const mappings = [{ from: "high", to: "medium" }];
    const wrapper = mount(GroupReasoningEffortControl, {
      props: { idPrefix: "test", maxReasoningEffort: "high", maxReasoningEffortOverLimit: "downgrade", mappings, values },
    });

    await wrapper.get('[data-testid="test-add-reasoning-mapping"]').trigger("click");

    expect(mappings).toEqual([{ from: "high", to: "medium" }]);
    expect(wrapper.emitted("update:mappings")?.[0]).toEqual([
      [{ from: "high", to: "medium" }, { from: "", to: "" }],
    ]);
  });

  it("emits immutable field updates and removal", async () => {
    const mappings = [{ from: "high", to: "medium" }];
    const wrapper = mount(GroupReasoningEffortControl, {
      props: { idPrefix: "test", maxReasoningEffort: "", maxReasoningEffortOverLimit: "downgrade", mappings, values },
    });

    await wrapper.get('[data-testid="test-reasoning-from-0"]').setValue("xhigh");
    await wrapper.get("#test-max-reasoning-effort").setValue("medium");
    await wrapper.get('[data-testid="test-remove-reasoning-0"]').trigger("click");

    expect(mappings).toEqual([{ from: "high", to: "medium" }]);
    expect(wrapper.emitted("update:mappings")?.[0]).toEqual([
      [{ from: "xhigh", to: "medium" }],
    ]);
    expect(wrapper.emitted("update:mappings")?.[1]).toEqual([[]]);
    expect(wrapper.emitted("update:maxReasoningEffort")?.[0]).toEqual(["medium"]);
  });

  it("edits the over-limit action and model scope without mutating props", async () => {
    const mappings = [{ from: "high", to: "medium" }];
    const wrapper = mount(GroupReasoningEffortControl, {
      props: { idPrefix: "test", maxReasoningEffort: "high", maxReasoningEffortOverLimit: "downgrade", mappings, values },
    });

    await wrapper.get("#test-reasoning-over-limit").setValue("deny");
    await wrapper.get('[data-testid="test-reasoning-match-0"]').setValue("prefix");

    expect(mappings).toEqual([{ from: "high", to: "medium" }]);
    expect(wrapper.emitted("update:maxReasoningEffortOverLimit")?.[0]).toEqual(["deny"]);
    expect(wrapper.emitted("update:mappings")?.[0]).toEqual([
      [{ from: "high", to: "medium", match_type: "prefix" }],
    ]);
  });
});

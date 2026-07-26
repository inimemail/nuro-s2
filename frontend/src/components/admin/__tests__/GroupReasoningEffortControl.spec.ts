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
      props: { idPrefix: "test", maxReasoningEffort: "high", mappings, values },
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
      props: { idPrefix: "test", maxReasoningEffort: "", mappings, values },
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
});

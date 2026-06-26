import { describe, it, expect } from "vitest";
import {
  parseFingerprintSignalsToRows,
  serializeFingerprintRowsToJSON,
} from "../codexFingerprintSignals";

describe("codex fingerprint signals rows", () => {
  it("parses variant arrays into editable rows", () => {
    const rows = parseFingerprintSignalsToRows(
      '[{"type":"header_exact","match":["session-id","session_id"],"required":true}]',
    );
    expect(rows).toEqual([
      { type: "header_exact", match: "session-id / session_id", required: true },
    ]);
  });

  it("serializes slash-separated variants back to JSON", () => {
    const json = serializeFingerprintRowsToJSON([
      { type: "header_prefix", match: "x-codex-", required: true },
      { type: "body_path", match: " a / b ", required: false },
    ]);
    expect(JSON.parse(json)).toEqual([
      { type: "header_prefix", match: ["x-codex-"], required: true },
      { type: "body_path", match: ["a", "b"], required: false },
    ]);
  });

  it("handles empty and invalid input", () => {
    expect(parseFingerprintSignalsToRows("")).toEqual([]);
    expect(parseFingerprintSignalsToRows("nope")).toEqual([]);
    expect(serializeFingerprintRowsToJSON([])).toBe("[]");
  });
});

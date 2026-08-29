import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const apiKeys = readFileSync(
  new URL("../pages/APIKeys.tsx", import.meta.url),
  "utf8",
);
const accounts = readFileSync(
  new URL("../pages/Accounts.tsx", import.meta.url),
  "utf8",
);
const types = readFileSync(new URL("../types.ts", import.meta.url), "utf8");

test("Claude API key fallback models use the native provider aliases", () => {
  assert.match(
    apiKeys,
    /const DEFAULT_CLAUDE_MODEL_OPTIONS = \[\s*"claude-opus-4-5",\s*"claude-sonnet-4-5",\s*"claude-haiku-4-5",\s*\]/,
  );
});

test("Claude API key plan allowlist is isolated from Codex plans", () => {
  assert.match(apiKeys, /const CLAUDE_PLAN_FILTER_OPTIONS = \[/);
  assert.match(
    apiKeys,
    /if \(channel === "claude"\) return CLAUDE_PLAN_FILTER_OPTIONS;/,
  );
});

test("recycle-bin account projection preserves Claude provider identity", () => {
  assert.match(types, /export interface RecycleBinAccountRow[\s\S]*claude_api\?: boolean/);
  assert.match(
    accounts,
    /claude_api:\s*row\.claude_api/,
  );
});

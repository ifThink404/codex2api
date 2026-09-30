import assert from "node:assert/strict";
import test from "node:test";

import { buildWritableSettingsPayload } from "./settingsPayload.ts";

test("writable settings payload omits response cache generation regardless of value", () => {
  for (const generation of [7, 0, null, undefined]) {
    const settings = {
      site_name: "CodexProxy",
      response_cache_local_max_bytes: 64 * 1024 * 1024,
      response_cache_local_max_entry_bytes: 8 * 1024 * 1024,
      response_cache_reconstruct_max_bytes: 64 * 1024 * 1024,
      response_cache_config_generation: generation,
      future_setting: "preserved",
      codex_images_main_model: "gpt-5.6-sol",
      codex_images_default_main_model: "gpt-5.6-luna",
      codex_egress: { mode: "resin", resin_enabled: true },
    };

    const payload = buildWritableSettingsPayload(settings);

    assert.equal(
      Object.hasOwn(payload, "response_cache_config_generation"),
      false,
    );
    assert.deepEqual(payload, {
      site_name: "CodexProxy",
      response_cache_local_max_bytes: 64 * 1024 * 1024,
      response_cache_local_max_entry_bytes: 8 * 1024 * 1024,
      response_cache_reconstruct_max_bytes: 64 * 1024 * 1024,
      future_setting: "preserved",
      codex_images_main_model: "gpt-5.6-sol",
    });
    assert.equal(
      Object.hasOwn(settings, "response_cache_config_generation"),
      true,
      "the source settings object must not be mutated",
    );
    assert.equal(settings.response_cache_config_generation, generation);
  }
});

test("writable settings payload never writes the Basispoints views of the BPS plugin", () => {
  const payload = buildWritableSettingsPayload({
    site_name: "CodexProxy",
    codex_basispoints_enabled: false,
    codex_basispoints_models: "",
    codex_basispoints_cache_creation_as_input: false,
    codex_basispoints_403_auto_pause: true,
  });
  assert.deepEqual(payload, { site_name: "CodexProxy" });
});

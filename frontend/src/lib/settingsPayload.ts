const RESPONSE_CACHE_CONFIG_GENERATION = "response_cache_config_generation";
const CODEX_IMAGES_DEFAULT_MAIN_MODEL = "codex_images_default_main_model";
// 后端按当前 Resin 配置现算的只读摘要,回写没有意义。
const CODEX_EGRESS = "codex_egress";
// Upstream's Basispoints settings are views of the BPS transport plugin,
// edited on the Plugins page. A settings save never writes them back, or a
// stale form would undo a plugin change made meanwhile.
const CODEX_BASISPOINTS_PREFIX = "codex_basispoints_";

export function buildWritableSettingsPayload<
  T extends object,
>(settings: T): Omit<T, typeof RESPONSE_CACHE_CONFIG_GENERATION | typeof CODEX_IMAGES_DEFAULT_MAIN_MODEL | typeof CODEX_EGRESS> {
  const payload = { ...settings };
  Reflect.deleteProperty(payload, RESPONSE_CACHE_CONFIG_GENERATION);
  Reflect.deleteProperty(payload, CODEX_IMAGES_DEFAULT_MAIN_MODEL);
  Reflect.deleteProperty(payload, CODEX_EGRESS);
  for (const key of Object.keys(payload)) {
    if (key.startsWith(CODEX_BASISPOINTS_PREFIX)) Reflect.deleteProperty(payload, key);
  }
  return payload;
}

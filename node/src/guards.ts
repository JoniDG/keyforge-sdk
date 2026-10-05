// Runtime checks for the frames the SDK consumes. @jdg-keyforge/protocol only
// ships types, so each check covers the fields the SDK reads (and the ones the
// schema requires of them), not the full JSON Schema.

export type JsonObject = Record<string, unknown>;

export function isObject(value: unknown): value is JsonObject {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

export function isString(value: unknown): value is string {
  return typeof value === 'string';
}

export function isPeerInfo(value: unknown): value is { name: string; version: string } {
  return isObject(value) && isString(value.name) && isString(value.version);
}

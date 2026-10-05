import { mkdtemp, readFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import type { InputAction, InputEvent, PluginManifest } from '@jdg-keyforge/protocol';
import type { Invocation, Logger } from '@jdg-keyforge/sdk/plugin';
import { describe, expect, it } from 'vitest';
import { VERSION, handlers, next } from './counter.js';

const silent: Logger = {
  debug: () => undefined,
  info: () => undefined,
  warn: () => undefined,
  error: () => undefined,
};

function key(): InputEvent {
  return {
    device_id: 'VID_1_PID_2',
    kind: 'key',
    input_id: 'key_1',
    action: 'press',
    timestamp_ms: 1,
  };
}

function encoder(action: InputAction): InputEvent {
  return {
    device_id: 'VID_1_PID_2',
    kind: 'encoder',
    input_id: 'encoder_0',
    action,
    timestamp_ms: 1,
  };
}

function invocation(
  context: string,
  input: InputEvent | undefined,
  params: Record<string, string>,
): Invocation {
  return { context, action: { id: 'count', params }, ...(input ? { input } : {}) };
}

function count(): (inv: Invocation) => Promise<void> {
  const handler = handlers(silent).count;
  if (!handler) {
    throw new Error('no count handler');
  }
  return async (inv) => handler(inv, new AbortController().signal);
}

async function tempDir(): Promise<string> {
  return mkdtemp(join(tmpdir(), 'counter-'));
}

describe('manifest', () => {
  it('matches the plugin', async () => {
    const manifest = JSON.parse(
      await readFile(new URL('../manifest.json', import.meta.url), 'utf8'),
    ) as PluginManifest;

    expect(manifest.version).toBe(VERSION);
    expect(manifest.protocol_version).toBe('1');
    for (const os of ['darwin', 'linux', 'windows'] as const) {
      expect(manifest.entrypoint[os]).toEqual({ path: 'node', args: ['dist/main.js'] });
    }
    expect(manifest.actions.map((a) => a.id).sort()).toEqual(Object.keys(handlers(silent)).sort());
  });
});

describe('next', () => {
  it.each([
    ['key press adds one', 2, key(), 3],
    ['run from the app adds one', 2, undefined, 3],
    ['turn right adds one', 2, encoder('rotate_cw'), 3],
    ['turn left takes one away', 2, encoder('rotate_ccw'), 1],
    ['never below zero', 0, encoder('rotate_ccw'), 0],
    ['press resets', 7, encoder('click'), 0],
  ])('%s', (_, current, input, want) => {
    expect(next(current, input)).toBe(want);
  });
});

describe('count', () => {
  it('keeps one counter per binding', async () => {
    const dir = await tempDir();
    const first = join(dir, 'first.txt');
    const second = join(dir, 'second.txt');
    const run = count();

    await run(invocation('binding-1', key(), { file: first }));
    await run(invocation('binding-1', key(), { file: first }));
    await run(invocation('binding-2', key(), { file: second }));

    expect(await readFile(first, 'utf8')).toBe('2');
    expect(await readFile(second, 'utf8')).toBe('1');
  });

  it('shares the counter between bindings with the same label', async () => {
    const file = join(await tempDir(), 'deaths.txt');
    const params = { label: 'Deaths', file };
    const run = count();

    // An encoder's three triggers are three bindings with their own contexts.
    await run(invocation('rotate-cw', encoder('rotate_cw'), params));
    await run(invocation('rotate-cw', encoder('rotate_cw'), params));
    await run(invocation('rotate-ccw', encoder('rotate_ccw'), params));
    expect(await readFile(file, 'utf8')).toBe('Deaths: 1');

    await run(invocation('click', encoder('click'), params));
    expect(await readFile(file, 'utf8')).toBe('Deaths: 0');
  });

  it('counts without writing when there is no file', async () => {
    const run = count();

    await expect(run(invocation('binding-1', undefined, {}))).resolves.toBeUndefined();
  });

  it.each([
    ['a relative path', async () => 'deaths.txt', 'must be an absolute path'],
    [
      'a missing folder',
      async () => join(await tempDir(), 'nope', 'deaths.txt'),
      'counter.writeCount',
    ],
  ])('rejects %s', async (_, file, want) => {
    await expect(count()(invocation('binding-1', key(), { file: await file() }))).rejects.toThrow(
      want,
    );
  });
});

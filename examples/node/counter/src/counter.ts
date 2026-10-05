// The counter plugin: a counter for streams (deaths, wins, coffees). A key
// adds one; on an encoder, turning right adds one, turning left takes one away
// and pressing resets it. The count can be written to a text file that OBS
// shows as a text source. Same plugin as examples/go/counter.
import { writeFile } from 'node:fs/promises';
import { isAbsolute } from 'node:path';
import type { InputEvent } from '@jdg-keyforge/protocol';
import type { Handlers, Invocation, Logger } from '@jdg-keyforge/sdk/plugin';

// Must match "version" in manifest.json.
export const VERSION = '0.1.0';

export function handlers(logger: Logger): Handlers {
  // Handlers run one at a time, so the map needs no coordination.
  const counts = new Map<string, number>();
  return {
    count: async (inv) => {
      const label = param(inv, 'label');
      // Each binding has its own context, and an encoder needs one binding
      // per trigger. Bindings that share a label share the count, which is
      // how an encoder's three bindings drive one counter.
      const key = label === '' ? inv.context : `label:${label}`;
      const count = next(counts.get(key) ?? 0, inv.input);
      counts.set(key, count);

      logger.info('count', { label, count });
      const file = param(inv, 'file');
      if (file !== '') {
        await writeCount(file, label, count);
      }
    },
  };
}

// param returns a string param of the binding, or "" when it is not set.
function param(inv: Invocation, name: string): string {
  const value = inv.action.params[name];
  return typeof value === 'string' ? value : '';
}

// next returns the count after input. A missing input means the action was
// run from the KeyForge app rather than by hardware.
export function next(count: number, input?: InputEvent): number {
  switch (input?.action) {
    case 'rotate_ccw':
      return Math.max(count - 1, 0);
    case 'click':
      return 0;
    default:
      return count + 1;
  }
}

async function writeCount(file: string, label: string, count: number): Promise<void> {
  // The daemon starts the plugin inside its install folder, so a relative
  // path would land there, and installing a new version replaces it.
  if (!isAbsolute(file)) {
    throw new Error(`counter.writeCount: param file must be an absolute path, got "${file}"`);
  }
  const text = label === '' ? String(count) : `${label}: ${count}`;
  try {
    await writeFile(file, text, { mode: 0o600 });
  } catch (err) {
    throw new Error(`counter.writeCount: ${err instanceof Error ? err.message : String(err)}`, {
      cause: err,
    });
  }
}

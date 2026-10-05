/**
 * Runs a KeyForge plugin: the author writes one handler per action and the
 * module takes care of the rest.
 *
 * run reads the launch info the daemon passes in the KEYFORGE_PLUGIN_INFO
 * environment variable, connects, performs the hello handshake, dispatches
 * each action_invoked event to the handler registered for its action id and
 * resolves when the daemon closes the connection, which is how the daemon
 * stops plugins.
 *
 * Handlers run one at a time, in the order the daemon fired them, so state
 * kept per Invocation.context (e.g. a toggle bound to two keys) needs no
 * coordination even when handlers are async. A handler that needs to do slow
 * work should start it without awaiting it: up to 128 invocations wait behind
 * a running handler, and any that arrive past that are dropped and logged. A
 * handler that throws or rejects is logged and skipped; the plugin keeps
 * running.
 *
 * @module
 */
import type { EventActionInvoked, PluginLaunchInfo } from '@jdg-keyforge/protocol';
import { ClosedError, InvalidFrameError, dial, type Conn } from './client.js';
import { isObject, isString } from './guards.js';

/** The environment variable the daemon passes the launch info in. */
export const LAUNCH_INFO_ENV = 'KEYFORGE_PLUGIN_INFO';

// How many invocations can wait behind a running handler before new ones are
// dropped. Large enough to absorb a fast encoder turn.
const QUEUE_SIZE = 128;

const INPUT_KINDS: readonly string[] = ['key', 'encoder'];
const INPUT_ACTIONS: readonly string[] = ['press', 'release', 'rotate_cw', 'rotate_ccw', 'click'];

/**
 * One firing of a plugin action: context identifies the binding that fired,
 * action.params holds its settings and input the hardware event (absent when
 * the action was run from the KeyForge app).
 */
export type Invocation = EventActionInvoked['data'];

/**
 * Runs one plugin action. The signal is aborted when the plugin is stopping,
 * so long work can give up early. A thrown error or rejection is logged.
 */
export type Handler = (inv: Invocation, signal: AbortSignal) => void | Promise<void>;

/**
 * Maps an action id, as declared in the manifest (e.g. "play_pause", without
 * the "plugin.<id>." prefix), to its handler.
 */
export type Handlers = Record<string, Handler>;

/** Structured attributes attached to a log line. */
export type LogAttrs = Record<string, unknown>;

/** Receives what the plugin logs. */
export interface Logger {
  debug(msg: string, attrs?: LogAttrs): void;
  info(msg: string, attrs?: LogAttrs): void;
  warn(msg: string, attrs?: LogAttrs): void;
  error(msg: string, attrs?: LogAttrs): void;
}

/** Options for run. */
export interface RunOptions {
  /** The plugin version sent in hello. Required; keep it in sync with the manifest. */
  version: string;
  /** One handler per action the manifest declares. */
  handlers: Handlers;
  /** Receives handler errors and ignored frames. Defaults to textLogger(). */
  logger?: Logger;
  /** Aborting it closes the connection and stops the plugin. */
  signal?: AbortSignal;
}

/**
 * Connects the plugin to the daemon and dispatches actions to
 * options.handlers until the daemon closes the connection, which resolves, or
 * options.signal is aborted, which rejects with its reason. Either way,
 * invocations still queued at that point are dropped. It rejects right away if
 * KEYFORGE_PLUGIN_INFO is missing or invalid, which means the process wasn't
 * launched by the daemon, or if the daemon rejects the hello.
 */
export function run(options: RunOptions): Promise<void> {
  return runWithEnv(options, process.env);
}

/** @internal run with the environment injected, for tests. */
export async function runWithEnv(
  options: RunOptions,
  env: Record<string, string | undefined>,
): Promise<void> {
  const info = launchInfo(env);
  const logger = withAttrs(options.logger ?? textLogger(), { plugin: info.plugin_id });

  const dialOptions = options.signal ? { signal: options.signal } : {};
  let conn: Conn;
  try {
    ({ conn } = await dial(
      info.ws_url,
      {
        protocol_version: info.protocol_version,
        client: { name: info.plugin_id, version: options.version },
      },
      dialOptions,
    ));
  } catch (err) {
    if (options.signal?.aborted) {
      throw options.signal.reason;
    }
    throw new Error(`plugin.run: ${errorMessage(err)}`, { cause: err });
  }

  try {
    await serve(conn, options.handlers, logger, options.signal);
  } finally {
    await conn.close();
  }
}

function launchInfo(env: Record<string, string | undefined>): PluginLaunchInfo {
  const raw = env[LAUNCH_INFO_ENV];
  if (raw === undefined || raw === '') {
    throw new Error(
      `plugin.run: ${LAUNCH_INFO_ENV} is not set; plugins must be launched by the KeyForge daemon`,
    );
  }
  // The value is never included in errors: ws_url carries the auth token.
  let info: unknown;
  try {
    info = JSON.parse(raw);
  } catch {
    throw new Error(`plugin.run: invalid ${LAUNCH_INFO_ENV}: not JSON`);
  }
  if (
    !isObject(info) ||
    !isString(info.plugin_id) ||
    !isString(info.ws_url) ||
    info.protocol_version !== '1'
  ) {
    throw new Error(
      `plugin.run: invalid ${LAUNCH_INFO_ENV}: want plugin_id, ws_url and protocol_version "1"`,
    );
  }
  return { plugin_id: info.plugin_id, ws_url: info.ws_url, protocol_version: '1' };
}

// serve reads events in one loop and runs handlers in another, so a closed
// connection is noticed (and the running handler's signal aborted) even while
// a handler is busy.
async function serve(
  conn: Conn,
  handlers: Handlers,
  logger: Logger,
  parent?: AbortSignal,
): Promise<void> {
  const stopping = new AbortController();
  const queue: Invocation[] = [];
  let wake: (() => void) | undefined;
  let done = false;

  // On abort the running handler is told right away and the connection is
  // closed normally, which also ends the reader.
  const onAbort = (): void => {
    stopping.abort();
    void conn.close();
  };
  if (parent?.aborted) {
    onAbort();
  }
  parent?.addEventListener('abort', onAbort, { once: true });

  const reader = read(conn, queue, logger, () => wake?.()).then((err) => {
    done = true;
    stopping.abort();
    wake?.();
    return err;
  });

  try {
    for (;;) {
      const inv = queue.shift();
      if (inv) {
        if (!stopping.signal.aborted) {
          await dispatch(handlers, inv, logger, stopping.signal);
        }
        continue;
      }
      if (done) {
        break;
      }
      await new Promise<void>((resolve) => {
        wake = resolve;
      });
      wake = undefined;
    }
  } finally {
    parent?.removeEventListener('abort', onAbort);
  }

  const readErr = await reader;
  if (parent?.aborted) {
    throw parent.reason;
  }
  if (readErr instanceof ClosedError) {
    return;
  }
  throw new Error(`plugin.run: ${errorMessage(readErr)}`, { cause: readErr });
}

// read queues action_invoked events until the connection is gone, and
// resolves to the error that ended it. It never waits on a full queue (it
// drops the invocation instead), so a closed connection is always noticed.
async function read(
  conn: Conn,
  queue: Invocation[],
  logger: Logger,
  notify: () => void,
): Promise<unknown> {
  for (;;) {
    let ev;
    try {
      ev = await conn.readEvent();
    } catch (err) {
      if (err instanceof InvalidFrameError) {
        logger.warn('ignoring invalid frame', { error: err });
        continue;
      }
      return err;
    }
    if (ev.name !== 'action_invoked') {
      logger.debug('ignoring event', { event: ev.name });
      continue;
    }
    if (!isInvocation(ev.data)) {
      logger.warn('ignoring invalid action_invoked');
      continue;
    }
    if (queue.length >= QUEUE_SIZE) {
      logger.warn('dropping action_invoked: handlers are not keeping up', {
        action: ev.data.action.id,
        context: ev.data.context,
      });
      continue;
    }
    queue.push(ev.data);
    notify();
  }
}

function isInvocation(data: unknown): data is Invocation {
  if (!isObject(data) || !isString(data.context)) {
    return false;
  }
  const { action, input } = data;
  if (!isObject(action) || !isString(action.id) || !isObject(action.params)) {
    return false;
  }
  return (
    input === undefined ||
    (isObject(input) &&
      isString(input.device_id) &&
      isString(input.input_id) &&
      isString(input.kind) &&
      INPUT_KINDS.includes(input.kind) &&
      isString(input.action) &&
      INPUT_ACTIONS.includes(input.action) &&
      typeof input.timestamp_ms === 'number')
  );
}

async function dispatch(
  handlers: Handlers,
  inv: Invocation,
  logger: Logger,
  signal: AbortSignal,
): Promise<void> {
  const handler = Object.hasOwn(handlers, inv.action.id) ? handlers[inv.action.id] : undefined;
  if (!handler) {
    logger.warn('no handler for action', { action: inv.action.id });
    return;
  }
  try {
    await handler(inv, signal);
  } catch (err) {
    logger.error('action failed', { action: inv.action.id, context: inv.context, error: err });
  }
}

/**
 * Returns a Logger that writes logfmt lines (level=INFO msg="..." key=value)
 * to stderr, like the Go SDK's default. Debug lines are skipped unless debug
 * is true. stdout is left to the plugin.
 */
export function textLogger(options: { debug?: boolean } = {}): Logger {
  const write =
    (level: string) =>
    (msg: string, attrs: LogAttrs = {}): void => {
      const fields = Object.entries({ level, msg, ...attrs }).map(
        ([key, value]) => `${key}=${formatValue(value)}`,
      );
      process.stderr.write(`${fields.join(' ')}\n`);
    };
  return {
    debug: options.debug ? write('DEBUG') : () => undefined,
    info: write('INFO'),
    warn: write('WARN'),
    error: write('ERROR'),
  };
}

function withAttrs(logger: Logger, attrs: LogAttrs): Logger {
  return {
    debug: (msg, more) => logger.debug(msg, { ...attrs, ...more }),
    info: (msg, more) => logger.info(msg, { ...attrs, ...more }),
    warn: (msg, more) => logger.warn(msg, { ...attrs, ...more }),
    error: (msg, more) => logger.error(msg, { ...attrs, ...more }),
  };
}

function formatValue(value: unknown): string {
  const text = value instanceof Error ? value.message : isString(value) ? value : stringify(value);
  return /^[^\s"=]+$/.test(text) ? text : JSON.stringify(text);
}

function stringify(value: unknown): string {
  try {
    return JSON.stringify(value) ?? String(value);
  } catch {
    // A bigint or a circular object: a log line must never throw.
    return String(value);
  }
}

function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

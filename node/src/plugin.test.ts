import { describe, expect, it, vi } from 'vitest';
import {
  LAUNCH_INFO_ENV,
  run,
  runWithEnv,
  textLogger,
  type Handlers,
  type Invocation,
  type LogAttrs,
  type Logger,
  type RunOptions,
} from './plugin.js';
import { TOKEN, fakeDaemon, type Peer } from './testing.js';

const PLUGIN_ID = 'dev.jonidg.test';

interface LogRecord {
  level: string;
  msg: string;
  attrs: LogAttrs;
}

function recordingLogger(): { logger: Logger; records: LogRecord[] } {
  const records: LogRecord[] = [];
  const log =
    (level: string) =>
    (msg: string, attrs: LogAttrs = {}) => {
      records.push({ level, msg, attrs });
    };
  return {
    records,
    logger: { debug: log('debug'), info: log('info'), warn: log('warn'), error: log('error') },
  };
}

function env(url: string): { [LAUNCH_INFO_ENV]: string } {
  return {
    [LAUNCH_INFO_ENV]: JSON.stringify({ plugin_id: PLUGIN_ID, ws_url: url, protocol_version: '1' }),
  };
}

function invoked(context: string, action: string, input?: object): string {
  return JSON.stringify({
    type: 'event',
    name: 'action_invoked',
    data: { context, action: { id: action, params: {} }, ...(input ? { input } : {}) },
  });
}

// counter's reached resolves once fire has been called count times.
function counter(count: number): { fire: () => void; reached: Promise<void> } {
  let fire: () => void = () => undefined;
  const reached = new Promise<void>((resolve) => {
    let calls = 0;
    fire = () => {
      calls++;
      if (calls === count) {
        resolve();
      }
    };
  });
  return { fire, reached };
}

function options(handlers: Handlers, logger: Logger, extra: Partial<RunOptions> = {}): RunOptions {
  return { version: '1.0.0', handlers, logger, ...extra };
}

describe('run', () => {
  it('dispatches in order until the daemon closes', async () => {
    const got: string[] = [];
    const all = counter(3);
    const record = (inv: Invocation) => {
      got.push(`${inv.action.id}/${inv.context}/${inv.input?.input_id ?? 'none'}`);
      all.fire();
    };
    let helloParams: unknown;
    const url = await fakeDaemon(async (peer) => {
      helloParams = (await peer.acceptHello()).params;
      peer.send(
        invoked('ctx-1', 'toggle', {
          device_id: 'VID_1234_PID_5678',
          input_id: 'key_0x04',
          kind: 'key',
          action: 'press',
          timestamp_ms: 1,
        }),
      );
      peer.send(invoked('ctx-2', 'toggle'));
      peer.send(invoked('ctx-1', 'other'));
      // Invocations still queued at the close are dropped, so wait for them.
      await all.reached;
      peer.ws.close(1000);
    });
    const { logger } = recordingLogger();

    await runWithEnv(options({ toggle: record, other: record }, logger), env(url));

    expect(got).toEqual(['toggle/ctx-1/key_0x04', 'toggle/ctx-2/none', 'other/ctx-1/none']);
    expect(helloParams).toEqual({
      protocol_version: '1',
      client: { name: PLUGIN_ID, version: '1.0.0' },
    });
  });

  it('runs async handlers one at a time', async () => {
    const events: string[] = [];
    const all = counter(2);
    const slow = async (inv: Invocation) => {
      events.push(`start ${inv.context}`);
      await new Promise((resolve) => setTimeout(resolve, 20));
      events.push(`end ${inv.context}`);
      all.fire();
    };
    const url = await fakeDaemon(async (peer) => {
      await peer.acceptHello();
      peer.send(invoked('a', 'slow'));
      peer.send(invoked('b', 'slow'));
      await all.reached;
      peer.ws.close(1000);
    });

    await runWithEnv(options({ slow }, recordingLogger().logger), env(url));

    expect(events).toEqual(['start a', 'end a', 'start b', 'end b']);
  });

  it('logs failing handlers, unknown actions and ignored frames and keeps running', async () => {
    const done = counter(1);
    const url = await fakeDaemon(async (peer) => {
      await peer.acceptHello();
      peer.send(invoked('c1', 'fails'));
      peer.send(invoked('c2', 'throws'));
      peer.send(invoked('c3', 'missing'));
      peer.send(invoked('c4', 'toString'));
      peer.send('{');
      peer.send(JSON.stringify({ type: 'event', name: 'input', data: {} }));
      peer.send(JSON.stringify({ type: 'event', name: 'action_invoked', data: { context: 1 } }));
      peer.send(
        JSON.stringify({
          type: 'event',
          name: 'action_invoked',
          data: { context: 'c', action: { id: 'ok' } },
        }),
      );
      peer.send(invoked('c5', 'ok', { kind: 'joystick' }));
      peer.send(invoked('c6', 'ok'));
      await done.reached;
      peer.ws.close(1000);
    });
    const { logger, records } = recordingLogger();

    await runWithEnv(
      options(
        {
          fails: () => Promise.reject(new Error('boom')),
          throws: () => {
            throw new Error('bang');
          },
          ok: () => done.fire(),
        },
        logger,
      ),
      env(url),
    );

    const summary = records.map((r) => `${r.level} ${r.msg} ${String(r.attrs.action ?? '')}`);
    expect(summary).toEqual([
      'error action failed fails',
      'error action failed throws',
      'warn no handler for action missing',
      'warn no handler for action toString',
      'warn ignoring invalid frame ',
      'debug ignoring event ',
      'warn ignoring invalid action_invoked ',
      'warn ignoring invalid action_invoked ',
      'warn ignoring invalid action_invoked ',
    ]);
    expect(records.every((r) => r.attrs.plugin === PLUGIN_ID)).toBe(true);
    expect(records[0]?.attrs).toMatchObject({ context: 'c1', error: new Error('boom') });
  });

  it('drops invocations past the queue while a handler runs', async () => {
    let release: () => void = () => undefined;
    const blocked = new Promise<void>((resolve) => {
      release = resolve;
    });
    const seen: string[] = [];
    const last = counter(1);
    const blocking = counter(1);
    const url = await fakeDaemon(async (peer) => {
      await peer.acceptHello();
      peer.send(invoked('first', 'block'));
      // Only once the handler holds the loop can the queue fill up.
      await blocking.reached;
      for (let i = 0; i < 130; i++) {
        peer.send(invoked(`q${i}`, 'count'));
      }
      // A frame the reader handles after all of the above, so the queue is
      // full by the time it is answered.
      peer.send(JSON.stringify({ type: 'event', name: 'input', data: {} }));
    });
    const { records } = recordingLogger();
    const logger: Logger = {
      ...recordingLogger().logger,
      debug: () => release(),
      warn: (msg, attrs) => records.push({ level: 'warn', msg, attrs: attrs ?? {} }),
    };
    const controller = new AbortController();

    const running = runWithEnv(
      options(
        {
          block: () => {
            blocking.fire();
            return blocked;
          },
          count: (inv) => {
            seen.push(inv.context);
            if (seen.length === 128) {
              last.fire();
            }
          },
        },
        logger,
        { signal: controller.signal },
      ),
      env(url),
    );
    await last.reached;
    controller.abort(new Error('done'));
    await expect(running).rejects.toThrow('done');

    expect(seen).toHaveLength(128);
    expect(seen.at(-1)).toBe('q127');
    expect(records.map((r) => r.attrs.context)).toEqual(['q128', 'q129']);
  });

  it('drops queued invocations when the daemon closes', async () => {
    const seen: string[] = [];
    const url = await fakeDaemon(async (peer) => {
      await peer.acceptHello();
      peer.send(invoked('first', 'block'));
      peer.send(invoked('queued', 'block'));
      peer.ws.close(1000);
    });

    await runWithEnv(
      options(
        {
          // Holds the loop until the client side has seen the close.
          block: (inv, signal) => {
            seen.push(inv.context);
            return new Promise((resolve) => signal.addEventListener('abort', () => resolve()));
          },
        },
        recordingLogger().logger,
      ),
      env(url),
    );

    expect(seen).toEqual(['first']);
  });

  it('closes normally and rejects with the reason when the signal is aborted', async () => {
    let peer: Peer | undefined;
    const started = counter(1);
    const url = await fakeDaemon(async (p) => {
      peer = p;
      await p.acceptHello();
      p.send(invoked('c1', 'wait'));
    });
    const controller = new AbortController();
    let handlerSignal: AbortSignal | undefined;

    const running = runWithEnv(
      options(
        {
          wait: (_, signal) => {
            handlerSignal = signal;
            started.fire();
            return new Promise((resolve) => signal.addEventListener('abort', () => resolve()));
          },
        },
        recordingLogger().logger,
        { signal: controller.signal },
      ),
      env(url),
    );
    await started.reached;
    controller.abort(new Error('shutting down'));

    await expect(running).rejects.toThrow('shutting down');
    expect(handlerSignal?.aborted).toBe(true);
    expect(await peer?.closed).toBe(1000);
  });

  it('rejects when the connection drops', async () => {
    const url = await fakeDaemon(async (peer) => {
      await peer.acceptHello();
      peer.ws.terminate();
    });

    // Without a logger the default one is used; nothing is logged here.
    await expect(runWithEnv({ version: '1.0.0', handlers: {} }, env(url))).rejects.toThrow(
      /^plugin\.run: client: connection closed abnormally/,
    );
  });

  it('rejects when the daemon rejects hello', async () => {
    const url = await fakeDaemon(async (peer) => {
      await peer.answerHello((id) =>
        JSON.stringify({
          type: 'response',
          id,
          ok: false,
          error: { code: 'FORBIDDEN', message: 'unknown plugin' },
        }),
      );
    });

    await expect(runWithEnv(options({}, recordingLogger().logger), env(url))).rejects.toThrow(
      'plugin.run: client: daemon error: FORBIDDEN: unknown plugin',
    );
  });

  it('rejects with the abort reason when aborted before connecting', async () => {
    const url = await fakeDaemon(() => undefined);

    await expect(
      runWithEnv(
        options({}, recordingLogger().logger, { signal: AbortSignal.abort(new Error('early')) }),
        env(url),
      ),
    ).rejects.toThrow('early');
  });

  it.each([
    ['missing', {}, /is not set/],
    ['empty', { [LAUNCH_INFO_ENV]: '' }, /is not set/],
    ['not JSON', { [LAUNCH_INFO_ENV]: `{"ws_url":"ws://x?token=${TOKEN}"` }, /not JSON/],
    [
      'incomplete',
      { [LAUNCH_INFO_ENV]: JSON.stringify({ ws_url: `ws://x?token=${TOKEN}` }) },
      /want plugin_id, ws_url/,
    ],
  ])('rejects a %s launch info without leaking it', async (_, environment, want) => {
    const err = await runWithEnv(options({}, recordingLogger().logger), environment).catch(
      (e: unknown) => e,
    );

    expect((err as Error).message).toMatch(want);
    expect((err as Error).message).not.toContain(TOKEN);
  });
});

describe('run with the process environment', () => {
  it('rejects when the process was not launched by the daemon', async () => {
    vi.stubEnv(LAUNCH_INFO_ENV, undefined);
    try {
      await expect(run({ version: '1.0.0', handlers: {} })).rejects.toThrow(/is not set/);
    } finally {
      vi.unstubAllEnvs();
    }
  });
});

describe('textLogger', () => {
  it('writes logfmt lines to stderr and skips debug by default', () => {
    const write = vi.spyOn(process.stderr, 'write').mockImplementation(() => true);
    try {
      const logger = textLogger();
      logger.debug('hidden');
      logger.info('count', { label: 'Deaths', count: 3 });
      logger.warn('two words', { error: new Error('went wrong'), list: ['a'] });
      logger.error('none', { value: undefined });
      textLogger({ debug: true }).debug('shown');

      expect(write.mock.calls.map(([line]) => line)).toEqual([
        'level=INFO msg=count label=Deaths count=3\n',
        'level=WARN msg="two words" error="went wrong" list="[\\"a\\"]"\n',
        'level=ERROR msg=none value=undefined\n',
        'level=DEBUG msg=shown\n',
      ]);
    } finally {
      write.mockRestore();
    }
  });
});

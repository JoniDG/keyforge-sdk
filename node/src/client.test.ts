import { describe, expect, it } from 'vitest';
import {
  ClosedError,
  Conn,
  InvalidFrameError,
  READ_LIMIT,
  ServerError,
  dial,
  type HelloParams,
} from './client.js';
import { TOKEN, fakeDaemon, type Peer } from './testing.js';

const hello: HelloParams = {
  protocol_version: '1',
  client: { name: 'dev.jonidg.test', version: '1.0.0' },
};

function event(name: string, data: object = {}): string {
  return JSON.stringify({ type: 'event', name, data });
}

// connect dials a fake daemon that accepts the hello and then runs handle.
async function connect(handle: (peer: Peer) => void | Promise<void> = () => undefined) {
  let peer: Peer | undefined;
  const url = await fakeDaemon(async (p) => {
    peer = p;
    await p.acceptHello();
    await handle(p);
  });
  const { conn } = await dial(url, hello);
  if (!peer) {
    throw new Error('connect: no peer');
  }
  return { conn, peer };
}

describe('dial', () => {
  it('sends hello and resolves with the daemon result', async () => {
    let request: unknown;
    const url = await fakeDaemon(async (peer) => {
      request = await peer.acceptHello();
    });

    const { conn, result } = await dial(url, hello);

    expect(result).toEqual({
      protocol_version: '1',
      server: { name: 'keyforged', version: '0.1.0' },
    });
    expect(request).toMatchObject({ type: 'request', method: 'hello', params: hello });
    await conn.close();
  });

  it('rejects with a ServerError when the daemon rejects hello', async () => {
    const url = await fakeDaemon(async (peer) => {
      await peer.answerHello((id) =>
        JSON.stringify({
          type: 'response',
          id,
          ok: false,
          error: { code: 'FORBIDDEN', message: 'wrong plugin', details: { plugin_id: 'x' } },
        }),
      );
    });

    const err = await dial(url, hello).catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ServerError);
    expect((err as ServerError).detail).toEqual({
      code: 'FORBIDDEN',
      message: 'wrong plugin',
      details: { plugin_id: 'x' },
    });
  });

  it.each([
    ['not JSON', () => '{'],
    ['not an envelope', () => '[]'],
    ['an event instead of the response', () => event('input')],
    [
      'a response to another request',
      () => JSON.stringify({ type: 'response', id: 'other', ok: true, data: {} }),
    ],
    [
      'an error without a code',
      (id: string) => JSON.stringify({ type: 'response', id, ok: false, error: { message: 'x' } }),
    ],
    [
      'a result without server info',
      (id: string) =>
        JSON.stringify({ type: 'response', id, ok: true, data: { protocol_version: '1' } }),
    ],
    [
      'another protocol version',
      (id: string) =>
        JSON.stringify({
          type: 'response',
          id,
          ok: true,
          data: { server: { name: 'keyforged', version: '9.0.0' }, protocol_version: '2' },
        }),
    ],
  ])('rejects with an InvalidFrameError for %s', async (_, answer) => {
    const url = await fakeDaemon(async (peer) => {
      await peer.answerHello(answer);
    });

    await expect(dial(url, hello)).rejects.toBeInstanceOf(InvalidFrameError);
  });

  it('rejects with a ClosedError when the daemon closes before answering', async () => {
    const url = await fakeDaemon(async (peer) => {
      await peer.next();
      peer.ws.close(1000);
    });

    await expect(dial(url, hello)).rejects.toBeInstanceOf(ClosedError);
  });

  it('rejects invalid hello params before connecting', async () => {
    const bad = { protocol_version: '2', client: { name: 'x' } } as unknown as HelloParams;

    await expect(dial('ws://127.0.0.1:1/ws', bad)).rejects.toThrow(/invalid hello params/);
  });

  it.each([
    ['an unreachable daemon', `ws://127.0.0.1:1/ws?token=${TOKEN}`],
    ['an invalid URL', `not a url?token=${TOKEN}`],
    ['a URL without a token', 'ws://127.0.0.1:1/ws'],
    ['an empty URL', ''],
  ])('never leaks the token when dialing %s fails', async (_, url) => {
    const err = await dial(url, hello).catch((e: unknown) => e);

    expect(err).toBeInstanceOf(Error);
    expect((err as Error).message).toMatch(/^client\.dial: /);
    expect((err as Error).message).not.toContain(TOKEN);
  });

  it('rejects with the abort reason while waiting for the hello response', async () => {
    let received: () => void = () => undefined;
    const helloReceived = new Promise<void>((resolve) => {
      received = resolve;
    });
    let peer: Peer | undefined;
    const url = await fakeDaemon(async (p) => {
      peer = p;
      await p.next();
      received();
    });
    const controller = new AbortController();

    const pending = dial(url, hello, { signal: controller.signal });
    await helloReceived;
    controller.abort(new Error('give up'));

    await expect(pending).rejects.toThrow('give up');
    expect(await peer?.closed).not.toBe(1006);
  });

  it('rejects with the abort reason while opening the connection', async () => {
    const controller = new AbortController();
    const pending = dial(`ws://127.0.0.1:1/ws?token=${TOKEN}`, hello, {
      signal: controller.signal,
    });
    controller.abort(new Error('give up'));

    await expect(pending).rejects.toThrow('give up');
  });

  it('rejects right away with an aborted signal', async () => {
    await expect(
      dial('ws://127.0.0.1:1/ws', hello, { signal: AbortSignal.abort(new Error('already')) }),
    ).rejects.toThrow('already');
  });
});

describe('Conn.readEvent', () => {
  it('resolves to each event and skips invalid frames', async () => {
    const { conn } = await connect((peer) => {
      peer.send(event('action_invoked', { context: 'c1' }));
      peer.send('{');
      peer.send(Buffer.from('binary'));
      peer.send(JSON.stringify({ type: 'response', id: 'x', ok: true, data: {} }));
      peer.send(JSON.stringify({ type: 'event', name: 'input' }));
      peer.send(event('input'));
    });

    expect(await conn.readEvent()).toEqual({ name: 'action_invoked', data: { context: 'c1' } });
    for (let i = 0; i < 4; i++) {
      await expect(conn.readEvent()).rejects.toBeInstanceOf(InvalidFrameError);
    }
    expect(await conn.readEvent()).toEqual({ name: 'input', data: {} });
    await conn.close();
  });

  it.each([1000, 1001])('rejects with a ClosedError after a close with %i', async (code) => {
    const { conn } = await connect((peer) => {
      peer.send(event('input'));
      peer.ws.close(code);
    });

    // Frames that arrived before the close are still delivered.
    expect((await conn.readEvent()).name).toBe('input');
    await expect(conn.readEvent()).rejects.toBeInstanceOf(ClosedError);
  });

  it('rejects with another error when the connection drops', async () => {
    const { conn } = await connect((peer) => {
      peer.ws.terminate();
    });

    const err = await conn.readEvent().catch((e: unknown) => e);

    expect(err).not.toBeInstanceOf(ClosedError);
    expect((err as Error).message).toMatch(/closed abnormally/);
  });

  it('accepts a frame at the read limit and closes on a larger one', async () => {
    const atLimit = event('input', { pad: '' });
    const pad = (size: number) => event('input', { pad: 'x'.repeat(size - atLimit.length) });
    const { conn, peer } = await connect((p) => {
      p.send(pad(READ_LIMIT));
      p.send(pad(READ_LIMIT + 1));
      p.send(event('ignored'));
    });

    expect((await conn.readEvent()).name).toBe('input');
    await expect(conn.readEvent()).rejects.toThrow(/exceeds the 32768-byte read limit/);
    expect(await peer.closed).toBe(4009);
  });

  it('rejects with the abort reason and keeps the connection usable', async () => {
    const { conn, peer } = await connect();
    const controller = new AbortController();

    const pending = conn.readEvent({ signal: controller.signal });
    controller.abort(new Error('stop waiting'));
    await expect(pending).rejects.toThrow('stop waiting');

    peer.send(event('input'));
    expect((await conn.readEvent()).name).toBe('input');
    await conn.close();
  });

  it('rejects right away with an aborted signal', async () => {
    const { conn } = await connect();

    await expect(conn.readEvent({ signal: AbortSignal.abort(new Error('no')) })).rejects.toThrow(
      'no',
    );
    await conn.close();
  });
});

describe('Conn.close', () => {
  it('stops waiting when the daemon never answers the close', async () => {
    const { conn, peer } = await connect();
    // A paused socket never reads the close frame, so it never answers.
    peer.ws.pause();
    const timeout = Conn.closeTimeoutMs;
    Conn.closeTimeoutMs = 50;
    try {
      await conn.close();
    } finally {
      Conn.closeTimeoutMs = timeout;
    }

    await expect(conn.readEvent()).rejects.toThrow('daemon did not answer the close');
  });

  it('closes with a normal closure', async () => {
    const { conn, peer } = await connect();

    await conn.close();

    expect(await peer.closed).toBe(1000);
    await expect(conn.readEvent()).rejects.toBeInstanceOf(ClosedError);
  });
});

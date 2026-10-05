// Test helpers: a fake daemon the SDK connects to, so tests never need the
// real one. Excluded from the build and from coverage.
import { onTestFinished } from 'vitest';
import { WebSocketServer, type RawData, type WebSocket as ServerSocket } from 'ws';

export const TOKEN = 's3cr3t-token';

/** The daemon side of one connection. */
export class Peer {
  readonly ws: ServerSocket;
  /** Resolves to the close code the client sent (or 1005/1006). */
  readonly closed: Promise<number>;
  readonly #messages: string[] = [];
  #wake: (() => void) | undefined;

  constructor(ws: ServerSocket) {
    this.ws = ws;
    ws.on('message', (data: RawData) => {
      this.#messages.push(data.toString());
      this.#wake?.();
    });
    this.closed = new Promise((resolve) => ws.on('close', (code: number) => resolve(code)));
  }

  /** Resolves to the next message the client sent. */
  async next(): Promise<string> {
    for (;;) {
      const message = this.#messages.shift();
      if (message !== undefined) {
        return message;
      }
      await new Promise<void>((resolve) => {
        this.#wake = resolve;
      });
    }
  }

  send(frame: string | Buffer): void {
    this.ws.send(frame);
  }

  /** Reads the hello request and answers it with the given frame. */
  async answerHello(answer: (id: string) => string): Promise<HelloRequest> {
    const req = JSON.parse(await this.next()) as HelloRequest;
    this.send(answer(req.id));
    return req;
  }

  /** Reads the hello request and accepts it. */
  acceptHello(): Promise<HelloRequest> {
    return this.answerHello(helloOk);
  }
}

export interface HelloRequest {
  type: string;
  id: string;
  method: string;
  params: unknown;
}

export function helloOk(id: string): string {
  return JSON.stringify({
    type: 'response',
    id,
    ok: true,
    data: { server: { name: 'keyforged', version: '0.1.0' }, protocol_version: '1' },
  });
}

/**
 * Starts a fake daemon that runs handle for each connection and returns its
 * URL, auth token included. It is shut down when the test finishes.
 */
export async function fakeDaemon(handle: (peer: Peer) => void | Promise<void>): Promise<string> {
  const server = new WebSocketServer({ host: '127.0.0.1', port: 0 });
  server.on('connection', (ws) => void handle(new Peer(ws)));
  await new Promise<void>((resolve) => server.once('listening', resolve));
  onTestFinished(async () => {
    for (const client of server.clients) {
      client.terminate();
    }
    await new Promise<void>((resolve) => server.close(() => resolve()));
  });
  const address = server.address();
  if (address === null || typeof address === 'string') {
    throw new Error('fakeDaemon: unexpected pipe address');
  }
  return `ws://127.0.0.1:${address.port}/ws?token=${TOKEN}`;
}

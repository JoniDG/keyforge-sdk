/**
 * Connection to the KeyForge daemon for plugins and external clients: connect,
 * perform the hello handshake and read the events the daemon pushes.
 *
 * @module
 */
import { randomUUID } from 'node:crypto';
import type { Error as ProtocolError, MethodHello } from '@jdg-keyforge/protocol';
import { isObject, isPeerInfo, isString, type JsonObject } from './guards.js';

/** Params of the hello request. */
export type HelloParams = MethodHello['params'];

/** The daemon's answer to hello. */
export type HelloResult = MethodHello['result'];

/**
 * Largest frame the client accepts, in bytes: the default read limit of the Go
 * SDK and the daemon. A larger frame closes the connection.
 */
export const READ_LIMIT = 32 * 1024;

// Close codes a browser-style WebSocket may send are 1000 and 3000-4999, so
// 1009 (message too big) is mirrored in the private range.
const CLOSE_MESSAGE_TOO_BIG = 4009;

// How long close waits for the daemon to answer the close frame, like
// coder/websocket in the Go SDK.
const CLOSE_TIMEOUT_MS = 5000;

/**
 * Thrown once the daemon has closed the connection normally (status 1000 or
 * 1001). For a plugin this is the signal to exit.
 */
export class ClosedError extends Error {
  constructor() {
    super('client: connection closed');
    this.name = 'ClosedError';
  }
}

/**
 * Thrown for a single frame that doesn't match the protocol. When readEvent
 * throws it, the connection is still usable.
 */
export class InvalidFrameError extends Error {
  constructor(reason: string) {
    super(`client: invalid frame: ${reason}`);
    this.name = 'InvalidFrameError';
  }
}

/**
 * An error response from the daemon, e.g. a hello rejected with code
 * FORBIDDEN or UNSUPPORTED_PROTOCOL_VERSION.
 */
export class ServerError extends Error {
  readonly detail: ProtocolError;

  constructor(detail: ProtocolError) {
    super(`client: daemon error: ${detail.code}: ${detail.message}`);
    this.name = 'ServerError';
    this.detail = detail;
  }
}

/** An event frame pushed by the daemon. */
export interface KeyForgeEvent {
  /** The event name, e.g. "action_invoked". */
  name: string;
  /**
   * The event payload, unchecked: cast it to the protocol type for name
   * (e.g. EventActionInvoked['data']) after checking the fields you use.
   */
  data: JsonObject;
}

/** Options for a call that can be aborted. */
export interface AbortOptions {
  signal?: AbortSignal;
}

/** What dial resolves to. */
export interface DialResult {
  conn: Conn;
  result: HelloResult;
}

/**
 * A connection to the daemon that has completed the hello handshake. Get one
 * with dial; it can't be constructed directly.
 */
export class Conn {
  /** @internal Overridden by tests. */
  static closeTimeoutMs = CLOSE_TIMEOUT_MS;

  readonly #ws: WebSocket;
  // Frames are buffered as they arrive: a WebSocket pushes them, readEvent
  // pulls them.
  readonly #frames: (string | InvalidFrameError)[] = [];
  readonly #waiters = new Set<() => void>();
  readonly #closed: Promise<void>;
  #end: Error | undefined;

  /** @internal Use dial. */
  constructor(ws: WebSocket) {
    this.#ws = ws;
    this.#closed = new Promise((resolve) => {
      ws.addEventListener('close', (ev) => {
        this.#end ??=
          ev.code === 1000 || ev.code === 1001
            ? new ClosedError()
            : new Error(`client: connection closed abnormally (code ${ev.code})`);
        this.#wake();
        resolve();
      });
    });
    ws.addEventListener('message', (ev: MessageEvent) => {
      if (this.#end) {
        return;
      }
      if (!isString(ev.data)) {
        this.#frames.push(new InvalidFrameError('expected a text frame, got a binary one'));
      } else if (Buffer.byteLength(ev.data) > READ_LIMIT) {
        this.#end = new Error(
          `client: frame of ${Buffer.byteLength(ev.data)} bytes exceeds the ${READ_LIMIT}-byte read limit`,
        );
        ws.close(CLOSE_MESSAGE_TOO_BIG, 'message too big');
      } else {
        this.#frames.push(ev.data);
      }
      this.#wake();
    });
  }

  /**
   * Waits until the daemon pushes an event. Throws ClosedError when the
   * daemon closes the connection normally and InvalidFrameError for a frame
   * that doesn't match the protocol. Any other error means the connection is
   * gone. Aborting the signal rejects with its reason and leaves the
   * connection open.
   */
  async readEvent(options: AbortOptions = {}): Promise<KeyForgeEvent> {
    const frame = parseFrame(await this.nextFrame(options.signal));
    if (frame.type !== 'event') {
      throw new InvalidFrameError(`unexpected ${JSON.stringify(frame.type)} frame`);
    }
    if (!isString(frame.name) || !isObject(frame.data)) {
      throw new InvalidFrameError('event without a name or data');
    }
    return { name: frame.name, data: frame.data };
  }

  /**
   * Closes the connection with a normal closure and waits until the daemon
   * answers, for at most 5 seconds. After that it stops waiting, but the
   * built-in WebSocket can't drop the socket, so a process exiting at that
   * point should call process.exit.
   */
  async close(): Promise<void> {
    this.#ws.close(1000);
    let timer: NodeJS.Timeout | undefined;
    const timedOut = new Promise<void>((resolve) => {
      timer = setTimeout(resolve, Conn.closeTimeoutMs).unref();
    });
    await Promise.race([this.#closed, timedOut]);
    clearTimeout(timer);
    this.#end ??= new Error('client: daemon did not answer the close');
    this.#wake();
  }

  /** @internal Sends a text frame. */
  send(data: string): void {
    this.#ws.send(data);
  }

  /** @internal Resolves to the next raw frame. */
  async nextFrame(signal?: AbortSignal): Promise<string> {
    for (;;) {
      signal?.throwIfAborted();
      const frame = this.#frames.shift();
      if (frame instanceof InvalidFrameError) {
        throw frame;
      }
      if (frame !== undefined) {
        return frame;
      }
      if (this.#end) {
        throw this.#end;
      }
      await this.#wait(signal);
    }
  }

  #wait(signal?: AbortSignal): Promise<void> {
    return new Promise((resolve, reject) => {
      const onAbort = (): void => {
        this.#waiters.delete(wake);
        reject(signal?.reason);
      };
      const wake = (): void => {
        signal?.removeEventListener('abort', onAbort);
        resolve();
      };
      this.#waiters.add(wake);
      signal?.addEventListener('abort', onAbort, { once: true });
    });
  }

  #wake(): void {
    for (const wake of this.#waiters) {
      wake();
    }
    this.#waiters.clear();
  }
}

/**
 * Connects to wsUrl, sends the hello request and waits for the daemon to
 * accept it.
 *
 * Any error means there is no connection. A rejected hello throws a
 * ServerError, and a daemon that closes before answering a ClosedError.
 * Because wsUrl carries the auth token, a failed connection is reported with
 * the URL redacted. Aborting the signal rejects with its reason.
 */
export async function dial(
  wsUrl: string,
  hello: HelloParams,
  options: AbortOptions = {},
): Promise<DialResult> {
  const { signal } = options;
  checkHello(hello);
  signal?.throwIfAborted();

  let ws: WebSocket;
  try {
    ws = new WebSocket(wsUrl);
  } catch (err) {
    throw dialError(wsUrl, err instanceof Error ? err.message : String(err));
  }
  const conn = new Conn(ws);
  try {
    await opened(ws, wsUrl, signal);
    return { conn, result: await handshake(conn, hello, signal) };
  } catch (err) {
    ws.close();
    throw err;
  }
}

function checkHello(hello: HelloParams): void {
  // TypeScript callers can't get this wrong, JavaScript ones can.
  if (!isObject(hello) || hello.protocol_version !== '1' || !isPeerInfo(hello.client)) {
    throw new TypeError(
      'client.dial: invalid hello params: want { protocol_version: "1", client: { name, version } }',
    );
  }
}

function opened(ws: WebSocket, wsUrl: string, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    let reason = 'connection failed';
    const cleanup = (): void => {
      ws.removeEventListener('open', onOpen);
      ws.removeEventListener('error', onError);
      ws.removeEventListener('close', onClose);
      signal?.removeEventListener('abort', onAbort);
    };
    const onOpen = (): void => {
      cleanup();
      resolve();
    };
    const onError = (ev: Event): void => {
      if ('message' in ev && isString(ev.message) && ev.message !== '') {
        reason = ev.message;
      }
    };
    const onClose = (): void => {
      cleanup();
      reject(dialError(wsUrl, reason));
    };
    const onAbort = (): void => {
      cleanup();
      reject(signal?.reason);
    };
    ws.addEventListener('open', onOpen);
    ws.addEventListener('error', onError);
    ws.addEventListener('close', onClose);
    signal?.addEventListener('abort', onAbort, { once: true });
  });
}

async function handshake(
  conn: Conn,
  hello: HelloParams,
  signal?: AbortSignal,
): Promise<HelloResult> {
  const id = randomUUID();
  conn.send(JSON.stringify({ type: 'request', id, method: 'hello', params: hello }));

  const frame = parseFrame(await conn.nextFrame(signal));
  if (frame.type !== 'response' || typeof frame.ok !== 'boolean') {
    throw new InvalidFrameError(`expected hello response, got ${JSON.stringify(frame.type)} frame`);
  }
  if (frame.id !== id) {
    throw new InvalidFrameError(
      `response id ${JSON.stringify(frame.id)} does not match hello id ${JSON.stringify(id)}`,
    );
  }
  if (!frame.ok) {
    const detail = frame.error;
    if (!isObject(detail) || !isString(detail.code) || !isString(detail.message)) {
      throw new InvalidFrameError('error response without a code or message');
    }
    throw new ServerError({
      code: detail.code,
      message: detail.message,
      ...(isObject(detail.details) ? { details: detail.details } : {}),
    });
  }

  const result = frame.data;
  if (!isObject(result) || !isPeerInfo(result.server)) {
    throw new InvalidFrameError('hello result without server info');
  }
  if (result.protocol_version !== hello.protocol_version) {
    throw new InvalidFrameError(
      `hello result protocol_version ${JSON.stringify(result.protocol_version)}, want ${JSON.stringify(hello.protocol_version)}`,
    );
  }
  return {
    protocol_version: result.protocol_version,
    server: { name: result.server.name, version: result.server.version },
  };
}

function parseFrame(raw: string): JsonObject {
  let frame: unknown;
  try {
    frame = JSON.parse(raw);
  } catch {
    throw new InvalidFrameError('not JSON');
  }
  if (!isObject(frame) || !isString(frame.type)) {
    throw new InvalidFrameError('not an envelope');
  }
  return frame;
}

// dialError makes sure a connection error never contains the auth token,
// which travels in the URL's query.
function dialError(wsUrl: string, reason: string): Error {
  let msg = wsUrl === '' ? reason : reason.replaceAll(wsUrl, '<redacted url>');
  if (URL.canParse(wsUrl)) {
    const query = new URL(wsUrl).search.slice(1);
    if (query !== '') {
      msg = msg.replaceAll(query, '<redacted>');
    }
  }
  return new Error(`client.dial: ${msg}`);
}

import { run, textLogger } from '@jdg-keyforge/sdk/plugin';
import { VERSION, handlers } from './counter.js';

const controller = new AbortController();
for (const signal of ['SIGINT', 'SIGTERM'] as const) {
  process.once(signal, () => controller.abort());
}

const logger = textLogger();
let code = 0;
try {
  await run({ version: VERSION, handlers: handlers(logger), logger, signal: controller.signal });
} catch (err) {
  if (!controller.signal.aborted) {
    logger.error('plugin stopped', { error: err });
    code = 1;
  }
}
// A daemon that never answers the close frame would keep the socket, and with
// it the process, alive.
process.exit(code);

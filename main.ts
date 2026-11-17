// main.ts -- thin Node launcher for the Go forge binary.
//
// The trial runs `node main.ts` (Node 22, --experimental-strip-types).
// This file does nothing but spawn ./forge from the same directory with the
// inherited environment, wait for it to exit, and exit with its exit code.
// It must NOT exit before the binary does, and it forwards signals so the
// binary shuts down cleanly. If the binary is missing or not executable,
// it exits non-zero loudly: a silent launcher failure scores 0.

import { spawn } from 'node:child_process';
import { accessSync, constants, existsSync, statSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const dir = dirname(fileURLToPath(import.meta.url));
const binary = join(dir, 'forge');

// Conventional exit codes for death by signal (128 + signal number).
const SIGNAL_EXIT_CODE: Record<string, number> = {
  SIGHUP: 129,
  SIGINT: 130,
  SIGQUIT: 131,
  SIGTERM: 143,
};

function fail(message: string): never {
  process.stderr.write(`[launcher] ${message}\n`);
  process.exit(1);
}

if (!existsSync(binary)) {
  fail(`forge binary not found at ${binary}: the entry cannot run`);
}
try {
  if (!statSync(binary).isFile()) {
    fail(`forge at ${binary} is not a regular file: the entry cannot run`);
  }
  accessSync(binary, constants.X_OK);
} catch (err) {
  fail(`forge binary at ${binary} is not executable: ${err instanceof Error ? err.message : String(err)}`);
}

const child = spawn(binary, [], {
  cwd: dir,
  env: process.env,
  stdio: 'inherit',
});

child.on('error', (err) => {
  fail(`failed to spawn forge: ${err.message}`);
});

// Forward termination signals to the binary so it can shut down cleanly.
for (const signal of ['SIGINT', 'SIGTERM', 'SIGHUP']) {
  process.on(signal, () => {
    if (!child.kill(signal)) {
      process.kill(process.pid, signal);
    }
  });
}

child.on('exit', (code, signal) => {
  if (signal) {
    // Killed by a signal: exit with the conventional 128 + signal number.
    process.exit(SIGNAL_EXIT_CODE[signal] ?? 128);
  }
  process.exit(code ?? 1);
});

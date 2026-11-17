// This file is only a launcher. It starts the forge-go-bin program that
// sits in this same folder, hands it the terminal so its output shows up
// directly, waits until it finishes, and then exits with the same code.
import { spawn } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const folder = dirname(fileURLToPath(import.meta.url));
const binary = join(folder, "forge-go-bin");

const child = spawn(binary, process.argv.slice(2), { stdio: "inherit" });

let done = false;

child.on("error", (err) => {
  if (done) return;
  done = true;
  console.error(`Failed to start forge-go-bin: ${err.message}`);
  process.exit(1);
});

child.on("close", (code) => {
  if (done) return;
  done = true;
  process.exit(code ?? 1);
});

for (const signal of ["SIGINT", "SIGTERM"] as const) {
  process.on(signal, () => {
    if (child.exitCode === null && child.signalCode === null) {
      child.kill(signal);
    }
  });
}

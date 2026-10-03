import assert from "node:assert/strict";
import { chmod, mkdtemp, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { findSubswapper, switchArgs } from "./cli.ts";

test("switchArgs refuses names that could become flags or paths", () => {
  assert.deepEqual(switchArgs("claude", "auto"), ["switch", "claude", "auto"]);
  for (const bad of ["-force", "../x", "a b", ""]) {
    assert.throws(() => switchArgs("claude", bad));
  }
});

test("findSubswapper honors SUBSWAPPER_BIN, then PATH", async () => {
  const dir = await mkdtemp(join(tmpdir(), "subswapper-plugin-"));
  const binary = join(dir, "subswapper");
  await writeFile(binary, "#!/bin/sh\n");
  await chmod(binary, 0o755);
  assert.equal(await findSubswapper({ PATH: dir }), binary);
  assert.equal(await findSubswapper({ SUBSWAPPER_BIN: binary, PATH: "" }), binary);
});

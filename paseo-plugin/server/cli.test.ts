import assert from "node:assert/strict";
import { chmod, mkdtemp, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { addArgs, findSubswapper, runSubswapper, switchArgs } from "./cli.ts";

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

test("addArgs signs Codex in with a device code and validates names", () => {
  assert.deepEqual(addArgs("claude", "work"), ["add", "claude", "work"]);
  assert.deepEqual(addArgs("codex", "personal"), ["add", "codex", "personal", "-device"]);
  assert.throws(() => addArgs("claude", "-force"));
  assert.throws(() => addArgs("other", "work"));
});

test("runSubswapper passes stdin to the CLI and reports its failure output", async () => {
  const dir = await mkdtemp(join(tmpdir(), "subswapper-plugin-"));
  const binary = join(dir, "subswapper");
  // Echoes the arguments and the length of stdin, never stdin itself.
  await writeFile(binary, `#!/bin/sh
input=$(cat)
if [ "$1" = fail ]; then echo "subswapper: account rejected" >&2; exit 1; fi
echo "args=$* stdin=\${#input}"
`);
  await chmod(binary, 0o755);
  const previous = process.env.SUBSWAPPER_BIN;
  process.env.SUBSWAPPER_BIN = binary;
  try {
    assert.equal((await runSubswapper(["add", "claude", "work"], { input: "sk-ant-oat01-abcdefgh\n" })).trim(), "args=add claude work stdin=21");
    await assert.rejects(runSubswapper(["fail"]), /account rejected/);
  } finally {
    if (previous === undefined) {
      delete process.env.SUBSWAPPER_BIN;
    } else {
      process.env.SUBSWAPPER_BIN = previous;
    }
  }
});

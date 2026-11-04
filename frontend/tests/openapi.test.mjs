import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import YAML from 'yaml';

test('OpenAPI parses as YAML and resolves every local reference', async () => {
  const document = YAML.parse(await readFile(new URL('../../openapi.yaml', import.meta.url), 'utf8'));
  assert.ok(document.openapi && document.paths && document.components);
  const nodes = [document];
  for (const node of nodes) {
    if (!node || typeof node !== 'object') continue;
    if (typeof node.$ref === 'string' && node.$ref.startsWith('#/')) {
      let target = document;
      for (const part of node.$ref.slice(2).split('/')) {
        target = target?.[part.replaceAll('~1', '/').replaceAll('~0', '~')];
      }
      assert.notEqual(target, undefined, node.$ref);
    }
    nodes.push(...Object.values(node));
  }
});

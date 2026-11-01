import assert from 'node:assert/strict';
import test from 'node:test';
import { environmentValues } from '../src/environment.ts';

test('environment rows preserve values, remove empty draft rows and reject invalid or duplicate names', () => {
  assert.deepEqual(environmentValues([[' CI ', 'true'], ['EMPTY', ''], ['', ''], ['PROMPT', ' keep spaces ']]), {
    CI: 'true', EMPTY: '', PROMPT: ' keep spaces ',
  });
  assert.deepEqual(environmentValues(Object.entries({ TOKEN: 'SERVER_TOKEN' })), { TOKEN: 'SERVER_TOKEN' });
  assert.throws(() => environmentValues([['CI', 'true'], [' CI ', 'false']]), /不能重复/);
  for (const name of ['', '1TOKEN', 'HAS SPACE', 'A=B'])
    assert.throws(() => environmentValues([[name, 'value']]), /环境变量名/);
});

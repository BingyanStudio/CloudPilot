export type EnvironmentRow = [name: string, value: string];

export function environmentValues(rows: EnvironmentRow[]): Record<string, string> {
  const names = new Set<string>();
  const entries: EnvironmentRow[] = [];
  for (const [rawName, value] of rows) {
    const name = rawName.trim();
    if (!name && !value) continue;
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(name))
      throw new Error('环境变量名只能包含字母、数字和下划线，且不能以数字开头。');
    if (names.has(name)) throw new Error(`环境变量名不能重复：${name}`);
    names.add(name);
    entries.push([name, value]);
  }
  return Object.fromEntries(entries);
}

export class APIError extends Error {
  status: number;
  constructor(message: string, status: number) {
    super(message);
    this.status = status;
  }
}

export async function request<T>(path: string, options: RequestInit = {}): Promise<T> {
  const response = await fetch(path, {
    ...options,
    credentials: 'same-origin',
    headers: {
      ...(options.body ? { 'Content-Type': 'application/json' } : {}),
      ...options.headers,
    },
  });
  if (!response.ok) {
    const body = (await response.json().catch(() => null)) as { message?: string } | null;
    if (response.status === 401 && !path.startsWith('/api/auth/'))
      window.dispatchEvent(new Event('session-expired'));
    throw new APIError(body?.message || `请求失败 (${response.status})`, response.status);
  }
  if (response.status === 204) return undefined as T;
  return response.json() as Promise<T>;
}

export const post = <T>(path: string, body?: unknown) =>
  request<T>(path, {
    method: 'POST',
    body: body === undefined ? undefined : JSON.stringify(body),
  });

export function message(error: unknown): string {
  return error instanceof Error ? error.message : '请求失败，请重试。';
}

export function resourcePath(kind: string, name: string): string {
  return `/api/resources/${encodeURIComponent(kind)}/${encodeURIComponent(name)}`;
}

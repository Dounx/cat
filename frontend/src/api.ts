export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message)
  }
}

export const auth = { onUnauthorized: () => {} }

export async function api<T = any>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch('/api' + path, {
    method,
    headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const data = await res.json().catch(() => ({}))
  if (res.status === 401 && path !== '/login') auth.onUnauthorized()
  if (!res.ok) throw new ApiError(res.status, data.error || res.statusText)
  return data as T
}

export const get = <T = any>(path: string) => api<T>('GET', path)
export const post = <T = any>(path: string, body?: unknown) => api<T>('POST', path, body ?? {})
export const put = <T = any>(path: string, body: unknown) => api<T>('PUT', path, body)
export const del = <T = any>(path: string) => api<T>('DELETE', path)

import { useCallback, useEffect, useState } from 'react';
import { message, request } from './api';

export function useRoute() {
  const [route, setRoute] = useState(() => window.location.hash.slice(1) || '/');
  useEffect(() => {
    const change = () => setRoute(window.location.hash.slice(1) || '/');
    window.addEventListener('hashchange', change);
    return () => window.removeEventListener('hashchange', change);
  }, []);
  return route;
}

export function navigate(path: string) {
  window.location.hash = path;
}

export function useData<T>(path: string, interval = 0) {
  const [data, setData] = useState<T | undefined>();
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(true);
  const [version, setVersion] = useState(0);
  const refresh = useCallback(() => setVersion((value) => value + 1), []);
  useEffect(() => {
    let active = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const controller = new AbortController();
    const load = async () => {
      try {
        const result = await request<T>(path, { signal: controller.signal });
        if (active) {
          setData(result);
          setError('');
        }
      } catch (cause) {
        if (active) setError(message(cause));
      } finally {
        if (active) {
          setLoading(false);
          if (interval) timer = setTimeout(load, interval);
        }
      }
    };
    setLoading(true);
    void load();
    return () => {
      active = false;
      controller.abort();
      clearTimeout(timer);
    };
  }, [path, interval, version]);
  return { data, error, loading, refresh };
}

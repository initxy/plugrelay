import { useCallback, useMemo, useState } from "react";
import { ApiError } from "./api/errors";

/** Minimal form state: values, dirty tracking against the last saved snapshot, and field errors. */
export function useForm<T>(initial: T) {
  const [saved, setSaved] = useState(initial);
  const [values, setValues] = useState(initial);
  const [errors, setErrors] = useState<Record<string, string>>({});

  const dirty = useMemo(() => JSON.stringify(values) !== JSON.stringify(saved), [values, saved]);

  const set = useCallback(<K extends keyof T>(key: K, value: T[K]) => {
    setValues((v) => ({ ...v, [key]: value }));
    setErrors((e) => {
      if (!(key as string in e)) return e;
      const next = { ...e };
      delete next[key as string];
      return next;
    });
  }, []);

  /** Start over from a new saved snapshot (after load or save). */
  const reset = useCallback((next: T) => {
    setSaved(next);
    setValues(next);
    setErrors({});
  }, []);

  /** Map a 422 response onto fields; returns false when the error was not field-level. */
  const absorb = useCallback((err: unknown): boolean => {
    if (err instanceof ApiError && Object.keys(err.fields).length > 0) {
      setErrors(err.fields);
      return true;
    }
    return false;
  }, []);

  return { values, setValues, set, dirty, errors, setErrors, reset, absorb };
}

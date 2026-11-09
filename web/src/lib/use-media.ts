import { useSyncExternalStore } from "react";

export function useMediaQuery(query: string): boolean {
  return useSyncExternalStore(
    (cb) => {
      if (typeof matchMedia !== "function") return () => {};
      const mq = matchMedia(query);
      mq.addEventListener("change", cb);
      return () => mq.removeEventListener("change", cb);
    },
    () => (typeof matchMedia === "function" ? matchMedia(query).matches : true),
    () => true,
  );
}

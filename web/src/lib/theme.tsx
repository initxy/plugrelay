import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import { store } from "./storage";

export type ThemeChoice = "light" | "dark" | "system";

interface ThemeState {
  choice: ThemeChoice;
  dark: boolean;
  setChoice: (c: ThemeChoice) => void;
}

const ThemeContext = createContext<ThemeState | null>(null);
const KEY = "pr.theme";

function systemDark(): boolean {
  return typeof matchMedia === "function" && matchMedia("(prefers-color-scheme: dark)").matches;
}

export function ThemeProvider({ children }: { children: ReactNode }) {
  const [choice, setChoiceState] = useState<ThemeChoice>(() => {
    const v = store.get(KEY);
    return v === "light" || v === "dark" ? v : "system";
  });
  const [sysDark, setSysDark] = useState(systemDark);

  useEffect(() => {
    if (typeof matchMedia !== "function") return;
    const mq = matchMedia("(prefers-color-scheme: dark)");
    const on = () => setSysDark(mq.matches);
    mq.addEventListener("change", on);
    return () => mq.removeEventListener("change", on);
  }, []);

  const dark = choice === "dark" || (choice === "system" && sysDark);
  useEffect(() => {
    document.documentElement.classList.toggle("dark", dark);
  }, [dark]);

  const setChoice = useCallback((c: ThemeChoice) => {
    store.set(KEY, c);
    setChoiceState(c);
  }, []);

  const value = useMemo(() => ({ choice, dark, setChoice }), [choice, dark, setChoice]);
  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>;
}

export function useTheme(): ThemeState {
  const ctx = useContext(ThemeContext);
  if (!ctx) throw new Error("useTheme outside ThemeProvider");
  return ctx;
}

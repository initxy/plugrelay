import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from "react";
import { Segmented } from "@/components/ui/segmented";
import type { TimeRange } from "@/lib/api/types";
import { store } from "@/lib/storage";
import { isTimeRange, TIME_RANGES } from "@/lib/time-range";
import { useI18n } from "@/i18n";

interface RangeState {
  range: TimeRange;
  setRange: (r: TimeRange) => void;
}

const RangeContext = createContext<RangeState | null>(null);
const KEY = "pr.range";

/** The one time range every card and chart on a page follows. Remembered across visits. */
export function TimeRangeProvider({ children }: { children: ReactNode }) {
  const [range, setState] = useState<TimeRange>(() => {
    const v = store.get(KEY);
    return isTimeRange(v) ? v : "7d";
  });
  const setRange = useCallback((r: TimeRange) => {
    store.set(KEY, r);
    setState(r);
  }, []);
  const value = useMemo(() => ({ range, setRange }), [range, setRange]);
  return <RangeContext.Provider value={value}>{children}</RangeContext.Provider>;
}

export function useTimeRange(): RangeState {
  const ctx = useContext(RangeContext);
  if (!ctx) throw new Error("useTimeRange outside TimeRangeProvider");
  return ctx;
}

export function TimeRangePicker() {
  const { range, setRange } = useTimeRange();
  const { t } = useI18n();
  return (
    <Segmented
      aria-label={t("range.label")}
      value={range}
      onChange={setRange}
      options={TIME_RANGES.map((r) => ({ value: r, label: t(`range.${r}`) }))}
    />
  );
}

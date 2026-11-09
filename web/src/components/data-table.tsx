import * as M from "@radix-ui/react-dropdown-menu";
import {
  flexRender,
  getCoreRowModel,
  useReactTable,
  type ColumnDef,
  type RowData,
  type Table as TTable,
  type VisibilityState,
} from "@tanstack/react-table";
import { Check, Columns3 } from "lucide-react";
import { useState, type KeyboardEvent, type ReactNode } from "react";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/cn";
import { store } from "@/lib/storage";
import { useI18n } from "@/i18n";
import { EmptyState } from "./page";

declare module "@tanstack/react-table" {
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  interface ColumnMeta<TData extends RowData, TValue> {
    /** Classes for both th and td (width, alignment). */
    className?: string;
    /** Name in the column menu when the header is not plain text. */
    label?: string;
    /** Right-aligned with tabular figures: amounts, counts, durations. */
    numeric?: boolean;
    /** Grid track and hiding rule of the request log's own row layout. */
    width?: string;
    align?: "right";
    hideBelow?: "lg" | "xl";
  }
}

interface ViewProps<T> {
  table: TTable<T>;
  onRowClick?: (row: T) => void;
  /** Row id (TanStack's `getRowId`) of the row whose drawer is open. */
  selectedId?: string;
  loading?: boolean;
  /** Skeleton rows while loading; match the usual page size so nothing jumps. */
  loadingRows?: number;
  empty?: ReactNode;
  className?: string;
  /** Scroll inside the table with a sticky header. Defaults to the viewport minus the page chrome. */
  maxHeight?: string;
  /** Extra classes per row, e.g. a red edge on failed requests. */
  rowClassName?: (row: T) => string | undefined;
  /** Called when the table is scrolled near its end (load the next page). */
  onEndReached?: () => void;
}

function rowKeys(onClick: () => void) {
  return (e: KeyboardEvent<HTMLTableRowElement>) => {
    if (e.target !== e.currentTarget) return;
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      onClick();
    }
  };
}

/** Renders a TanStack Table instance: sticky header, skeleton while loading, empty state, clickable rows. */
function TableView<T>({
  table,
  onRowClick,
  selectedId,
  loading,
  loadingRows = 8,
  empty,
  className,
  maxHeight = "calc(100dvh - 13rem)",
  rowClassName,
  onEndReached,
}: ViewProps<T>) {
  const { t } = useI18n();
  const rows = table.getRowModel().rows;
  const columns = table.getVisibleLeafColumns();
  return (
    <div className={cn("overflow-hidden rounded-lg border border-line bg-surface", className)}>
      <div
        className="overflow-auto"
        style={{ maxHeight }}
        onScroll={
          onEndReached &&
          ((e) => {
            const el = e.currentTarget;
            if (el.scrollTop + el.clientHeight >= el.scrollHeight - 160) onEndReached();
          })
        }
      >
        <table className="w-full border-separate border-spacing-0 text-sm" aria-busy={loading || undefined}>
          <thead>
            {table.getHeaderGroups().map((hg) => (
              <tr key={hg.id}>
                {hg.headers.map((h) => {
                  const meta = h.column.columnDef.meta;
                  return (
                    <th
                      key={h.id}
                      scope="col"
                      className={cn(
                        "sticky top-0 z-[1] h-8 whitespace-nowrap border-b border-line bg-surface-2/95 px-3 text-left text-xs font-medium text-ink-3 backdrop-blur-sm first:pl-4 last:pr-4",
                        meta?.numeric && "text-right",
                        meta?.className,
                      )}
                    >
                      {h.isPlaceholder ? null : flexRender(h.column.columnDef.header, h.getContext())}
                    </th>
                  );
                })}
              </tr>
            ))}
          </thead>
          <tbody>
            {loading
              ? Array.from({ length: loadingRows }, (_, i) => (
                  <tr key={i} aria-hidden>
                    {columns.map((c, j) => (
                      <td key={c.id} className="h-10 border-b border-line-soft px-3 first:pl-4 last:pr-4">
                        <Skeleton className={cn("h-3.5", j === 0 ? "w-3/4" : c.columnDef.meta?.numeric ? "ml-auto w-12" : "w-1/2")} />
                      </td>
                    ))}
                  </tr>
                ))
              : rows.map((row) => {
                  const click = onRowClick ? () => onRowClick(row.original) : undefined;
                  const selected = selectedId !== undefined && row.id === selectedId;
                  return (
                    <tr
                      key={row.id}
                      onClick={click}
                      onKeyDown={click && rowKeys(click)}
                      tabIndex={click ? 0 : undefined}
                      aria-selected={click ? selected : undefined}
                      className={cn(
                        "group/row [&:last-child>td]:border-b-0",
                        click && "cursor-pointer outline-none focus-visible:[&>td]:bg-accent-soft/60",
                        click && !selected && "hover:[&>td]:bg-surface-2/70",
                        selected && "[&>td]:bg-accent-soft",
                        rowClassName?.(row.original),
                      )}
                    >
                      {row.getVisibleCells().map((cell) => {
                        const meta = cell.column.columnDef.meta;
                        return (
                          <td
                            key={cell.id}
                            className={cn(
                              "h-10 border-b border-line-soft px-3 align-middle first:pl-4 last:pr-4",
                              meta?.numeric && "text-right tabular-nums",
                              meta?.className,
                            )}
                          >
                            {flexRender(cell.column.columnDef.cell, cell.getContext())}
                          </td>
                        );
                      })}
                    </tr>
                  );
                })}
          </tbody>
        </table>
        {!loading && rows.length === 0 && (empty ?? <EmptyState compact title={t("table.empty")} />)}
      </div>
    </div>
  );
}

/** Column picker for a table: every hideable column, by its header text or `meta.label`. */
function ColumnsMenu<T>({ table }: { table: TTable<T> }) {
  const { t } = useI18n();
  const cols = table.getAllLeafColumns().filter((c) => c.getCanHide());
  if (cols.length === 0) return null;
  return (
    <M.Root>
      <M.Trigger asChild>
        <Button size="sm">
          <Columns3 />
          {t("table.columns")}
        </Button>
      </M.Trigger>
      <M.Portal>
        <M.Content
          align="end"
          sideOffset={4}
          className="z-50 min-w-48 rounded-lg border border-line bg-surface p-1 shadow-pop animate-[pop-in_100ms_var(--ease-out-quart)]"
        >
          {cols.map((c) => {
            const header = c.columnDef.header;
            const label = c.columnDef.meta?.label ?? (typeof header === "string" ? header : c.id);
            return (
              <M.CheckboxItem
                key={c.id}
                checked={c.getIsVisible()}
                onCheckedChange={(v) => c.toggleVisibility(v === true)}
                onSelect={(e) => e.preventDefault()}
                className="relative flex cursor-default select-none items-center rounded-sm py-1.5 pl-7 pr-2 text-sm outline-none data-[highlighted]:bg-surface-2"
              >
                <M.ItemIndicator className="absolute left-2">
                  <Check className="size-3.5 text-accent" />
                </M.ItemIndicator>
                {label}
              </M.CheckboxItem>
            );
          })}
        </M.Content>
      </M.Portal>
    </M.Root>
  );
}

const visibilityKey = (id: string) => `pr.table.${id}.columns`;

function savedVisibility(id: string | undefined): VisibilityState {
  if (!id) return {};
  try {
    const v: unknown = JSON.parse(store.get(visibilityKey(id)) ?? "{}");
    return v && typeof v === "object" ? (v as VisibilityState) : {};
  } catch {
    return {};
  }
}

export interface DataTableProps<T> extends Omit<ViewProps<T>, "table"> {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  columns: ColumnDef<T, any>[];
  data: T[] | undefined;
  getRowId?: (row: T) => string;
  /** Remembers hidden columns under this name. */
  id?: string;
  /** Things left of the column picker (filters live in a `FilterBar` above instead). */
  toolbar?: ReactNode;
  /** Show the column picker. */
  columnPicker?: boolean;
  /** A line under the table: a row count, "load more". */
  footer?: ReactNode;
  /** Something between the toolbar and the table, e.g. a chart of the rows. */
  beforeTable?: ReactNode;
  /** Columns hidden until someone picks them in the column picker. */
  defaultHidden?: readonly string[];
}

/**
 * The console's table. Data in, rows out: sticky header, skeleton rows while `loading`, an empty
 * state, row click (pair with `useDrawerParam`), and a column picker that remembers its choice.
 */
export function DataTable<T>({
  columns,
  data,
  getRowId,
  id,
  toolbar,
  columnPicker,
  footer,
  beforeTable,
  defaultHidden,
  ...view
}: DataTableProps<T>) {
  const [columnVisibility, setVisibility] = useState<VisibilityState>(() => ({
    ...Object.fromEntries((defaultHidden ?? []).map((c) => [c, false])),
    ...savedVisibility(id),
  }));
  const table = useReactTable({
    data: data ?? [],
    columns,
    getRowId,
    getCoreRowModel: getCoreRowModel(),
    state: { columnVisibility },
    onColumnVisibilityChange: (u) =>
      setVisibility((prev) => {
        const next = typeof u === "function" ? u(prev) : u;
        if (id) store.set(visibilityKey(id), JSON.stringify(next));
        return next;
      }),
  });
  return (
    <div className="grid gap-2">
      {(toolbar || columnPicker) && (
        <div className="flex flex-wrap items-center gap-2">
          <div className="flex min-w-0 flex-1 flex-wrap items-center gap-2">{toolbar}</div>
          {columnPicker && <ColumnsMenu table={table} />}
        </div>
      )}
      {beforeTable}
      <TableView table={table} {...view} loading={view.loading ?? data === undefined} />
      {footer && <div className="px-1 text-xs text-ink-3">{footer}</div>}
    </div>
  );
}

import type { ColumnDef } from "@tanstack/react-table";
import { CircleSlash, Download, Plus, RefreshCw, Server, Trash2 } from "lucide-react";
import { useMemo, useState, type ReactNode } from "react";
import { toast } from "sonner";
import { useConfirm } from "@/components/confirm";
import { DataTable } from "@/components/data-table";
import { DetailList, Drawer } from "@/components/drawer";
import { ClearFilters, FilterBar, FilterSearch, FilterSelect, FilterToggle } from "@/components/filter-bar";
import { DurationInput, FormSection, HelpText, MoneyInput, SwitchRow } from "@/components/form-kit";
import { EmptyState, PageHeader, Panel } from "@/components/page";
import { StatTile } from "@/components/stat-tile";
import { Pill, StatusDot, StatusText, type Tone } from "@/components/status";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input, Textarea } from "@/components/ui/input";
import { Segmented } from "@/components/ui/segmented";
import { Select } from "@/components/ui/select";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { cn } from "@/lib/cn";
import { pickEnum, pickString } from "@/lib/url-filters";
import { useDrawerParam, useUrlSearch } from "@/lib/use-url-search";

/*
 * Dev-only gallery (`/dev/components`): every shared component, once per theme. Sample data is
 * hard-coded here on purpose; nothing in this file ships.
 */

interface Row {
  id: string;
  time: string;
  client: string;
  clientTone: string;
  model: string;
  tier: "passthrough" | "patch" | "convert";
  cache?: number;
  retries?: number;
  status: "ok" | "error";
  error?: string;
  ttfb: string;
  total: string;
  upstream: string;
  cost: string;
}

const ROWS: Row[] = [
  { id: "r1", time: "02:37:53", client: "Codex", clientTone: "var(--violet)", model: "gpt-5.5", tier: "passthrough", cache: 61, status: "ok", ttfb: "1.2s", total: "8.8s", upstream: "openai", cost: "¥0.189" },
  { id: "r2", time: "02:37:20", client: "Claude Code", clientTone: "var(--accent)", model: "claude-sonnet-4-5", tier: "passthrough", cache: 74, retries: 1, status: "ok", ttfb: "1.8s", total: "7.4s", upstream: "anthropic-main", cost: "¥0.302" },
  { id: "r3", time: "02:36:39", client: "Claude Code", clientTone: "var(--accent)", model: "deepseek-chat", tier: "convert", status: "ok", ttfb: "0.7s", total: "3.1s", upstream: "deepseek", cost: "¥0.033" },
  { id: "r4", time: "02:35:51", client: "Cursor", clientTone: "var(--amber)", model: "auto-coding → claude-sonnet-4-5", tier: "patch", status: "ok", ttfb: "0.9s", total: "7.9s", upstream: "anthropic-main", cost: "¥0.435" },
  { id: "r5", time: "02:34:16", client: "Codex", clientTone: "var(--violet)", model: "gpt-5-codex", tier: "passthrough", status: "error", error: "上游 503", ttfb: "—", total: "2.0s", upstream: "openai", cost: "¥0" },
  { id: "r6", time: "02:33:02", client: "Claude Code", clientTone: "var(--accent)", model: "claude-opus-4-1", tier: "passthrough", cache: 88, status: "ok", ttfb: "2.4s", total: "21.6s", upstream: "bedrock-us", cost: "¥1.204" },
];

const TIER: Record<Row["tier"], { label: string; tone: Tone }> = {
  passthrough: { label: "原样转发", tone: "neutral" },
  patch: { label: "小改转发", tone: "info" },
  convert: { label: "协议转换", tone: "violet" },
};

function Section({ title, children, className }: { title: string; children: ReactNode; className?: string }) {
  return (
    <section className={cn("grid gap-3", className)}>
      <h2 className="text-2xs font-medium tracking-[0.06em] text-ink-3">{title}</h2>
      {children}
    </section>
  );
}

function Swatch({ name, varName }: { name: string; varName: string }) {
  return (
    <div className="flex items-center gap-2.5">
      <span className="size-8 shrink-0 rounded-md border border-line" style={{ background: `var(${varName})` }} />
      <span className="min-w-0 leading-tight">
        <span className="block text-xs font-medium">{name}</span>
        <span className="block font-mono text-2xs text-ink-3">{varName}</span>
      </span>
    </div>
  );
}

function Tokens() {
  const groups: [string, [string, string][]][] = [
    ["表面", [["页面底色", "--bg"], ["卡片", "--surface"], ["悬停 / 填充", "--surface-2"], ["分隔线", "--line"], ["输入框边框", "--line-strong"]]],
    ["文字", [["正文", "--ink"], ["次要", "--ink-2"], ["提示", "--ink-3"]]],
    ["品牌", [["主色", "--accent"], ["选中底色", "--accent-soft"], ["选中文字", "--accent-strong"]]],
    ["状态", [["正常", "--ok"], ["注意", "--warn"], ["失败", "--bad"], ["信息", "--info"], ["待处理", "--violet"]]],
  ];
  return (
    <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
      {groups.map(([title, items]) => (
        <div key={title} className="grid content-start gap-2.5 rounded-lg border border-line bg-surface p-3.5">
          <div className="text-xs font-semibold">{title}</div>
          {items.map(([n, v]) => (
            <Swatch key={v} name={n} varName={v} />
          ))}
        </div>
      ))}
    </div>
  );
}

function Type() {
  return (
    <div className="grid gap-2 rounded-lg border border-line bg-surface p-4">
      <div className="text-2xl font-semibold tabular-nums">¥723.62 · 22 / 600</div>
      <div className="text-xl font-semibold">页面标题 Page title · 18 / 600</div>
      <div className="text-base font-medium">卡片标题 Panel title · 14 / 500</div>
      <div className="text-sm">正文：转发、选路、协议转换这些底层机制保持不变。Body text at 13px. · 13 / 400</div>
      <div className="text-xs text-ink-2">辅助说明：留空沿用分组设置。Help text · 12</div>
      <div className="text-2xs text-ink-3">分组标题 · 计数 · 11</div>
      <div className="font-mono text-xs">sk-••••••••3f9a · claude-sonnet-4-5 · IBM Plex Mono</div>
    </div>
  );
}

function Buttons() {
  return (
    <div className="grid gap-3">
      <div className="flex flex-wrap items-center gap-2">
        <Button variant="primary">
          <Plus />
          添加上游
        </Button>
        <Button>
          <RefreshCw />
          全部测试
        </Button>
        <Button variant="ghost">取消</Button>
        <Button variant="danger">
          <Trash2 />
          删除
        </Button>
        <Button variant="danger-solid">确认删除</Button>
        <Button variant="link">去查看 →</Button>
        <Button variant="primary" disabled>
          保存中…
        </Button>
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <Button size="xs">xs</Button>
        <Button size="sm">sm 小按钮</Button>
        <Button size="md">md 默认</Button>
        <Button size="lg">lg 大按钮</Button>
        <Button size="icon-sm" aria-label="下载">
          <Download />
        </Button>
        <Button size="icon" variant="ghost" aria-label="上游">
          <Server />
        </Button>
      </div>
    </div>
  );
}

function States() {
  const tones: [Tone, string][] = [
    ["ok", "运行中"],
    ["warn", "被限流，冷却到 02:46"],
    ["bad", "上游暂停"],
    ["violet", "疑似封号，已停用"],
    ["info", "同步中"],
    ["brand", "新"],
    ["neutral", "未开启"],
  ];
  return (
    <div className="grid gap-3">
      <div className="flex flex-wrap items-center gap-2">
        {tones.map(([tone, text]) => (
          <Pill key={tone} tone={tone}>
            {text}
          </Pill>
        ))}
      </div>
      <div className="flex flex-wrap items-center gap-2">
        {tones.map(([tone, text]) => (
          <Pill key={tone} tone={tone} dot>
            {text}
          </Pill>
        ))}
      </div>
      <div className="flex flex-wrap items-center gap-x-5 gap-y-2">
        <StatusText tone="ok">正常</StatusText>
        <StatusText tone="warn" pulse>
          重试中
        </StatusText>
        <StatusText tone="bad">失败</StatusText>
        <StatusText tone="neutral">已关闭</StatusText>
        <span className="inline-flex items-center gap-2 text-ink-2">
          <StatusDot tone="ok" label="正常" />
          <StatusDot tone="warn" label="注意" />
          <StatusDot tone="bad" label="失败" />
          <StatusDot tone="violet" label="待处理" />
        </span>
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <Badge>anthropic</Badge>
        <Badge tone="outline">YAML</Badge>
        <Badge tone="accent">v1.4</Badge>
        <Badge tone="dashed">只读</Badge>
      </div>
    </div>
  );
}

function Stats() {
  return (
    <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
      <StatTile label="花费" value="¥723.62" delta={{ ratio: 0.06, goodWhen: "down", label: "比昨天" }} spark={[22, 20, 23, 17, 18, 14, 16, 11, 12, 6].map((v) => 30 - v)} onClick={() => toast("打开这段时间的请求")} />
      <StatTile label="请求 · 成功率" value="1,226" note="成功 97.4%" spark={[12, 14, 11, 16, 15, 18, 17, 21, 19, 22]} />
      <StatTile label="缓存命中" value="66%" delta={{ ratio: 0.031, label: "省下 ¥609.00" }} spark={[16, 17, 15, 18, 19, 18, 20, 20, 21, 22]} sparkTone="ok" />
      <StatTile label="首字时间（95% 的请求在此之内）" value="1.8 秒" delta={{ ratio: 0.12, goodWhen: "down", label: "比昨天慢 0.2 秒" }} spark={[10, 11, 9, 12, 11, 14, 13, 16, 17, 19]} sparkTone="amber" />
      <StatTile label="正在加载" value="" loading spark={[1, 2]} />
    </div>
  );
}

function useColumns(): ColumnDef<Row>[] {
  return useMemo(
    () => [
      { accessorKey: "time", header: "时间", enableHiding: false, meta: { className: "w-[84px] tabular-nums text-ink-2" } },
      {
        id: "client",
        header: "客户端",
        cell: ({ row }) => (
          <span className="inline-flex items-center gap-1.5 whitespace-nowrap">
            <i className="size-2 rounded-[2px]" style={{ background: row.original.clientTone }} />
            {row.original.client}
          </span>
        ),
      },
      {
        id: "model",
        header: "模型 · 怎么处理的",
        enableHiding: false,
        cell: ({ row }) => {
          const r = row.original;
          return (
            <span className="flex flex-wrap items-center gap-1.5">
              <span className="font-medium">{r.model}</span>
              {r.status === "error" ? (
                <Pill tone="bad">失败 · {r.error}</Pill>
              ) : (
                <>
                  <Pill tone={TIER[r.tier].tone}>{TIER[r.tier].label}</Pill>
                  {r.cache !== undefined && <Pill tone="ok">缓存 {r.cache}%</Pill>}
                  {r.retries && <Pill tone="warn">重试 {r.retries} 次</Pill>}
                </>
              )}
            </span>
          );
        },
      },
      { id: "latency", header: "首字 / 总耗时", meta: { label: "首字 / 总耗时", className: "whitespace-nowrap tabular-nums text-ink-2" }, cell: ({ row }) => `${row.original.ttfb} / ${row.original.total}` },
      { accessorKey: "upstream", header: "上游", meta: { className: "text-ink-2" } },
      { accessorKey: "cost", header: "费用", meta: { numeric: true } },
    ],
    [],
  );
}

function Table() {
  const columns = useColumns();
  const drawer = useDrawerParam();
  const [search, set] = useUrlSearch();
  const status = pickEnum(search.status, ["ok", "error"] as const);
  const model = pickString(search.model);
  const q = pickString(search.q);
  const failed = search.failed === true;
  const [loading, setLoading] = useState(false);
  const rows = ROWS.filter(
    (r) =>
      (!status || r.status === status) &&
      (!failed || r.status === "error") &&
      (!model || r.model.includes(model)) &&
      (!q || `${r.model} ${r.upstream} ${r.client}`.toLowerCase().includes(q.toLowerCase())),
  );
  const active = [status, model, q, failed || undefined].filter(Boolean).length;
  return (
    <div className="grid gap-2.5">
      <FilterBar>
        <FilterSearch value={q} onChange={(v) => set({ q: v }, { replace: true })} placeholder="请求号或模型" />
        <FilterSelect
          label="模型"
          allLabel="全部模型"
          value={model}
          onChange={(v) => set({ model: v })}
          options={[...new Set(ROWS.map((r) => r.model))].map((m) => ({ value: m, label: m }))}
        />
        <FilterSelect
          label="结果"
          allLabel="全部结果"
          value={status}
          onChange={(v) => set({ status: v })}
          options={[
            { value: "ok", label: "成功" },
            { value: "error", label: "失败" },
          ]}
        />
        <FilterToggle pressed={failed} onChange={(v) => set({ failed: v })}>
          只看失败
        </FilterToggle>
        <ClearFilters count={active} onClear={() => set({ q: undefined, model: undefined, status: undefined, failed: undefined })} />
      </FilterBar>
      <DataTable
        id="dev.requests"
        columns={columns}
        data={rows}
        loading={loading}
        getRowId={(r) => r.id}
        onRowClick={(r) => drawer.open(r.id)}
        selectedId={drawer.id}
        columnPicker
        maxHeight="320px"
        toolbar={
          <Segmented
            size="sm"
            aria-label="视图"
            value={loading ? "loading" : "rows"}
            onChange={(v) => setLoading(v === "loading")}
            options={[
              { value: "rows", label: "有数据" },
              { value: "loading", label: "加载中" },
            ]}
          />
        }
        empty={<EmptyState compact icon={CircleSlash} title="没有符合筛选条件的请求" hint="放宽时间范围或者去掉一个条件试试。" />}
        footer={`共 ${rows.length} 条 · 点一行从右侧打开详情，网址会带上 ?id=`}
      />
    </div>
  );
}

function RequestDrawer() {
  const drawer = useDrawerParam();
  const row = ROWS.find((r) => r.id === drawer.id);
  const [tab, setTab] = useState("overview");
  return (
    <Tabs value={tab} onValueChange={setTab}>
      <Drawer
        open={row !== undefined}
        onOpenChange={drawer.onOpenChange}
        title={row ? `${row.model} · ${row.time}` : ""}
        subtitle={row ? `${row.client} · ${row.upstream}` : undefined}
        meta={row && (row.status === "ok" ? <Pill tone="ok" dot>成功</Pill> : <Pill tone="bad" dot>失败</Pill>)}
        actions={
          <Button size="sm" variant="ghost">
            复制链接
          </Button>
        }
        tabs={
          <TabsList>
            <TabsTrigger value="overview">概览</TabsTrigger>
            <TabsTrigger value="attempts">上游尝试</TabsTrigger>
            <TabsTrigger value="cost">费用</TabsTrigger>
            <TabsTrigger value="raw">原始数据</TabsTrigger>
          </TabsList>
        }
        footer={
          <>
            <Button onClick={drawer.close}>关闭</Button>
            <Button variant="primary">用同样参数重放</Button>
          </>
        }
      >
        {row && (
          <div className="p-5">
            <TabsContent value="overview">
              <DetailList
                items={[
                  { label: "密钥", value: "laptop-claude（张三）" },
                  { label: "客户端", value: `${row.client} 2.4 · 来自 10.2.3.18` },
                  { label: "会话", value: <a className="text-accent hover:underline" href="#">第 14 轮，这个会话累计花了 ¥3.12</a> },
                  { label: "处理方式", value: `${TIER[row.tier].label}：请求逐字节没改，所以缓存照常命中` },
                  { label: "首字 / 总耗时", value: `${row.ttfb} / ${row.total}` },
                ]}
              />
            </TabsContent>
            <TabsContent value="attempts" className="text-sm text-ink-2">
              ① anthropic-main / team-2：被限流（429），这个凭据冷却到 02:46，换下一个。② team-1 成功。
            </TabsContent>
            <TabsContent value="cost" className="text-sm tabular-nums">
              合计 {row.cost}（分组倍率 1.0）
            </TabsContent>
            <TabsContent value="raw">
              <pre className="overflow-x-auto rounded-md bg-surface-2 p-3 font-mono text-xs">{JSON.stringify(row, null, 2)}</pre>
            </TabsContent>
          </div>
        )}
      </Drawer>
    </Tabs>
  );
}

function Forms() {
  const [budget, setBudget] = useState<number | null>(200);
  const [interval, setInterval] = useState<string | null>("15m");
  const [sync, setSync] = useState(true);
  const [test, setTest] = useState(false);
  const [strategy, setStrategy] = useState("priority");
  return (
    <Panel title="表单">
      <FormSection title="基础" description="名称和地址，保存后客户端马上能用。" layout="split">
        <Field label="名称" hint="只用来在控制台里认出它。">
          <Input defaultValue="deepseek" />
        </Field>
        <Field label="API Key" hint="可以一次贴多个，一行一个。" optional>
          <Textarea className="font-mono text-xs" defaultValue={"sk-••••••••••••3f9a\nsk-••••••••••••81c2"} />
        </Field>
        <Field label="地址" error="请填一个 https:// 开头的地址">
          <Input defaultValue="api.deepseek.com" />
        </Field>
      </FormSection>
      <FormSection title="额度与选路" description="金额都按站点币种填写，不用换算。" layout="split">
        <Field label="每月预算" hint={budget === null ? "留空表示不设上限。" : `当前：¥${budget}`}>
          <MoneyInput value={budget} onChange={setBudget} currency="CNY" />
        </Field>
        <Field label="定时测试间隔" hint={`保存的值：${interval ?? "（空）"}`}>
          <DurationInput value={interval} onChange={setInterval} />
        </Field>
        <Field label="选路方式">
          <Select
            value={strategy}
            onChange={setStrategy}
            options={[
              { value: "priority", label: "按优先级和权重（推荐）" },
              { value: "cheapest", label: "最便宜的优先" },
              { value: "fastest", label: "最快的优先" },
            ]}
          />
        </Field>
        <SwitchRow label="自动同步模型列表" description="新出的模型自动加进来，下线的模型自动停用，不会删除。" checked={sync} onChange={setSync} />
        <SwitchRow label="保存前先发一条测试请求" checked={test} onChange={setTest} aside={<Pill tone="brand">新</Pill>} />
        <SwitchRow label="记录请求正文" description="开了计费功能才能改。" checked={false} onChange={() => {}} disabled />
        <HelpText>帮助文字用在整组表单下面，口气要平。</HelpText>
      </FormSection>
    </Panel>
  );
}

function Feedback() {
  const confirm = useConfirm();
  return (
    <div className="grid gap-3 lg:grid-cols-2">
      <Panel title="空状态" bodyClassName="p-0">
        <EmptyState
          icon={Server}
          title="还没有上游"
          hint="从服务商列表里选一个，只需要填 API key。"
          action={
            <Button variant="primary">
              <Plus />
              添加上游
            </Button>
          }
        />
      </Panel>
      <Panel title="确认框和提示" aside="toast 用 sonner">
        <div className="flex flex-wrap gap-2">
          <Button
            variant="danger"
            onClick={async () => {
              const ok = await confirm({ title: "删除 laptop-claude？", body: "之后用这把 key 的客户端都会收到 401，请求日志会保留。", confirm: "删除", danger: true });
              if (ok) toast.success("已删除 laptop-claude");
            }}
          >
            危险操作确认
          </Button>
          <Button onClick={() => void confirm({ title: "放弃没保存的修改？", body: "这里的修改还没有保存。" })}>普通确认</Button>
          <Button onClick={() => toast.success("设置已保存并生效")}>成功提示</Button>
          <Button onClick={() => toast.error("保存失败：上游返回 401")}>失败提示</Button>
          <Button onClick={() => toast.warning("已保存，部分修改重启后生效。")}>警告提示</Button>
          <Button onClick={() => toast("已复制", { description: "完整的 key 只显示这一次。" })}>普通提示</Button>
        </div>
      </Panel>
    </div>
  );
}

function Gallery() {
  const [range, setRange] = useState("today");
  return (
    <div className="grid gap-8">
      <PageHeader
        title="总览"
        description="页头：标题、一句说明、右边放操作。"
        meta={<Pill tone="ok" dot>运行中</Pill>}
        actions={
          <>
            <Segmented
              aria-label="时间范围"
              value={range}
              onChange={setRange}
              options={[
                { value: "1h", label: "1 小时" },
                { value: "today", label: "今天" },
                { value: "7d", label: "7 天" },
                { value: "30d", label: "30 天" },
              ]}
            />
            <Button variant="primary">
              <Plus />
              新建 key
            </Button>
          </>
        }
        className="mb-0"
      />
      <Section title="数字卡片">
        <Stats />
      </Section>
      <Section title="表格 · 筛选 · 抽屉">
        <Table />
      </Section>
      <Section title="状态">
        <States />
      </Section>
      <Section title="按钮">
        <Buttons />
      </Section>
      <Section title="表单">
        <Forms />
      </Section>
      <Section title="反馈">
        <Feedback />
      </Section>
      <Section title="颜色">
        <Tokens />
      </Section>
      <Section title="字号">
        <Type />
      </Section>
    </div>
  );
}

export function ComponentsPage() {
  const [which, setWhich] = useState<"both" | "light" | "dark">("both");
  return (
    <>
      <PageHeader
        title="组件"
        description="开发用：所有共用组件，浅色和深色各一份。只在开发构建里有。"
        actions={
          <Segmented
            aria-label="主题"
            value={which}
            onChange={setWhich}
            options={[
              { value: "both", label: "两种都看" },
              { value: "light", label: "浅色" },
              { value: "dark", label: "深色" },
            ]}
          />
        }
      />
      <div className="grid gap-6">
        {(which === "both" || which === "light") && (
          <div className="light rounded-xl border border-line bg-bg p-5 text-ink md:p-7">
            <Gallery />
          </div>
        )}
        {(which === "both" || which === "dark") && (
          <div className="dark rounded-xl border border-line bg-bg p-5 text-ink md:p-7">
            <Gallery />
          </div>
        )}
      </div>
      <RequestDrawer />
    </>
  );
}

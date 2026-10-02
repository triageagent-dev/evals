import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { css } from '@emotion/react';
import { Bar } from 'react-chartjs-2';
import {
  Chart as ChartJS,
  CategoryScale,
  LinearScale,
  BarElement,
  Tooltip,
  Legend,
} from 'chart.js';
import { RefreshCw } from 'lucide-react';
import { getUsage, getUsageCalls, StorageUnavailableError } from '../../api/client';
import { estimateCostUsd, formatCostUsd } from '../../lib/pricing';
import type { UsageBucket, UsageCall, UsageSeries } from '../../lib/types';

ChartJS.register(CategoryScale, LinearScale, BarElement, Tooltip, Legend);

// Token Usage view - additive over Python (agentevals has no usage view).
// Reads the LLM call ledger (GET /api/usage, /api/usage/calls).

type Dimension = 'kind' | 'tenant' | 'model' | 'service';

const RANGES: { label: string; hours: number }[] = [
  { label: '24h', hours: 24 },
  { label: '7d', hours: 24 * 7 },
  { label: '30d', hours: 24 * 30 },
  { label: '90d', hours: 24 * 90 },
];

const DIMENSIONS: { key: Dimension; label: string }[] = [
  { key: 'kind', label: 'Call kind' },
  { key: 'tenant', label: 'Tenant' },
  { key: 'model', label: 'Model' },
  { key: 'service', label: 'Service' },
];

// Categorical slots for the dark surface, in fixed order (validated against
// --card-bg #252a36: CVD ΔE 8.4, normal-vision ΔE 19.3; green sits under
// 3:1, which the breakdown table relieves). An eighth group and beyond fold
// into "Other".
const SLOTS = ['#3987e5', '#d95926', '#199e70', '#c98500', '#d55181', '#008300', '#9085e9'];
const OTHER_COLOR = '#6b7586';
const OTHER = 'Other';
const NONE = '(none)';

// triage-core's call kinds keep the same colour in every window.
const KIND_ORDER = ['reflection', 'rca', 'guardrail', 'agent', 'knowledge', 'rsi', 'unknown'];

const SURFACE = '#252a36';
const REFRESH_MS = 60_000;

interface Totals {
  calls: number;
  errors: number;
  input: number;
  output: number;
  durationMs: number;
  cost: number;
  unpricedTokens: number;
}

function emptyTotals(): Totals {
  return { calls: 0, errors: 0, input: 0, output: 0, durationMs: 0, cost: 0, unpricedTokens: 0 };
}

function addBucket(t: Totals, b: UsageBucket) {
  t.calls += b.calls;
  t.errors += b.errors;
  t.input += b.inputTokens;
  t.output += b.outputTokens;
  t.durationMs += b.durationMs;
  const cost = estimateCostUsd(b.model, b.inputTokens, b.outputTokens);
  if (cost == null) t.unpricedTokens += b.inputTokens + b.outputTokens;
  else t.cost += cost;
}

function groupName(b: UsageBucket, dim: Dimension): string {
  return b[dim] || NONE;
}

function formatTokens(n: number): string {
  if (n >= 1e9) return `${(n / 1e9).toFixed(2)}B`;
  if (n >= 1e6) return `${(n / 1e6).toFixed(2)}M`;
  if (n >= 1e3) return `${(n / 1e3).toFixed(1)}k`;
  return String(n);
}

function formatMs(ms: number): string {
  return ms >= 1000 ? `${(ms / 1000).toFixed(1)}s` : `${Math.round(ms)}ms`;
}

function bucketLabel(start: number, bucketMs: number): string {
  const d = new Date(start);
  if (bucketMs >= 24 * 3600 * 1000) {
    return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
  }
  const hh = d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', hour12: false });
  return d.getHours() === 0 ? `${d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })} ${hh}` : hh;
}

export const UsageView: React.FC = () => {
  const [hours, setHours] = useState(24 * 7);
  const [dim, setDim] = useState<Dimension>('kind');
  const [series, setSeries] = useState<UsageSeries | null>(null);
  const [calls, setCalls] = useState<UsageCall[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const [s, c] = await Promise.all([getUsage(hours), getUsageCalls(hours, 25)]);
      setSeries(s);
      setCalls(c);
      setError(null);
    } catch (err) {
      setError(
        err instanceof StorageUnavailableError
          ? 'Token usage needs the session database (start the server with --session-db).'
          : err instanceof Error ? err.message : String(err),
      );
    } finally {
      setLoading(false);
    }
  }, [hours]);

  useEffect(() => {
    const first = setTimeout(load, 0);
    const id = setInterval(load, REFRESH_MS);
    return () => {
      clearTimeout(first);
      clearInterval(id);
    };
  }, [load]);

  const view = useMemo(() => {
    if (!series) return null;
    const totals = emptyTotals();
    const byGroup = new Map<string, Totals>();
    for (const b of series.buckets) {
      addBucket(totals, b);
      const g = groupName(b, dim);
      if (!byGroup.has(g)) byGroup.set(g, emptyTotals());
      addBucket(byGroup.get(g)!, b);
    }

    // The seven largest groups keep a colour; the rest fold into Other.
    // Colours follow the group, not its rank: call kinds use a fixed order,
    // other dimensions are coloured alphabetically among the kept groups.
    const ranked = [...byGroup.entries()].sort((a, b) => (b[1].input + b[1].output) - (a[1].input + a[1].output));
    const kept = new Set(ranked.slice(0, SLOTS.length).map(([g]) => g));
    const colourOrder = dim === 'kind'
      ? [...kept].sort((a, b) => {
        const ia = KIND_ORDER.indexOf(a), ib = KIND_ORDER.indexOf(b);
        return (ia < 0 ? 99 : ia) - (ib < 0 ? 99 : ib) || a.localeCompare(b);
      })
      : [...kept].sort((a, b) => a.localeCompare(b));
    const colour = new Map<string, string>();
    colourOrder.forEach((g, i) => colour.set(g, SLOTS[i]));
    const folded = (g: string) => (kept.has(g) ? g : OTHER);

    // Time axis: every bucket in the window, empty ones included, so gaps read as gaps.
    const startAligned = Math.floor(series.from / series.bucketMs) * series.bucketMs;
    const starts: number[] = [];
    for (let t = startAligned; t < series.to; t += series.bucketMs) starts.push(t);
    const index = new Map(starts.map((t, i) => [t, i]));

    const stacks = new Map<string, number[]>();
    for (const g of [...colourOrder, ...(ranked.length > kept.size ? [OTHER] : [])]) {
      stacks.set(g, new Array(starts.length).fill(0));
    }
    for (const b of series.buckets) {
      const i = index.get(b.start);
      if (i == null) continue;
      stacks.get(folded(groupName(b, dim)))![i] += b.inputTokens + b.outputTokens;
    }

    const datasetOrder = [...stacks.keys()];
    const datasets = datasetOrder.map((g, di) => ({
      label: g,
      data: stacks.get(g)!,
      backgroundColor: colour.get(g) ?? OTHER_COLOR,
      borderColor: SURFACE,
      borderWidth: { top: di === datasetOrder.length - 1 ? 0 : 2 },
      borderSkipped: 'bottom' as const,
      borderRadius: di === datasetOrder.length - 1 ? { topLeft: 4, topRight: 4 } : 0,
      maxBarThickness: 28,
    }));

    const rows = ranked.map(([g, t]) => ({ group: g, colour: colour.get(g) ?? OTHER_COLOR, ...t }));
    return { totals, rows, labels: starts.map(t => bucketLabel(t, series.bucketMs)), datasets };
  }, [series, dim]);

  const total = view ? view.totals.input + view.totals.output : 0;

  return (
    <div css={pageStyle}>
      <header css={headerStyle}>
        <div>
          <h1 css={titleStyle}>Token Usage</h1>
          <p css={subtitleStyle}>
            Tokens reported by LLM spans in ingested traces. Cost is estimated from public list prices, not billed spend.
          </p>
        </div>
        <button css={refreshStyle} onClick={load} disabled={loading} title="Refresh">
          <RefreshCw size={14} css={loading ? spinStyle : undefined} /> Refresh
        </button>
      </header>

      <div css={filterRowStyle}>
        <div css={segmentStyle} role="group" aria-label="Time range">
          {RANGES.map(r => (
            <button key={r.hours} css={[segBtnStyle, hours === r.hours && segActiveStyle]} onClick={() => setHours(r.hours)}>
              {r.label}
            </button>
          ))}
        </div>
        <div css={segmentStyle} role="group" aria-label="Group by">
          <span css={segLabelStyle}>Group by</span>
          {DIMENSIONS.map(d => (
            <button key={d.key} css={[segBtnStyle, dim === d.key && segActiveStyle]} onClick={() => setDim(d.key)}>
              {d.label}
            </button>
          ))}
        </div>
      </div>

      {error && <div css={errorStyle}>{error}</div>}

      {view && (
        <>
          <div css={tilesStyle}>
            <Tile label="Total tokens" value={formatTokens(total)}
              sub={`${formatTokens(view.totals.input)} in · ${formatTokens(view.totals.output)} out`} />
            <Tile label="Estimated cost" value={formatCostUsd(view.totals.cost)}
              sub={view.totals.unpricedTokens > 0 ? `${formatTokens(view.totals.unpricedTokens)} tokens unpriced` : 'all models priced'} />
            <Tile label="LLM calls" value={view.totals.calls.toLocaleString()}
              sub={view.totals.calls ? `${formatTokens(Math.round(total / view.totals.calls))} tokens per call` : '–'} />
            <Tile label="Avg latency" value={view.totals.calls ? formatMs(view.totals.durationMs / view.totals.calls) : '–'}
              sub="per LLM call" />
            <Tile label="Failed calls" value={view.totals.errors.toLocaleString()}
              sub={view.totals.calls ? `${((view.totals.errors / view.totals.calls) * 100).toFixed(1)}% of calls` : '–'} />
          </div>

          <section css={cardStyle}>
            <h2 css={cardTitleStyle}>Tokens per {series!.bucketMs >= 24 * 3600 * 1000 ? 'day' : 'hour'}, by {DIMENSIONS.find(d => d.key === dim)!.label.toLowerCase()}</h2>
            {view.totals.calls === 0 ? (
              <p css={emptyStyle}>No LLM calls with token counts in this window.</p>
            ) : (
              <div css={chartBoxStyle}>
                <Bar
                  data={{ labels: view.labels, datasets: view.datasets }}
                  options={{
                    responsive: true,
                    maintainAspectRatio: false,
                    animation: false,
                    interaction: { mode: 'index', intersect: false },
                    plugins: {
                      legend: {
                        position: 'top',
                        align: 'start',
                        labels: { color: '#a8b2c1', boxWidth: 12, boxHeight: 12, useBorderRadius: true, borderRadius: 2 },
                      },
                      tooltip: {
                        filter: item => (item.raw as number) > 0,
                        itemSort: (a, b) => (b.raw as number) - (a.raw as number),
                        callbacks: {
                          label: item => ` ${item.dataset.label}: ${formatTokens(item.raw as number)}`,
                          footer: items => `Total: ${formatTokens(items.reduce((s, i) => s + (i.raw as number), 0))}`,
                        },
                      },
                    },
                    scales: {
                      x: {
                        stacked: true,
                        grid: { display: false },
                        ticks: { color: '#6b7586', maxRotation: 0, autoSkipPadding: 16 },
                        border: { color: '#3a4052' },
                      },
                      y: {
                        stacked: true,
                        beginAtZero: true,
                        grid: { color: 'rgba(58, 64, 82, 0.5)' },
                        border: { display: false },
                        ticks: { color: '#6b7586', callback: v => formatTokens(Number(v)) },
                      },
                    },
                  }}
                />
              </div>
            )}
          </section>

          <section css={cardStyle}>
            <h2 css={cardTitleStyle}>Breakdown by {DIMENSIONS.find(d => d.key === dim)!.label.toLowerCase()}</h2>
            <div css={tableWrapStyle}>
              <table css={tableStyle}>
                <thead>
                  <tr>
                    <th>{DIMENSIONS.find(d => d.key === dim)!.label}</th>
                    <th css={numStyle}>Calls</th>
                    <th css={numStyle}>Input</th>
                    <th css={numStyle}>Output</th>
                    <th css={numStyle}>Total</th>
                    <th>Share</th>
                    <th css={numStyle}>Est. cost</th>
                    <th css={numStyle}>Tokens / call</th>
                    <th css={numStyle}>Avg latency</th>
                    <th css={numStyle}>Failed</th>
                  </tr>
                </thead>
                <tbody>
                  {view.rows.map(r => {
                    const t = r.input + r.output;
                    const share = total ? t / total : 0;
                    return (
                      <tr key={r.group}>
                        <td><span css={swatchStyle} style={{ background: r.colour }} />{r.group}</td>
                        <td css={numStyle}>{r.calls.toLocaleString()}</td>
                        <td css={numStyle}>{formatTokens(r.input)}</td>
                        <td css={numStyle}>{formatTokens(r.output)}</td>
                        <td css={numStyle}>{formatTokens(t)}</td>
                        <td>
                          <div css={shareStyle}>
                            <div css={shareTrackStyle}><div css={shareFillStyle} style={{ width: `${share * 100}%`, background: r.colour }} /></div>
                            <span>{(share * 100).toFixed(1)}%</span>
                          </div>
                        </td>
                        <td css={numStyle}>{r.unpricedTokens === t ? '–' : formatCostUsd(r.cost)}</td>
                        <td css={numStyle}>{r.calls ? formatTokens(Math.round(t / r.calls)) : '–'}</td>
                        <td css={numStyle}>{r.calls ? formatMs(r.durationMs / r.calls) : '–'}</td>
                        <td css={numStyle}>{r.errors || '–'}</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          </section>

          <section css={cardStyle}>
            <h2 css={cardTitleStyle}>Largest calls by tokens</h2>
            {calls.length === 0 ? (
              <p css={emptyStyle}>No calls in this window.</p>
            ) : (
              <div css={tableWrapStyle}>
                <table css={tableStyle}>
                  <thead>
                    <tr>
                      <th>Time</th>
                      <th>Kind</th>
                      <th>Tenant</th>
                      <th>Model</th>
                      <th css={numStyle}>Input</th>
                      <th css={numStyle}>Output</th>
                      <th css={numStyle}>Est. cost</th>
                      <th css={numStyle}>Latency</th>
                      <th>Session</th>
                    </tr>
                  </thead>
                  <tbody>
                    {calls.map(c => {
                      const cost = estimateCostUsd(c.model, c.inputTokens, c.outputTokens);
                      return (
                        <tr key={c.spanId}>
                          <td css={monoStyle}>{new Date(c.start).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false })}</td>
                          <td>{c.kind || NONE}{c.isError && <span css={failTagStyle}>failed</span>}</td>
                          <td>{c.tenant || NONE}</td>
                          <td css={monoStyle}>{c.model}</td>
                          <td css={numStyle}>{formatTokens(c.inputTokens)}</td>
                          <td css={numStyle}>{formatTokens(c.outputTokens)}</td>
                          <td css={numStyle}>{cost == null ? '–' : formatCostUsd(cost)}</td>
                          <td css={numStyle}>{formatMs(c.durationMs)}</td>
                          <td css={monoStyle} title={c.sessionId}>{c.sessionId}</td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            )}
          </section>
        </>
      )}
    </div>
  );
};

const Tile: React.FC<{ label: string; value: string; sub: string }> = ({ label, value, sub }) => (
  <div css={tileStyle}>
    <div css={tileLabelStyle}>{label}</div>
    <div css={tileValueStyle}>{value}</div>
    <div css={tileSubStyle}>{sub}</div>
  </div>
);

const pageStyle = css`
  padding: 32px 40px 48px;
  max-width: 1400px;
  font-family: var(--font-display);
  color: var(--text-primary);
`;

const headerStyle = css`
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: 16px;
  margin-bottom: 20px;
`;

const titleStyle = css`
  font-size: 1.6rem;
  font-weight: 600;
  margin: 0 0 4px;
`;

const subtitleStyle = css`
  margin: 0;
  color: var(--text-secondary);
  font-size: 0.875rem;
`;

const refreshStyle = css`
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 6px 12px;
  border-radius: 6px;
  border: 1px solid var(--border-default);
  background: transparent;
  color: var(--text-secondary);
  font-family: var(--font-display);
  cursor: pointer;
  &:hover { color: var(--text-primary); border-color: var(--accent-primary); }
  &:disabled { opacity: 0.6; cursor: default; }
`;

const spinStyle = css`
  animation: usage-spin 1s linear infinite;
  @keyframes usage-spin { to { transform: rotate(360deg); } }
`;

const filterRowStyle = css`
  display: flex;
  flex-wrap: wrap;
  gap: 16px;
  margin-bottom: 20px;
`;

const segmentStyle = css`
  display: flex;
  align-items: center;
  gap: 2px;
  padding: 3px;
  border: 1px solid var(--border-default);
  border-radius: 8px;
  background: var(--bg-surface);
`;

const segLabelStyle = css`
  font-size: 0.75rem;
  color: var(--text-tertiary);
  padding: 0 8px 0 6px;
`;

const segBtnStyle = css`
  border: none;
  background: transparent;
  color: var(--text-secondary);
  font-family: var(--font-display);
  font-size: 0.8rem;
  padding: 5px 12px;
  border-radius: 6px;
  cursor: pointer;
  &:hover { color: var(--text-primary); background: var(--bg-elevated); }
`;

const segActiveStyle = css`
  color: var(--text-primary);
  background: var(--bg-elevated);
  box-shadow: inset 0 0 0 1px var(--accent-primary);
`;

const errorStyle = css`
  padding: 12px 16px;
  border: 1px solid var(--status-failure);
  border-radius: 8px;
  color: var(--text-primary);
  margin-bottom: 20px;
`;

const tilesStyle = css`
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
  gap: 12px;
  margin-bottom: 20px;
`;

const tileStyle = css`
  background: var(--card-bg);
  border: 1px solid var(--border-default);
  border-radius: 10px;
  padding: 14px 16px;
`;

const tileLabelStyle = css`
  font-size: 0.75rem;
  color: var(--text-secondary);
  text-transform: uppercase;
  letter-spacing: 0.04em;
`;

const tileValueStyle = css`
  font-family: var(--font-mono);
  font-size: 1.6rem;
  font-weight: 500;
  margin: 6px 0 2px;
`;

const tileSubStyle = css`
  font-size: 0.75rem;
  color: var(--text-tertiary);
`;

const cardStyle = css`
  background: var(--card-bg);
  border: 1px solid var(--border-default);
  border-radius: 10px;
  padding: 16px 18px;
  margin-bottom: 20px;
`;

const cardTitleStyle = css`
  font-size: 0.95rem;
  font-weight: 600;
  margin: 0 0 12px;
`;

const chartBoxStyle = css`
  height: 320px;
  position: relative;
`;

const emptyStyle = css`
  color: var(--text-tertiary);
  font-size: 0.875rem;
  margin: 8px 0;
`;

const tableWrapStyle = css`
  overflow-x: auto;
`;

const tableStyle = css`
  width: 100%;
  border-collapse: collapse;
  font-size: 0.82rem;
  th {
    text-align: left;
    font-weight: 600;
    font-size: 0.72rem;
    color: var(--text-tertiary);
    text-transform: uppercase;
    letter-spacing: 0.03em;
    padding: 8px 10px;
    border-bottom: 1px solid var(--border-default);
    white-space: nowrap;
  }
  td {
    padding: 8px 10px;
    border-bottom: 1px solid rgba(58, 64, 82, 0.5);
    white-space: nowrap;
  }
  tbody tr:hover { background: var(--bg-elevated); }
  tbody tr:last-child td { border-bottom: none; }
`;

const numStyle = css`
  text-align: right !important;
  font-family: var(--font-mono);
  font-variant-numeric: tabular-nums;
`;

const monoStyle = css`
  font-family: var(--font-mono);
  font-size: 0.78rem;
  color: var(--text-secondary);
  max-width: 260px;
  overflow: hidden;
  text-overflow: ellipsis;
`;

const swatchStyle = css`
  display: inline-block;
  width: 10px;
  height: 10px;
  border-radius: 2px;
  margin-right: 8px;
  vertical-align: baseline;
`;

const shareStyle = css`
  display: flex;
  align-items: center;
  gap: 8px;
  font-family: var(--font-mono);
  font-size: 0.75rem;
  color: var(--text-secondary);
`;

const shareTrackStyle = css`
  width: 120px;
  height: 6px;
  border-radius: 3px;
  background: var(--bg-elevated);
  overflow: hidden;
`;

const shareFillStyle = css`
  height: 100%;
  border-radius: 3px;
`;

const failTagStyle = css`
  margin-left: 6px;
  font-size: 0.7rem;
  padding: 1px 6px;
  border-radius: 4px;
  border: 1px solid var(--status-failure);
  color: var(--text-primary);
`;

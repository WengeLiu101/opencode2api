// Page: 代理池 — 分组管理、扫描进度、逐条剔除。
//
// The pool is one payload from /api/pool: every proxy with its health, latency,
// tier and the sweep snapshot. Grouping and filtering are computed here, so
// switching tabs or narrowing to a group never issues a request.
import { api } from "../api.js";
import { $, el, toast, emptyRow, formatNumber } from "../ui.js";
import { t } from "../i18n.js";

const GROUP_CHECK_CAP = 200; // per-action probe cap: a group scan must not burst
const TIER_NAMES = ["快", "中", "慢", "未知"];
const DIMENSIONS = [
  ["source", "档位"],
  ["status", "状态"],
  ["tier", "分级"],
  ["region", "地区"],
];

let rows = [];
let groupDim = "source";
let filter = null; // {dim, key}
let pollTimer = null;

function groupKey(p, dim) {
  if (dim === "source") return p.fallback ? "fallback" : "primary";
  if (dim === "status") return p.retired ? "retired" : p.healthy ? "healthy" : "unhealthy";
  if (dim === "tier") return String(p.tier ?? 3);
  return (p.region || "").toUpperCase() || "??";
}

function groupLabel(key, dim) {
  if (dim === "source") return key === "fallback" ? "备用档 · 付费桥接" : "主档 · 免费池";
  if (dim === "status") return { healthy: "健康", unhealthy: "不通", retired: "短期排除" }[key] || key;
  if (dim === "tier") return `分级 · ${TIER_NAMES[Number(key)] ?? key}`;
  if (key === "??") return "地区 · 未知";
  if (key === "LOCAL") return "地区 · 本地桥接";
  return `地区 · ${key}`;
}

function groups(dim) {
  const map = new Map();
  for (const p of rows) {
    const key = groupKey(p, dim);
    let g = map.get(key);
    if (!g) map.set(key, (g = { key, dim, rows: [], healthy: 0, retired: 0, latency: 0, measured: 0 }));
    g.rows.push(p);
    if (p.retired) g.retired++;
    else if (p.healthy) g.healthy++;
    if (!p.retired && p.latency_ms >= 0) {
      g.latency += p.latency_ms;
      g.measured++;
    }
  }
  return [...map.values()].sort((a, b) => b.rows.length - a.rows.length || String(a.key).localeCompare(String(b.key)));
}

function fmtDuration(ms) {
  const total = Math.round((ms || 0) / 1000);
  if (total < 60) return `${total}s`;
  return `${Math.floor(total / 60)}m${String(total % 60).padStart(2, "0")}s`;
}

function schedulePoll(active) {
  if (pollTimer) {
    clearTimeout(pollTimer);
    pollTimer = null;
  }
  if (active) pollTimer = setTimeout(load, 1500);
}

function renderSweep(sw) {
  const state = $("#pool-sweep-state");
  const fill = $("#pool-sweep-fill");
  const running = !!sw.running;
  $("#pool-sweep-stop").classList.toggle("hidden", !running);
  $("#pool-sweep").disabled = running;
  if (running) {
    const pct = sw.total ? Math.round((sw.done / sw.total) * 100) : 0;
    fill.style.width = `${pct}%`;
    state.textContent =
      `扫描中 ${sw.done}/${sw.total}（${pct}%）· 通过 ${sw.passed} · 失败 ${sw.failed}` +
      ` · 新剔除 ${sw.retired} · 恢复 ${sw.restored} · 已用 ${fmtDuration(sw.duration_ms)}`;
  } else {
    fill.style.width = "0";
    if (sw.total) {
      const when = (sw.finished_at || "").replace("T", " ").slice(0, 19);
      state.textContent =
        `上次扫描 ${when} · 探测 ${sw.total} · 通过 ${sw.passed} · 失败 ${sw.failed}` +
        ` · 新剔除 ${sw.retired} · 恢复 ${sw.restored} · 用时 ${fmtDuration(sw.duration_ms)}` +
        (sw.done < sw.total ? `（中途停止，只扫到 ${sw.done}）` : "");
    } else {
      state.textContent = "尚未扫描";
    }
  }
  schedulePoll(running);
}

function renderGroups() {
  const box = $("#pool-groups");
  if (!box) return;
  box.replaceChildren();
  const list = groups(groupDim);
  if (!list.length) {
    box.append(el("div", { class: "empty", text: "池内还没有代理。" }));
  }
  for (const g of list) {
    const selected = !!(filter && filter.dim === g.dim && filter.key === g.key);
    const allRetired = g.retired > 0 && g.retired === g.rows.length;
    const live = g.rows.length - g.retired;
    const rate = live ? Math.round((g.healthy / live) * 100) : 0;
    const avg = g.measured ? `${Math.round(g.latency / g.measured)} ms` : "—";

    const card = el("div", { class: `pool-group${selected ? " active" : ""}` }, [
      el("div", { class: "pool-group-head" }, [
        el("span", { class: "pool-group-name", text: groupLabel(g.key, g.dim), title: groupLabel(g.key, g.dim) }),
        el("span", { class: "pool-group-count", text: formatNumber(g.rows.length) }),
      ]),
      el("div", {
        class: "pool-group-meta",
        text: `健康 ${g.healthy}/${live}（${rate}%）· 平均 ${avg}` + (g.retired ? ` · 短期排除 ${g.retired}` : ""),
      }),
      el("div", { class: "pool-group-ops" }, [
        el("button", {
          type: "button", class: "btn btn-sm btn-ghost", text: selected ? "取消筛选" : "筛选",
          onclick: () => {
            filter = selected ? null : { dim: g.dim, key: g.key };
            renderGroups();
            renderRows();
          },
        }),
        el("button", {
          type: "button", class: "btn btn-sm btn-ghost", text: "扫描本组",
          title: `定点探测本组最多 ${GROUP_CHECK_CAP} 个（含短期排除项，通了的会自动恢复）`,
          onclick: () => checkGroup(g),
        }),
        el("button", {
          type: "button", class: "btn btn-sm btn-ghost", text: allRetired ? "整组恢复" : "整组剔除",
          onclick: () => retireGroup(g, !allRetired),
        }),
      ]),
    ]);
    box.append(card);
  }

  const chip = $("#pool-filter-chip");
  chip.replaceChildren();
  chip.classList.toggle("hidden", !filter);
  if (filter) {
    chip.append(
      el("span", { text: `已筛选：${groupLabel(filter.key, filter.dim)}` }),
      el("button", {
        type: "button", class: "btn btn-sm btn-ghost", text: "✕", title: "清除筛选",
        onclick: () => {
          filter = null;
          renderGroups();
          renderRows();
        },
      }),
    );
  }
  $("#pool-filter-chip").classList.toggle("hidden", !filter);
}

function renderRows() {
  const body = $("#pool-rows");
  if (!body) return;
  body.replaceChildren();
  const q = ($("#pool-search").value || "").toLowerCase();
  const list = rows.filter((p) => {
    if (filter && groupKey(p, filter.dim) !== filter.key) return false;
    if (!q) return true;
    return (
      p.name.toLowerCase().includes(q) ||
      (p.region || "").toLowerCase().includes(q) ||
      (p.fallback ? "备用" : "主档").includes(q)
    );
  });
  for (const p of list.slice(0, 400)) {
    const tr = el("tr", {}, [
      el("td", {
        class: "truncate",
        text: p.fallback ? "备用 · " + p.name : p.name,
        title: p.fallback ? "备用 · " + p.name : p.name,
      }),
      el("td", { text: p.fallback ? "备用" : "主" }),
      el("td", { text: p.fallback ? "本地桥接" : p.region || "—" }),
      el("td", { text: p.latency_ms >= 0 ? `${p.latency_ms} ms` : "—" }),
      el("td", { text: TIER_NAMES[p.tier] ?? "—" }),
      el("td", { text: p.retired ? "短期排除" : p.healthy ? "健康" : "不通" }),
      el("td", {
        text: String(p.failures ?? 0) + (p.rejections ? ` / 拒${p.rejections}` : ""),
      }),
      el("td", { text: (p.last_probe || "—").replace("T", " ").slice(0, 19) }),
    ]);
    const action = el("td", {}, [
      el("button", {
        type: "button", class: "btn btn-sm btn-ghost", text: p.retired ? "恢复" : "剔除",
        onclick: () => retire([p.id], !p.retired),
      }),
    ]);
    tr.append(action);
    body.append(tr);
  }
  if (!list.length) {
    body.append(emptyRow(9, filter ? "该分组下没有代理。" : "没有匹配的代理。"));
  }
}

async function checkGroup(g) {
  const ids = g.rows.slice(0, GROUP_CHECK_CAP).map((p) => p.id);
  if (!ids.length) {
    toast("该组没有可扫描的代理", true);
    return;
  }
  try {
    const result = await api("/api/pool/check", { method: "POST", body: JSON.stringify({ ids }) });
    const skipped = g.rows.length - ids.length;
    toast(`已定点扫描 ${result.checked} 个${skipped > 0 ? `（超出上限，${skipped} 个未扫）` : ""}`);
    load();
  } catch (error) {
    toast(error.message, true);
  }
}

async function retire(ids, retired) {
  try {
    const result = await api("/api/pool/retire", { method: "POST", body: JSON.stringify({ ids, retired }) });
    toast(`${retired ? "已短期排除" : "已恢复"} ${result.changed} 个`);
    load();
  } catch (error) {
    toast(error.message, true);
  }
}

function retireGroup(g, retired) {
  return retire(g.rows.map((p) => p.id), retired);
}

async function load() {
  try {
    const data = await api("/api/pool");
    const s = data.summary || {};
    $("#pool-total").textContent = formatNumber(s.total ?? 0);
    $("#pool-healthy").textContent = formatNumber(s.healthy ?? 0);
    $("#pool-fast").textContent = formatNumber(s.fast ?? 0);
    $("#pool-retired").textContent = formatNumber(s.retired ?? 0);
    const retiredNote = $("#pool-retired-note");
    if (retiredNote) {
      retiredNote.textContent = (s.purged ?? 0) > 0
        ? `待复活 · 已永久删除 ${formatNumber(s.purged)}`
        : "待复活，超期永久删除";
    }
    const regions = Object.entries(s.regions || {})
      .sort((a, b) => b[1] - a[1])
      .slice(0, 8)
      .map(([k, v]) => `${k} ${v}`)
      .join("　");
    $("#pool-summary-note").textContent =
      `分级：快 ${s.fast || 0} · 中 ${s.mid || 0} · 慢 ${s.slow || 0} · 未知 ${s.unknown || 0}　|　地区：${regions || "—"}`;
    rows = data.proxies || [];
    renderSweep(data.sweep || {});
    renderGroups();
    renderRows();
  } catch (error) {
    toast(error.message, true);
  }
}

export default {
  id: "pool",
  title: "代理池",
  sub: "分组管理、扫描进度与逐条剔除。",
  init() {
    $("#pool-sweep-fill").style.width = "0";
    $("#pool-refresh").addEventListener("click", load);
    $("#pool-import-btn").addEventListener("click", async () => {
      const content = $("#pool-import-text").value.trim();
      if (!content) return toast("请先粘贴代理列表", true);
      const mode = $("#pool-import-mode").value;
      if (mode === "replace" && !confirm("替换将重写整个主档代理文件（旧文件会自动备份为 .import.bak）。继续？")) return;
      const button = $("#pool-import-btn");
      button.disabled = true;
      try {
        const result = await api("/api/pool/import", {
          method: "POST",
          body: JSON.stringify({ content, mode }),
        });
        toast(
          `导入 ${formatNumber(result.imported)} 条` +
            (result.duplicates ? ` · 去重 ${result.duplicates}` : "") +
            (result.invalid ? ` · 无效 ${result.invalid}` : "") +
            (result.reloaded ? "，已热生效" : "")
        );
        $("#pool-import-text").value = "";
        load();
      } catch (error) {
        toast(error.message || "导入失败", true);
      } finally {
        button.disabled = false;
      }
    });
    $("#pool-search").addEventListener("input", renderRows);
    $("#pool-sweep").addEventListener("click", async () => {
      try {
        const result = await api("/api/pool/sweep", { method: "POST", body: "{}" });
        if (result.started) {
          toast("已开始全量扫描（含短期排除节点，可自动恢复）");
          load();
        } else {
          toast("已有扫描在进行中", true);
        }
      } catch (error) {
        toast(error.message, true);
      }
    });
    $("#pool-sweep-stop").addEventListener("click", async () => {
      try {
        const result = await api("/api/pool/sweep/cancel", { method: "POST", body: "{}" });
        toast(result.stopping ? "正在停止…" : "当前没有扫描在跑", !result.stopping);
      } catch (error) {
        toast(error.message, true);
      }
    });
    $("#pool-reset").addEventListener("click", async () => {
      try {
        const result = await api("/api/pool/reset", { method: "POST", body: "{}" });
        toast(`已解除 ${result.restored} 个剔除`);
        load();
      } catch (error) {
        toast(error.message, true);
      }
    });
    for (const [dim, label] of DIMENSIONS) {
      const button = el("button", {
        type: "button", class: `btn btn-sm ${dim === "source" ? "btn-primary" : "btn-ghost"}`,
        text: label, "data-dim": dim,
        onclick: () => {
          groupDim = dim;
          filter = null;
          for (const node of $("#pool-group-dims").children) {
            const on = node.dataset.dim === dim;
            node.classList.toggle("btn-primary", on);
            node.classList.toggle("btn-ghost", !on);
          }
          renderGroups();
          renderRows();
        },
      });
      $("#pool-group-dims").append(button);
    }
  },
  show() {
    load();
  },
  hide() {
    schedulePoll(false);
  },
  reset() {
    schedulePoll(false);
    rows = [];
    filter = null;
  },
};

import { useQuery } from "@tanstack/react-query";
import { createColumnHelper, type ColumnDef } from "@tanstack/react-table";
import { AlertTriangle, ArrowDown, ArrowUp, Eye, Info, Plus, RefreshCw, Trash2, X } from "lucide-react";
import { useEffect, useMemo, useState, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { useFieldArray, type UseFormReturn } from "react-hook-form";
import { Badge } from "../../components/ui/Badge";
import { Button } from "../../components/ui/Button";
import { Card } from "../../components/ui/Card";
import { DataTable } from "../../components/ui/DataTable";
import { OffsetPagination } from "../../components/ui/OffsetPagination";
import { Select } from "../../components/ui/Select";
import { Textarea } from "../../components/ui/Textarea";
import { ToastContainer } from "../../components/ui/Toast";
import { useToast } from "../../hooks/useToast";
import { useI18n } from "../../i18n";
import { formatApiErrorMessage } from "../../lib/error-message";
import { formatDateTime } from "../../lib/time";
import { getPlatformPriorityTierViews, previewPlatformPriorityTiers } from "./api";
import {
  defaultPriorityTierFormValue,
  toPlatformPriorityTierPreviewSpec,
  type PlatformFormValues,
} from "./formModel";
import type {
  PlatformPriorityTierPreviewNode,
  PlatformPriorityTierViewKind,
  PlatformPriorityTierViewSummary,
  PreviewPlatformPriorityTiersInput,
} from "./types";

type PriorityTierEditorProps = {
  form: UseFormReturn<PlatformFormValues>;
  hasUnsavedChanges?: boolean;
  platformId?: string;
};

type TierDisplaySummary = {
  tier_key: string;
  kind: PlatformPriorityTierViewKind;
  label: string;
  node_count: number;
  tier_index?: number;
};

type DrawerSource = "draft" | "saved";

type DrawerContent = {
  title: string;
  hint: string;
  request: PreviewPlatformPriorityTiersInput;
};

type DrawerState = {
  draft: DrawerContent;
  saved?: DrawerContent;
  overall: boolean;
  draftInvalidFallback?: boolean;
};

type TierDisplaySource =
  | TierDisplaySummary
  | Pick<PlatformPriorityTierPreviewNode, "tier_key" | "tier_kind" | "tier_label" | "tier_index">;

type TierFilterOption = {
  value: string;
  label: string;
};

const PAGE_SIZE_OPTIONS = [10, 20, 50, 100] as const;
const PREVIEW_FIELD_NAMES: Array<keyof PlatformFormValues> = [
  "regex_filters_text",
  "exclude_regex_filters_text",
  "region_filters_text",
  "priority_tiers",
];
const previewNodeColumnHelper = createColumnHelper<PlatformPriorityTierPreviewNode>();

function displayTierLabel(summary: TierDisplaySource, t: (key: string, vars?: Record<string, unknown>) => string): string {
  const kind = "kind" in summary ? summary.kind : summary.tier_kind;
  const tierKey = summary.tier_key;
  const tierIndex = summary.tier_index;

  if (kind === "explicit") {
    const indexFromSummary = typeof tierIndex === "number" ? tierIndex : Number(tierKey);
    if (Number.isFinite(indexFromSummary) && indexFromSummary >= 0) {
      return t("优先级层 {{index}}", { index: indexFromSummary + 1 });
    }
  }
  if (kind === "fallback") {
    return t("兜底层（未命中显式层）");
  }
  if (kind === "platform_pool") {
    return t("整个平台候选池（未配置优先级层）");
  }
  return "label" in summary ? summary.label : summary.tier_label;
}

function displayTierHint(
  summary: TierDisplaySummary,
  t: (key: string) => string,
  source: DrawerSource,
): string {
  if (summary.kind === "explicit") {
    return source === "draft"
      ? t("当前草稿配置下，命中这个优先级层的候选节点。")
      : t("当前已生效配置下，命中这个优先级层的候选节点。");
  }
  if (summary.kind === "fallback") {
    return source === "draft"
      ? t("当前草稿配置下，未命中任何显式优先级层的兜底候选节点。")
      : t("当前已生效配置下，未命中任何显式优先级层的兜底候选节点。");
  }
  return source === "draft"
    ? t("当前草稿配置下，未配置优先级层时的平台全部候选节点。")
    : t("当前已生效配置下，未配置优先级层时的平台全部候选节点。");
}

function nodeDisplayName(node: PlatformPriorityTierPreviewNode): string {
  if (node.display_tag && node.display_tag.trim()) {
    return node.display_tag;
  }
  if (node.tags.length > 0) {
    return node.tags[0].tag;
  }
  return "-";
}

function nodeStatusLabel(node: PlatformPriorityTierPreviewNode, t: (key: string) => string): string {
  if (!node.enabled) {
    return t("禁用");
  }
  if (!node.has_outbound) {
    return t("错误");
  }
  if (node.circuit_open_since) {
    return t("熔断");
  }
  return t("健康");
}

function nodeStatusBadgeVariant(node: PlatformPriorityTierPreviewNode): "success" | "warning" | "danger" | "neutral" {
  if (!node.enabled) {
    return "neutral";
  }
  if (!node.has_outbound) {
    return "danger";
  }
  if (node.circuit_open_since) {
    return "warning";
  }
  return "success";
}

function formatRegion(node: PlatformPriorityTierPreviewNode): string {
  return node.region ? node.region.toUpperCase() : "-";
}

function formatLatency(node: PlatformPriorityTierPreviewNode): string {
  return typeof node.reference_latency_ms === "number" ? `${node.reference_latency_ms.toFixed(0)} ms` : "-";
}

function renderPortal(children: ReactNode): ReactNode {
  if (typeof document === "undefined") {
    return null;
  }
  return createPortal(children, document.body);
}

function buildExplicitTierSummary(index: number): TierDisplaySummary {
  return {
    tier_key: String(index),
    kind: "explicit",
    label: "",
    node_count: 0,
    tier_index: index,
  };
}

function buildSavedTierSummary(summary: TierDisplaySummary, savedTierKey: string): TierDisplaySummary {
  if (summary.kind !== "explicit") {
    return summary;
  }
  const savedIndex = Number(savedTierKey);
  return {
    ...summary,
    tier_key: savedTierKey,
    tier_index: Number.isFinite(savedIndex) && savedIndex >= 0 ? savedIndex : summary.tier_index,
  };
}

function buildDraftOverallTierOptions(
  fieldsCount: number,
  t: (key: string, vars?: Record<string, unknown>) => string,
): TierFilterOption[] {
  const options: TierFilterOption[] = [{ value: "all", label: t("全部") }];
  if (fieldsCount > 0) {
    for (let index = 0; index < fieldsCount; index += 1) {
      options.push({
        value: String(index),
        label: displayTierLabel(buildExplicitTierSummary(index), t),
      });
    }
    options.push({
      value: "fallback",
      label: displayTierLabel({ tier_key: "fallback", kind: "fallback", label: "", node_count: 0, tier_index: -1 }, t),
    });
    return options;
  }

  options.push({
    value: "platform_pool",
    label: displayTierLabel({ tier_key: "platform_pool", kind: "platform_pool", label: "", node_count: 0, tier_index: -1 }, t),
  });
  return options;
}

function buildSavedOverallTierOptions(
  summaries: PlatformPriorityTierViewSummary[],
  t: (key: string, vars?: Record<string, unknown>) => string,
): TierFilterOption[] {
  const options: TierFilterOption[] = [{ value: "all", label: t("全部") }];
  for (const summary of summaries) {
    options.push({
      value: summary.tier_key,
      label: displayTierLabel(
        {
          tier_key: summary.tier_key,
          kind: summary.kind,
          label: summary.label,
          node_count: summary.node_count,
          tier_index: summary.kind === "explicit" ? Number(summary.tier_key) : -1,
        },
        t,
      ),
    });
  }
  return options;
}

export function PriorityTierEditor({ form, hasUnsavedChanges = false, platformId = "" }: PriorityTierEditorProps) {
  const { t } = useI18n();
  const { toasts, showToast, dismissToast } = useToast();
  const {
    control,
    register,
    trigger,
    getValues,
    formState: { errors },
  } = form;
  const { fields, append, remove, move } = useFieldArray({
    control,
    name: "priority_tiers",
  });
  const topLevelError = errors.priority_tiers;

  const [drawerState, setDrawerState] = useState<DrawerState | null>(null);
  const [activeDrawerSource, setActiveDrawerSource] = useState<DrawerSource>("draft");
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [page, setPage] = useState(0);
  const [pageSize, setPageSize] = useState<number>(20);
  const [overallTierFilter, setOverallTierFilter] = useState<string>("all");
  const [savedTierKeyByFieldId, setSavedTierKeyByFieldId] = useState<Record<string, string>>({});

  const canInspectSavedViews = Boolean(platformId);
  const savedViewSummariesQuery = useQuery({
    queryKey: ["platform-tier-views", platformId],
    queryFn: () => getPlatformPriorityTierViews(platformId),
    enabled: canInspectSavedViews,
    placeholderData: (previous) => previous,
  });
  const savedViewSummaries = useMemo(
    () => savedViewSummariesQuery.data ?? [],
    [savedViewSummariesQuery.data],
  );
  const activeDrawerContent = drawerState
    ? activeDrawerSource === "saved"
      ? drawerState.saved ?? drawerState.draft
      : drawerState.draft
    : null;
  const activeDrawerRequest = activeDrawerContent
    ? {
        ...activeDrawerContent.request,
        ...(drawerState?.overall && overallTierFilter !== "all" ? { tier_key: overallTierFilter } : {}),
      }
    : null;

  const nodesQuery = useQuery({
    queryKey: ["platform-tier-preview", activeDrawerSource, activeDrawerRequest, page, pageSize],
    queryFn: () =>
      previewPlatformPriorityTiers({
        ...(activeDrawerRequest ?? {}),
        offset: page * pageSize,
        limit: pageSize,
      }),
    enabled: drawerOpen && activeDrawerRequest !== null,
  });

  const nodesPage = nodesQuery.data ?? {
    items: [] as PlatformPriorityTierPreviewNode[],
    total: 0,
    limit: pageSize,
    offset: page * pageSize,
    unique_egress_ips: 0,
    unique_healthy_egress_ips: 0,
  };
  const totalPages = Math.max(1, Math.ceil(nodesPage.total / pageSize));

  useEffect(() => {
    if (!drawerOpen) {
      return;
    }
    if (page > totalPages - 1) {
      queueMicrotask(() => {
        setPage(Math.max(0, totalPages - 1));
      });
    }
  }, [drawerOpen, page, totalPages]);

  useEffect(() => {
    if (!drawerState?.overall) {
      queueMicrotask(() => {
        setOverallTierFilter("all");
      });
      return;
    }
    const options = activeDrawerSource === "saved"
      ? buildSavedOverallTierOptions(savedViewSummaries, t)
      : buildDraftOverallTierOptions(fields.length, t);
    if (!options.some((option) => option.value === overallTierFilter)) {
      queueMicrotask(() => {
        setOverallTierFilter("all");
      });
    }
  }, [activeDrawerSource, drawerState?.overall, fields.length, overallTierFilter, savedViewSummaries, t]);

  useEffect(() => {
    if (!canInspectSavedViews) {
      queueMicrotask(() => {
        setSavedTierKeyByFieldId((previous) => (Object.keys(previous).length === 0 ? previous : {}));
      });
      return;
    }
    if (hasUnsavedChanges) {
      return;
    }
    const nextMap: Record<string, string> = {};
    for (let index = 0; index < fields.length; index += 1) {
      nextMap[fields[index].id] = String(index);
    }
    queueMicrotask(() => {
      setSavedTierKeyByFieldId((previous) => {
        const previousKeys = Object.keys(previous);
        const nextKeys = Object.keys(nextMap);
        if (previousKeys.length === nextKeys.length && nextKeys.every((key) => previous[key] === nextMap[key])) {
          return previous;
        }
        return nextMap;
      });
    });
  }, [canInspectSavedViews, fields, hasUnsavedChanges]);

  const columns = useMemo(() => {
    const result = [
      previewNodeColumnHelper.accessor((node) => nodeDisplayName(node), {
        id: "display_tag",
        header: () => t("节点名"),
        cell: (info) => (
          <div className="platform-tier-node-cell">
            <span title={info.getValue()}>{info.getValue()}</span>
            <small title={info.row.original.node_hash}>{info.row.original.node_hash}</small>
          </div>
        ),
      }),
      previewNodeColumnHelper.accessor((node) => node.egress_ip || "-", {
        id: "egress_ip",
        header: () => t("出口 IP"),
        cell: (info) => (
          <div className="platform-tier-egress-cell" title={info.getValue()}>
            {info.getValue()}
          </div>
        ),
      }),
      previewNodeColumnHelper.accessor((node) => formatRegion(node), {
        id: "region",
        header: () => t("区域"),
        cell: (info) => info.getValue(),
      }),
      previewNodeColumnHelper.display({
        id: "status",
        header: () => t("状态"),
        cell: (info) => (
          <Badge variant={nodeStatusBadgeVariant(info.row.original)}>
            {nodeStatusLabel(info.row.original, t)}
          </Badge>
        ),
      }),
      previewNodeColumnHelper.accessor((node) => formatLatency(node), {
        id: "latency",
        header: () => t("参考延迟"),
        cell: (info) => info.getValue(),
      }),
      previewNodeColumnHelper.accessor((node) => node.last_latency_probe_attempt || "", {
        id: "last_probe",
        header: () => t("上次探测"),
        cell: (info) => formatDateTime(info.getValue()),
      }),
    ];

    if (drawerState?.overall) {
      result.splice(1, 0, previewNodeColumnHelper.display({
        id: "tier_label",
        header: () => t("所属层级"),
        cell: (info) => displayTierLabel(info.row.original, t),
      }));
    }

    return result as ColumnDef<PlatformPriorityTierPreviewNode, unknown>[];
  }, [drawerState?.overall, t]);

  const closeDrawer = () => {
    setDrawerOpen(false);
    setDrawerState(null);
    setActiveDrawerSource("draft");
    setOverallTierFilter("all");
    setPage(0);
  };

  const refreshDrawer = async () => {
    try {
      if (activeDrawerSource === "draft") {
        const valid = await validateDraftPreview(false);
        if (!valid) {
          return;
        }
      }
      await nodesQuery.refetch();
    } catch (error) {
      showToast("error", formatApiErrorMessage(error, t));
    }
  };

  const validateDraftPreview = async (showError: boolean): Promise<boolean> => {
    const valid = await trigger(PREVIEW_FIELD_NAMES);
    if (!valid && showError) {
      showToast("error", t("请先修正平台筛选与优先级分层中的错误后再预览。"));
    }
    return valid;
  };

  const buildDraftRequest = (tierKey?: string): PreviewPlatformPriorityTiersInput => {
    const values = getValues();
    return {
      platform_spec: toPlatformPriorityTierPreviewSpec(values),
      ...(tierKey ? { tier_key: tierKey } : {}),
    };
  };

  const buildSavedHint = (baseHint: string): string => {
    if (!hasUnsavedChanges) {
      return baseHint;
    }
    return `${baseHint} ${t("当前显示的是已生效配置，不包含未保存修改。")}`;
  };

  const buildTierDrawerState = (summary: TierDisplaySummary, savedTierKey?: string): DrawerState => {
    const draft: DrawerContent = {
      title: displayTierLabel(summary, t),
      hint: displayTierHint(summary, t, "draft"),
      request: buildDraftRequest(summary.tier_key),
    };

    if (!canInspectSavedViews || !savedTierKey) {
      return { draft, overall: false };
    }

    const savedSummary = buildSavedTierSummary(summary, savedTierKey);
    return {
      draft,
      saved: {
        title: displayTierLabel(savedSummary, t),
        hint: buildSavedHint(displayTierHint(savedSummary, t, "saved")),
        request: {
          platform_id: platformId,
          tier_key: savedTierKey,
        },
      },
      overall: false,
    };
  };

  const buildOverallDrawerState = (): DrawerState => {
    const draft: DrawerContent = {
      title: t("整体预览"),
      hint: t("按优先级顺序展示当前草稿配置下的全部层级命中结果。"),
      request: buildDraftRequest(),
    };

    if (!canInspectSavedViews) {
      return { draft, overall: true };
    }

    return {
      draft,
      saved: {
        title: t("整体预览"),
        hint: buildSavedHint(t("按优先级顺序展示当前已生效配置下的全部层级命中结果。")),
        request: {
          platform_id: platformId,
        },
      },
      overall: true,
    };
  };

  const openTierPreview = async (summary: TierDisplaySummary, savedTierKey?: string) => {
    const nextState = buildTierDrawerState(summary, savedTierKey);
    const valid = await validateDraftPreview(false);
    if (!valid && !nextState.saved) {
      showToast("error", t("请先修正平台筛选与优先级分层中的错误后再预览。"));
      return;
    }
    setDrawerState({
      ...nextState,
      draftInvalidFallback: !valid,
    });
    setActiveDrawerSource(!valid && nextState.saved ? "saved" : "draft");
    setOverallTierFilter("all");
    setPage(0);
    setDrawerOpen(true);
  };

  const openOverallPreview = async () => {
    const nextState = buildOverallDrawerState();
    const valid = await validateDraftPreview(false);
    if (!valid && !nextState.saved) {
      showToast("error", t("请先修正平台筛选与优先级分层中的错误后再预览。"));
      return;
    }
    setDrawerState({
      ...nextState,
      draftInvalidFallback: !valid,
    });
    setActiveDrawerSource(!valid && nextState.saved ? "saved" : "draft");
    setOverallTierFilter("all");
    setPage(0);
    setDrawerOpen(true);
  };

  const activeOverallTierOptions = useMemo(() => {
    if (!drawerState?.overall) {
      return [] as TierFilterOption[];
    }
    return activeDrawerSource === "saved"
      ? buildSavedOverallTierOptions(savedViewSummaries, t)
      : buildDraftOverallTierOptions(fields.length, t);
  }, [activeDrawerSource, drawerState?.overall, fields.length, savedViewSummaries, t]);
  const effectiveOverallTierFilter =
    drawerState?.overall && activeOverallTierOptions.some((option) => option.value === overallTierFilter)
      ? overallTierFilter
      : "all";

  return (
    <>
      <ToastContainer toasts={toasts} onDismiss={dismissToast} />

      <div className="field-group field-span-2">
        <div className="platform-priority-tiers-header">
          <div>
            <label className="field-label field-label-with-info" style={{ marginBottom: 0 }}>
              <span>{t("平台内优先级分层（可选）")}</span>
              <span
                className="subscription-info-icon"
                title={t("只对已经被下方平台筛选规则选中的节点再次分层；路由时优先使用最前面的非空层。")}
                aria-label={t("只对已经被下方平台筛选规则选中的节点再次分层；路由时优先使用最前面的非空层。")}
                tabIndex={0}
              >
                <Info size={13} />
              </span>
            </label>
            <p className="muted" style={{ marginTop: 4, fontSize: 12 }}>
              {t("先筛出属于平台的候选节点，再按这里的顺序决定先用哪一层；未命中任何显式层的节点会自动落入隐式最后一层。")}
            </p>
          </div>
          <div className="platform-priority-tier-header-actions">
            <Button variant="secondary" size="sm" onClick={() => void openOverallPreview()}>
              <Eye size={16} />
              {t("整体预览")}
            </Button>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => append({ ...defaultPriorityTierFormValue })}
              disabled={fields.length >= 8}
            >
              <Plus size={16} />
              {t("新增一层")}
            </Button>
          </div>
        </div>

        {topLevelError && !Array.isArray(topLevelError) && "message" in topLevelError && topLevelError.message ? (
          <p className="field-error">{t(topLevelError.message)}</p>
        ) : null}

        {!fields.length ? (
          <div className="platform-priority-tier-empty">
            <p>{t("未配置优先级层时，将直接在整个平台候选集中按层内分配策略选点。")}</p>
          </div>
        ) : null}

        {fields.length ? (
          <div className="platform-priority-tier-list">
            {fields.map((field, index) => {
              const tierErrors = Array.isArray(errors.priority_tiers) ? errors.priority_tiers[index] : undefined;
              const regexError = tierErrors?.regex_filters_text?.message;
              const excludeError = tierErrors?.exclude_regex_filters_text?.message;
              const regionError = tierErrors?.region_filters_text?.message;
              const savedTierKey = savedTierKeyByFieldId[field.id];
              const draftSummary = buildExplicitTierSummary(index);

              return (
                <div key={field.id} className="platform-priority-tier-card">
                  <div className="platform-priority-tier-card-head">
                    <div>
                      <strong>{t("优先级层 {{index}}", { index: index + 1 })}</strong>
                      <p>{t("先命中的层先参与选点；节点命中多层时以最前面的层为准。")}</p>
                    </div>
                    <div className="platform-priority-tier-head-side">
                      <Button
                        variant="secondary"
                        size="sm"
                        onClick={() => void openTierPreview(draftSummary, savedTierKey)}
                      >
                        <Eye size={16} />
                        {t("预览节点")}
                      </Button>
                      <div className="platform-priority-tier-actions">
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={() => move(index, index - 1)}
                          disabled={index === 0}
                          aria-label={t("上移")}
                        >
                          <ArrowUp size={16} />
                        </Button>
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={() => move(index, index + 1)}
                          disabled={index === fields.length - 1}
                          aria-label={t("下移")}
                        >
                          <ArrowDown size={16} />
                        </Button>
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={() => remove(index)}
                          aria-label={t("删除")}
                        >
                          <Trash2 size={16} />
                        </Button>
                      </div>
                    </div>
                  </div>

                  <div className="platform-priority-tier-grid">
                    <div className="field-group">
                      <label className="field-label" htmlFor={`priority-tier-regex-${index}`}>
                        {t("包含正则")}
                      </label>
                      <Textarea
                        id={`priority-tier-regex-${index}`}
                        rows={3}
                        placeholder={t("每行一条，例如 .*(家宽|住宅).* 或 <订阅名>/.*")}
                        {...register(`priority_tiers.${index}.regex_filters_text`)}
                      />
                      {regexError ? <p className="field-error">{t(regexError)}</p> : null}
                    </div>

                    <div className="field-group">
                      <label className="field-label" htmlFor={`priority-tier-exclude-${index}`}>
                        {t("排除正则")}
                      </label>
                      <Textarea
                        id={`priority-tier-exclude-${index}`}
                        rows={3}
                        placeholder={t("每行一条，例如 .*专线.* 或 .*中转.*")}
                        {...register(`priority_tiers.${index}.exclude_regex_filters_text`)}
                      />
                      {excludeError ? <p className="field-error">{t(excludeError)}</p> : null}
                    </div>

                    <div className="field-group">
                      <label className="field-label" htmlFor={`priority-tier-region-${index}`}>
                        {t("地区规则")}
                      </label>
                      <Textarea
                        id={`priority-tier-region-${index}`}
                        rows={3}
                        placeholder={t("每行一条，如 hk / us / !hk")}
                        {...register(`priority_tiers.${index}.region_filters_text`)}
                      />
                      {regionError ? <p className="field-error">{t(regionError)}</p> : null}
                    </div>
                  </div>
                </div>
              );
            })}
          </div>
        ) : null}
      </div>

      {renderPortal(
        drawerOpen && drawerState && activeDrawerContent ? (
          <div
            className="drawer-overlay"
            role="dialog"
            aria-modal="true"
            aria-label={activeDrawerContent.title}
            onClick={closeDrawer}
          >
            <Card className="drawer-panel" onClick={(event) => event.stopPropagation()}>
              <div className="drawer-header">
                <div>
                  <h3>{activeDrawerContent.title}</h3>
                  <p>{activeDrawerContent.hint}</p>
                  {activeDrawerSource === "saved" && drawerState.draftInvalidFallback ? (
                    <p className="platform-preview-source-notice">
                      {t("草稿存在校验错误，当前已切换到已生效视图。")}
                    </p>
                  ) : null}
                  {canInspectSavedViews ? (
                    <div className="platform-preview-source-tabs" role="tablist" aria-label={t("预览来源切换")}>
                      <button
                        type="button"
                        role="tab"
                        aria-selected={activeDrawerSource === "draft"}
                        className={`platform-preview-source-tab ${activeDrawerSource === "draft" ? "platform-preview-source-tab-active" : ""}`}
                        onClick={async () => {
                          const valid = await validateDraftPreview(true);
                          if (!valid) {
                            return;
                          }
          setActiveDrawerSource("draft");
          setOverallTierFilter("all");
          setPage(0);
                        }}
                      >
                        {t("草稿")}
                      </button>
                      <button
                        type="button"
                        role="tab"
                        aria-selected={activeDrawerSource === "saved"}
                        className={`platform-preview-source-tab ${activeDrawerSource === "saved" ? "platform-preview-source-tab-active" : ""}`}
                        onClick={() => {
                          if (!drawerState.saved) {
                            return;
                          }
          setActiveDrawerSource("saved");
          setOverallTierFilter("all");
          setPage(0);
                        }}
                        disabled={!drawerState.saved}
                      >
                        {t("已生效")}
                      </button>
                    </div>
                  ) : null}
                </div>
                <div className="drawer-header-actions">
                  <Button
                    variant="secondary"
                    size="sm"
                    onClick={() => void refreshDrawer()}
                    disabled={nodesQuery.isFetching}
                  >
                    <RefreshCw size={16} className={nodesQuery.isFetching ? "spin" : undefined} />
                    {t("刷新")}
                  </Button>
                  <Button variant="ghost" size="sm" aria-label={t("关闭详情面板")} onClick={closeDrawer}>
                    <X size={16} />
                  </Button>
                </div>
              </div>

              <div className="platform-drawer-layout">
                <section className="platform-drawer-section">
                  <div className="platform-drawer-section-head platform-drawer-section-head-inline">
                    <div className="platform-drawer-section-head-copy">
                      <h4>{t("节点列表")}</h4>
                      <p>
                        {drawerState.overall
                          ? t("共 {{count}} 个节点，按优先级顺序展示完整命中结果。", { count: nodesPage.total })
                          : t("共 {{count}} 个节点，显示该分组当前完整命中结果。", { count: nodesPage.total })}
                      </p>
                    </div>

                    {drawerState.overall ? (
                      <label className="platform-drawer-filter" htmlFor="platform-tier-filter">
                        <span>{t("所属层级筛选")}</span>
                        <Select
                          id="platform-tier-filter"
                          value={effectiveOverallTierFilter}
                          onChange={(event) => {
                            setOverallTierFilter(event.target.value);
                            setPage(0);
                          }}
                        >
                          {activeOverallTierOptions.map((option) => (
                            <option key={option.value} value={option.value}>
                              {option.label}
                            </option>
                          ))}
                        </Select>
                      </label>
                    ) : null}
                  </div>

                  {nodesQuery.isLoading ? <p className="muted">{t("正在加载层级节点列表...")}</p> : null}

                  {nodesQuery.isError ? (
                    <div className="callout callout-error">
                      <AlertTriangle size={14} />
                      <span>{formatApiErrorMessage(nodesQuery.error, t)}</span>
                    </div>
                  ) : null}

                  {!nodesQuery.isLoading && !nodesPage.items.length ? (
                    <div className="empty-box">
                      <p>{t("该分组当前没有命中节点")}</p>
                    </div>
                  ) : null}

                  {nodesPage.items.length ? (
                    <>
                      <DataTable
                        data={nodesPage.items}
                        columns={columns}
                        getRowId={(node) => node.node_hash}
                        className={drawerState.overall ? "data-table-tier-nodes data-table-tier-nodes-overall" : "data-table-tier-nodes"}
                        wrapClassName="data-table-wrap-tier-nodes"
                      />
                      <OffsetPagination
                        page={page}
                        totalPages={totalPages}
                        totalItems={nodesPage.total}
                        pageSize={pageSize}
                        pageSizeOptions={PAGE_SIZE_OPTIONS}
                        onPageChange={setPage}
                        onPageSizeChange={(next) => {
                          setPageSize(next);
                          setPage(0);
                        }}
                      />
                    </>
                  ) : null}
                </section>
              </div>
            </Card>
          </div>
        ) : null,
      )}
    </>
  );
}

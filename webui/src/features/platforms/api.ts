import { apiRequest } from "../../lib/api-client";
import type { NodeSummary, PageResponse as NodePageResponse } from "../nodes/types";
import type {
  PageResponse,
  Platform,
  PlatformCreateInput,
  PlatformPriorityTier,
  PlatformPriorityTierPreviewNode,
  PreviewPlatformPriorityTiersInput,
  PlatformPriorityTierViewSummary,
  PlatformUpdateInput,
} from "./types";

const basePath = "/api/v1/platforms";

type ApiPlatform = Omit<Platform, "regex_filters" | "exclude_regex_filters" | "region_filters" | "priority_tiers"> & {
  regex_filters?: string[] | null;
  exclude_regex_filters?: string[] | null;
  region_filters?: string[] | null;
  priority_tiers?: Array<{
    regex_filters?: string[] | null;
    exclude_regex_filters?: string[] | null;
    region_filters?: string[] | null;
  }> | null;
  routable_node_count?: number | null;
  reverse_proxy_miss_action?: Platform["reverse_proxy_miss_action"] | null;
  reverse_proxy_empty_account_behavior?: Platform["reverse_proxy_empty_account_behavior"] | null;
  reverse_proxy_fixed_account_header?: string | null;
};

type ApiPriorityTier = NonNullable<ApiPlatform["priority_tiers"]>[number];
type ApiNodeSummary = Omit<NodeSummary, "tags"> & {
  tags?: NodeSummary["tags"] | null;
  enabled?: boolean | null;
  display_tag?: string | null;
  last_error?: string | null;
  circuit_open_since?: string | null;
  egress_ip?: string | null;
  reference_latency_ms?: number | null;
  region?: string | null;
  last_egress_update?: string | null;
  last_latency_probe_attempt?: string | null;
  last_authority_latency_probe_attempt?: string | null;
  last_egress_update_attempt?: string | null;
};

type ApiPriorityTierPreviewNode = ApiNodeSummary & {
  tier_key: string;
  tier_kind: PlatformPriorityTierPreviewNode["tier_kind"];
  tier_label?: string | null;
  tier_index: number;
};

function normalizeNode(raw: ApiNodeSummary): NodeSummary {
  const { reference_latency_ms, ...rest } = raw;
  const normalized: NodeSummary = {
    ...rest,
    enabled: raw.enabled !== false,
    display_tag: raw.display_tag || "",
    tags: Array.isArray(raw.tags) ? raw.tags : [],
    last_error: raw.last_error || "",
    circuit_open_since: raw.circuit_open_since || "",
    egress_ip: raw.egress_ip || "",
    region: raw.region || "",
    last_egress_update: raw.last_egress_update || "",
    last_latency_probe_attempt: raw.last_latency_probe_attempt || "",
    last_authority_latency_probe_attempt: raw.last_authority_latency_probe_attempt || "",
    last_egress_update_attempt: raw.last_egress_update_attempt || "",
  };

  if (typeof reference_latency_ms === "number") {
    normalized.reference_latency_ms = reference_latency_ms;
  }

  return normalized;
}

function normalizePriorityTierPreviewNode(raw: ApiPriorityTierPreviewNode): PlatformPriorityTierPreviewNode {
  return {
    ...normalizeNode(raw),
    tier_key: raw.tier_key,
    tier_kind: raw.tier_kind,
    tier_label: raw.tier_label || "",
    tier_index: raw.tier_index,
  };
}

function parseMissAction(raw: ApiPlatform["reverse_proxy_miss_action"]): Platform["reverse_proxy_miss_action"] {
  if (raw === "TREAT_AS_EMPTY" || raw === "REJECT") {
    return raw;
  }
  throw new Error(`invalid reverse_proxy_miss_action: ${String(raw)}`);
}

function normalizePriorityTier(raw: ApiPriorityTier): PlatformPriorityTier {
  return {
    regex_filters: Array.isArray(raw?.regex_filters) ? raw.regex_filters : [],
    exclude_regex_filters: Array.isArray(raw?.exclude_regex_filters) ? raw.exclude_regex_filters : [],
    region_filters: Array.isArray(raw?.region_filters) ? raw.region_filters : [],
  };
}

function normalizePlatform(raw: ApiPlatform): Platform {
  return {
    ...raw,
    reverse_proxy_miss_action: parseMissAction(raw.reverse_proxy_miss_action),
    regex_filters: Array.isArray(raw.regex_filters) ? raw.regex_filters : [],
    exclude_regex_filters: Array.isArray(raw.exclude_regex_filters) ? raw.exclude_regex_filters : [],
    region_filters: Array.isArray(raw.region_filters) ? raw.region_filters : [],
    priority_tiers: Array.isArray(raw.priority_tiers) ? raw.priority_tiers.map(normalizePriorityTier) : [],
    routable_node_count: typeof raw.routable_node_count === "number" ? raw.routable_node_count : 0,
    reverse_proxy_empty_account_behavior:
      raw.reverse_proxy_empty_account_behavior === "RANDOM" ||
      raw.reverse_proxy_empty_account_behavior === "FIXED_HEADER" ||
      raw.reverse_proxy_empty_account_behavior === "ACCOUNT_HEADER_RULE"
        ? raw.reverse_proxy_empty_account_behavior
        : "RANDOM",
    reverse_proxy_fixed_account_header:
      typeof raw.reverse_proxy_fixed_account_header === "string" ? raw.reverse_proxy_fixed_account_header : "",
  };
}

function normalizePlatformPage(raw: PageResponse<ApiPlatform>): PageResponse<Platform> {
  return {
    ...raw,
    items: raw.items.map(normalizePlatform),
  };
}

export type ListPlatformsPageInput = {
  limit?: number;
  offset?: number;
  keyword?: string;
};

export async function listPlatforms(input: ListPlatformsPageInput = {}): Promise<PageResponse<Platform>> {
  const query = new URLSearchParams({
    limit: String(input.limit ?? 50),
    offset: String(input.offset ?? 0),
    sort_by: "name",
    sort_order: "asc",
  });
  const keyword = input.keyword?.trim();
  if (keyword) {
    query.set("keyword", keyword);
  }

  const data = await apiRequest<PageResponse<ApiPlatform>>(`${basePath}?${query.toString()}`);
  return normalizePlatformPage(data);
}

export async function getPlatform(id: string): Promise<Platform> {
  const data = await apiRequest<ApiPlatform>(`${basePath}/${id}`);
  return normalizePlatform(data);
}

export async function createPlatform(input: PlatformCreateInput): Promise<Platform> {
  const data = await apiRequest<ApiPlatform>(basePath, {
    method: "POST",
    body: input,
  });
  return normalizePlatform(data);
}

export async function updatePlatform(id: string, input: PlatformUpdateInput): Promise<Platform> {
  const data = await apiRequest<ApiPlatform>(`${basePath}/${id}`, {
    method: "PATCH",
    body: input,
  });
  return normalizePlatform(data);
}

export async function deletePlatform(id: string): Promise<void> {
  await apiRequest<void>(`${basePath}/${id}`, {
    method: "DELETE",
  });
}

export async function resetPlatform(id: string): Promise<Platform> {
  const data = await apiRequest<ApiPlatform>(`${basePath}/${id}/actions/reset-to-default`, {
    method: "POST",
  });
  return normalizePlatform(data);
}

export async function rebuildPlatform(id: string): Promise<void> {
  await apiRequest<{ status: "ok" }>(`${basePath}/${id}/actions/rebuild-routable-view`, {
    method: "POST",
  });
}

export async function clearAllPlatformLeases(id: string): Promise<void> {
  await apiRequest<void>(`${basePath}/${id}/leases`, {
    method: "DELETE",
  });
}

export async function getPlatformPriorityTierViews(id: string): Promise<PlatformPriorityTierViewSummary[]> {
  return apiRequest<PlatformPriorityTierViewSummary[]>(`${basePath}/${id}/priority-tiers/views`);
}

export type ListPlatformPriorityTierNodesInput = {
  tierKey: string;
  limit?: number;
  offset?: number;
  sort_by?: "tag" | "created_at" | "failure_count" | "region";
  sort_order?: "asc" | "desc";
};

export async function listPlatformPriorityTierNodes(
  id: string,
  input: ListPlatformPriorityTierNodesInput,
): Promise<NodePageResponse<NodeSummary>> {
  const query = new URLSearchParams({
    limit: String(input.limit ?? 20),
    offset: String(input.offset ?? 0),
    sort_by: input.sort_by || "tag",
    sort_order: input.sort_order || "asc",
  });
  const data = await apiRequest<NodePageResponse<ApiNodeSummary>>(
    `${basePath}/${id}/priority-tiers/${encodeURIComponent(input.tierKey)}/nodes?${query.toString()}`,
  );
  return {
    ...data,
    items: data.items.map(normalizeNode),
  };
}

export async function previewPlatformPriorityTiers(
  input: PreviewPlatformPriorityTiersInput,
): Promise<NodePageResponse<PlatformPriorityTierPreviewNode>> {
  const query = new URLSearchParams({
    limit: String(input.limit ?? 20),
    offset: String(input.offset ?? 0),
    sort_by: input.sort_by || "tag",
    sort_order: input.sort_order || "asc",
  });

  const body: {
    platform_id?: string;
    platform_spec?: PreviewPlatformPriorityTiersInput["platform_spec"];
    tier_key?: string;
  } = {};
  if (input.platform_id) {
    body.platform_id = input.platform_id;
  }
  if (input.platform_spec) {
    body.platform_spec = input.platform_spec;
  }
  if (input.tier_key) {
    body.tier_key = input.tier_key;
  }

  const data = await apiRequest<NodePageResponse<ApiPriorityTierPreviewNode>>(
    `${basePath}/preview-priority-tiers?${query.toString()}`,
    {
      method: "POST",
      body,
    },
  );
  return {
    ...data,
    items: data.items.map(normalizePriorityTierPreviewNode),
  };
}

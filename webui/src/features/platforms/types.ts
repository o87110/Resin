export type PlatformMissAction = "TREAT_AS_EMPTY" | "REJECT";
export type PlatformEmptyAccountBehavior = "RANDOM" | "FIXED_HEADER" | "ACCOUNT_HEADER_RULE";
export type PlatformAllocationPolicy = "BALANCED" | "PREFER_LOW_LATENCY" | "PREFER_IDLE_IP";

export type PlatformPriorityTier = {
  regex_filters: string[];
  exclude_regex_filters: string[];
  region_filters: string[];
};

export type PlatformPriorityTierViewKind = "explicit" | "fallback" | "platform_pool";

export type PlatformPriorityTierViewSummary = {
  tier_key: string;
  label: string;
  kind: PlatformPriorityTierViewKind;
  node_count: number;
};

export type PlatformPriorityTierPreviewSpec = {
  regex_filters: string[];
  exclude_regex_filters: string[];
  region_filters: string[];
  priority_tiers: PlatformPriorityTier[];
};

export type PlatformPriorityTierPreviewNode = {
  node_hash: string;
  created_at: string;
  enabled: boolean;
  display_tag?: string;
  has_outbound: boolean;
  last_error?: string;
  circuit_open_since?: string;
  failure_count: number;
  egress_ip?: string;
  reference_latency_ms?: number;
  region?: string;
  last_egress_update?: string;
  last_latency_probe_attempt?: string;
  last_authority_latency_probe_attempt?: string;
  last_egress_update_attempt?: string;
  tags: Array<{
    subscription_id: string;
    subscription_name: string;
    tag: string;
  }>;
  tier_key: string;
  tier_kind: PlatformPriorityTierViewKind;
  tier_label: string;
  tier_index: number;
};

export type Platform = {
  id: string;
  name: string;
  sticky_ttl: string;
  regex_filters: string[];
  exclude_regex_filters: string[];
  region_filters: string[];
  priority_tiers: PlatformPriorityTier[];
  routable_node_count: number;
  reverse_proxy_miss_action: PlatformMissAction;
  reverse_proxy_empty_account_behavior: PlatformEmptyAccountBehavior;
  reverse_proxy_fixed_account_header: string;
  allocation_policy: PlatformAllocationPolicy;
  updated_at: string;
};

export type PageResponse<T> = {
  items: T[];
  total: number;
  limit: number;
  offset: number;
};

export type PlatformCreateInput = {
  name: string;
  sticky_ttl?: string;
  regex_filters?: string[];
  exclude_regex_filters?: string[];
  region_filters?: string[];
  priority_tiers?: PlatformPriorityTier[];
  reverse_proxy_miss_action?: PlatformMissAction;
  reverse_proxy_empty_account_behavior?: PlatformEmptyAccountBehavior;
  reverse_proxy_fixed_account_header?: string;
  allocation_policy?: PlatformAllocationPolicy;
};

export type PlatformUpdateInput = {
  name?: string;
  sticky_ttl?: string;
  regex_filters?: string[];
  exclude_regex_filters?: string[];
  region_filters?: string[];
  priority_tiers?: PlatformPriorityTier[];
  reverse_proxy_miss_action?: PlatformMissAction;
  reverse_proxy_empty_account_behavior?: PlatformEmptyAccountBehavior;
  reverse_proxy_fixed_account_header?: string;
  allocation_policy?: PlatformAllocationPolicy;
};

export type PreviewPlatformPriorityTiersInput = {
  platform_id?: string;
  platform_spec?: PlatformPriorityTierPreviewSpec;
  tier_key?: string;
  limit?: number;
  offset?: number;
  sort_by?: "tag" | "created_at" | "failure_count" | "region";
  sort_order?: "asc" | "desc";
};

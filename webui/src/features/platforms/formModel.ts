import { z } from "zod";
import { allocationPolicies, emptyAccountBehaviors, missActions } from "./constants";
import { parseHeaderLines, parseLinesToList } from "./formParsers";
import type {
  Platform,
  PlatformCreateInput,
  PlatformPriorityTier,
  PlatformPriorityTierPreviewSpec,
  PlatformUpdateInput,
} from "./types";

const platformNameForbiddenChars = ".:|/\\@?#%~";
const platformNameForbiddenSpacing = " \t\r\n";
const platformNameReserved = "api";

function containsAny(source: string, chars: string): boolean {
  for (const ch of chars) {
    if (source.includes(ch)) {
      return true;
    }
  }
  return false;
}

export const platformNameRuleHint = "平台名不能包含 .:|/\\@?#%~、空格、Tab、换行、回车，也不能为保留字。";

const priorityTierSchema = z.object({
  regex_filters_text: z.string().optional(),
  exclude_regex_filters_text: z.string().optional(),
  region_filters_text: z.string().optional(),
}).superRefine((value, ctx) => {
  const regexFilters = parseLinesToList(value.regex_filters_text);
  const excludeRegexFilters = parseLinesToList(value.exclude_regex_filters_text);
  const regionFilters = parseLinesToList(value.region_filters_text, (item) => item.toLowerCase());

  if (regexFilters.length === 0 && excludeRegexFilters.length === 0 && regionFilters.length === 0) {
    ctx.addIssue({
      code: "custom",
      path: ["regex_filters_text"],
      message: "每个优先级层至少要填写一条包含、排除或地区规则",
    });
  }

  for (const region of regionFilters) {
    if (!/^!?[a-z]{2}$/.test(region)) {
      ctx.addIssue({
        code: "custom",
        path: ["region_filters_text"],
        message: "地区规则必须是两位小写国家码，可选 ! 前缀",
      });
      break;
    }
  }
});

export type PlatformPriorityTierFormValue = z.infer<typeof priorityTierSchema>;

export const defaultPriorityTierFormValue: PlatformPriorityTierFormValue = {
  regex_filters_text: "",
  exclude_regex_filters_text: "",
  region_filters_text: "",
};

export const platformFormSchema = z.object({
  name: z.string().trim()
    .min(1, "平台名称不能为空")
    .refine((value) => !containsAny(value, platformNameForbiddenChars), {
      message: "平台名称不能包含字符 .:|/\\@?#%~",
    })
    .refine((value) => !containsAny(value, platformNameForbiddenSpacing), {
      message: "平台名称不能包含空格、Tab、换行、回车",
    })
    .refine((value) => value.toLowerCase() !== platformNameReserved, {
      message: "平台名称不能为保留字",
    }),
  sticky_ttl: z.string().optional(),
  regex_filters_text: z.string().optional(),
  exclude_regex_filters_text: z.string().optional(),
  region_filters_text: z.string().optional(),
  priority_tiers: z.array(priorityTierSchema).max(8, "节点优先级层最多 8 层"),
  reverse_proxy_miss_action: z.enum(missActions),
  reverse_proxy_empty_account_behavior: z.enum(emptyAccountBehaviors),
  reverse_proxy_fixed_account_header: z.string().optional(),
  allocation_policy: z.enum(allocationPolicies),
}).superRefine((value, ctx) => {
  if (
    value.reverse_proxy_empty_account_behavior === "FIXED_HEADER" &&
    parseHeaderLines(value.reverse_proxy_fixed_account_header).length === 0
  ) {
    ctx.addIssue({
      code: "custom",
      path: ["reverse_proxy_fixed_account_header"],
      message: "用于提取 Account 的 Headers 不能为空",
    });
  }
});

export type PlatformFormValues = z.infer<typeof platformFormSchema>;

export const defaultPlatformFormValues: PlatformFormValues = {
  name: "",
  sticky_ttl: "",
  regex_filters_text: "",
  exclude_regex_filters_text: "",
  region_filters_text: "",
  priority_tiers: [],
  reverse_proxy_miss_action: "TREAT_AS_EMPTY",
  reverse_proxy_empty_account_behavior: "RANDOM",
  reverse_proxy_fixed_account_header: "Authorization",
  allocation_policy: "BALANCED",
};

export function platformToFormValues(platform: Platform): PlatformFormValues {
  const regexFilters = Array.isArray(platform.regex_filters) ? platform.regex_filters : [];
  const excludeRegexFilters = Array.isArray(platform.exclude_regex_filters) ? platform.exclude_regex_filters : [];
  const regionFilters = Array.isArray(platform.region_filters) ? platform.region_filters : [];
  const priorityTiers = Array.isArray(platform.priority_tiers) ? platform.priority_tiers : [];

  return {
    name: platform.name,
    sticky_ttl: platform.sticky_ttl,
    regex_filters_text: regexFilters.join("\n"),
    exclude_regex_filters_text: excludeRegexFilters.join("\n"),
    region_filters_text: regionFilters.join("\n"),
    priority_tiers: priorityTiers.map(priorityTierToFormValue),
    reverse_proxy_miss_action: platform.reverse_proxy_miss_action,
    reverse_proxy_empty_account_behavior: platform.reverse_proxy_empty_account_behavior,
    reverse_proxy_fixed_account_header: platform.reverse_proxy_fixed_account_header,
    allocation_policy: platform.allocation_policy,
  };
}

function toPlatformPayloadBase(values: PlatformFormValues) {
  return {
    name: values.name.trim(),
    regex_filters: parseLinesToList(values.regex_filters_text),
    exclude_regex_filters: parseLinesToList(values.exclude_regex_filters_text),
    region_filters: parseLinesToList(values.region_filters_text, (value) => value.toLowerCase()),
    priority_tiers: values.priority_tiers.map(priorityTierToPayload),
    reverse_proxy_miss_action: values.reverse_proxy_miss_action,
    reverse_proxy_empty_account_behavior: values.reverse_proxy_empty_account_behavior,
    reverse_proxy_fixed_account_header: parseHeaderLines(values.reverse_proxy_fixed_account_header).join("\n"),
    allocation_policy: values.allocation_policy,
  };
}

export function toPlatformCreateInput(values: PlatformFormValues): PlatformCreateInput {
  return {
    ...toPlatformPayloadBase(values),
    sticky_ttl: values.sticky_ttl?.trim() || undefined,
  };
}

export function toPlatformUpdateInput(values: PlatformFormValues): PlatformUpdateInput {
  return {
    ...toPlatformPayloadBase(values),
    sticky_ttl: values.sticky_ttl?.trim() || "",
  };
}

export function toPlatformPriorityTierPreviewSpec(values: PlatformFormValues): PlatformPriorityTierPreviewSpec {
  const payload = toPlatformPayloadBase(values);
  return {
    regex_filters: payload.regex_filters,
    exclude_regex_filters: payload.exclude_regex_filters,
    region_filters: payload.region_filters,
    priority_tiers: payload.priority_tiers,
  };
}

function priorityTierToFormValue(tier: PlatformPriorityTier): PlatformPriorityTierFormValue {
  return {
    regex_filters_text: Array.isArray(tier.regex_filters) ? tier.regex_filters.join("\n") : "",
    exclude_regex_filters_text: Array.isArray(tier.exclude_regex_filters) ? tier.exclude_regex_filters.join("\n") : "",
    region_filters_text: Array.isArray(tier.region_filters) ? tier.region_filters.join("\n") : "",
  };
}

function priorityTierToPayload(tier: PlatformPriorityTierFormValue): PlatformPriorityTier {
  return {
    regex_filters: parseLinesToList(tier.regex_filters_text),
    exclude_regex_filters: parseLinesToList(tier.exclude_regex_filters_text),
    region_filters: parseLinesToList(tier.region_filters_text, (value) => value.toLowerCase()),
  };
}

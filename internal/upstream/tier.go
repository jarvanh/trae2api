// tier.go SOLO 请求档位（__dev/__max）解析与请求体改写。
// 2026-10-26 实测（memory/2026-10-26.md:136-147）：solo_agent 通道下
// 930K 输入 + model=glm-5.3__max + extra_info.v2_max_mode_enabled=true → 200，
// HEAD/TAIL 双针正确召回（1M 总窗口实证）；裸名或仅开关都被 dev 档 4022 拒。
// 2026-10-30 直连矩阵补测：config_name 必须保持裸名 —— 带 __max 后缀上游直接
// 4001 param is invalid（config_name 是目录身份，档位后缀只允许出现在 model
// 字段），故本文件只改写 model，config_name 交由 PrepareBody 归一化（恒裸名）。
package upstream

import (
	"encoding/json"
	"strings"
)

// ModelTier 请求命中的上游档位。
type ModelTier int

const (
	// TierDev dev 档：裸 config_name 的默认档（glm-5.3 = 200K/168K/32K）。
	TierDev ModelTier = iota
	// TierMax max 档：model=<model>__max + max 模式开关（glm-5.3 = 1M/936K/64K；
	// config_name 保持裸名，2026-10-30 矩阵实测）。
	TierMax
)

// MaxTierSuffix / DevTierSuffix 上游 model_detail_list 里的档位名后缀。
const (
	MaxTierSuffix = "__max"
	DevTierSuffix = "__dev"
)

// maxModeExtraInfoKey / maxModeField 启用 max 模式的 extra_info 字段。
// 字段别名经 dsh-connect-trae / pi-connect / cli2api 同类项目互证，
// 并由 2026-10-26 实测确认（大输入下仅加开关不加 __max 仍被 dev 档 4022 拒）。
const (
	maxModeExtraInfoKey = "extra_info"
	maxModeField        = "v2_max_mode_enabled"
)

// MaxTierModelName 拼 config 的 max 档模型名：<config>__max（已是 __max 结尾则原样）。
func MaxTierModelName(config string) string {
	if strings.HasSuffix(config, MaxTierSuffix) {
		return config
	}
	return config + MaxTierSuffix
}

// SetMaxTierMode 把请求体改写为 max 档：
//
//	model → <model>__max（config_name 不动，由 PrepareBody 归一化为裸名）
//	extra_info.v2_max_mode_enabled = true
//
// 2026-10-30 直连矩阵实测：config_name 带 __max 后缀 → 上游 4001 param is
// invalid；config_name 裸名 + model=__max → 200。大输入下 model=__max 与开关
// 缺一都被 dev 档 4022 拒（10-26 实测），故两者成对注入。model 缺省时依次回退
// config_name、DefaultConfigName（与 PrepareBody 的缺省口径一致）。
// 无法解析时原样返回。
func SetMaxTierMode(src []byte) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	maxName := ""
	if cur, ok := obj["model"].(string); ok && strings.TrimSpace(cur) != "" {
		maxName = MaxTierModelName(strings.TrimSpace(cur))
	} else if cur, ok := obj["config_name"].(string); ok && strings.TrimSpace(cur) != "" {
		maxName = MaxTierModelName(strings.TrimSpace(cur))
	}
	if maxName == "" {
		maxName = MaxTierModelName(DefaultConfigName)
	}
	obj["model"] = maxName

	extra, _ := obj[maxModeExtraInfoKey].(map[string]any)
	if extra == nil {
		extra = map[string]any{}
	}
	extra[maxModeField] = true
	obj[maxModeExtraInfoKey] = extra

	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// StripMaxTierMode 撤销 SetMaxTierMode：model 去掉 __max 后缀（config_name
// 一并剥后缀兜底，正常本就是裸名），删除 extra_info.v2_max_mode_enabled。
// SOLO→Work 回退路径用 —— Work 通道没有档位概念，bridge 模式透传原始 body，
// 改写残留可能被桥接端误消费。无法解析时原样返回。
func StripMaxTierMode(src []byte) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	if s, ok := obj["config_name"].(string); ok {
		obj["config_name"] = strings.TrimSuffix(s, MaxTierSuffix)
	}
	if s, ok := obj["model"].(string); ok {
		obj["model"] = strings.TrimSuffix(s, MaxTierSuffix)
	}
	if extra, ok := obj[maxModeExtraInfoKey].(map[string]any); ok {
		delete(extra, maxModeField)
		if len(extra) == 0 {
			delete(obj, maxModeExtraInfoKey)
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

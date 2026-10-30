// tier.go SOLO 请求档位（__dev/__max）解析与请求体改写。
// 2026-10-26 实测（memory/2026-10-26.md:136-147）：solo_agent 通道下
// 930K 输入 + model=glm-5.3__max + extra_info.v2_max_mode_enabled=true → 200，
// HEAD/TAIL 双针正确召回（1M 总窗口实证）。
// 裸名或仅 extra_info 开关都被 dev 档 4022 prompt tokens exceeded 拒，
// 因此改写时两者必须成对注入 —— 见 SetMaxTierMode。
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
	// TierMax max 档：config_name=<model>__max + max 模式开关（glm-5.3 = 1M/936K/64K）。
	TierMax
)

// MaxTierSuffix / DevTierSuffix 上游 model_detail_list 里的档位名后缀。
const (
	MaxTierSuffix = "__max"
	DevTierSuffix = "__dev"
)

// maxModeExtraInfoKey / maxModeField 启用 max 模式的 extra_info 字段。
// 字段别名经 dsh-connect-trae / pi-connect / cli2api 同类项目互证，
// 并由 2026-10-26 实测确认（仅加开关不加 __max 仍被 dev 档 4022 拒）。
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
//	model + config_name → <config>__max
//	extra_info.v2_max_mode_enabled = true
//
// 与 10-26 实测成功样本逐字段对齐：只改 model 不加开关、或只加开关不改名，
// 都会被 dev 档 4022 拒。config_name/model 都缺省时按 DefaultConfigName 兜底
// （与 PrepareBody 的缺省口径一致）。无法解析时原样返回。
func SetMaxTierMode(src []byte) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	maxName := ""
	if cur, ok := obj["config_name"].(string); ok && strings.TrimSpace(cur) != "" {
		maxName = MaxTierModelName(strings.TrimSpace(cur))
	} else if cur, ok := obj["model"].(string); ok && strings.TrimSpace(cur) != "" {
		maxName = MaxTierModelName(strings.TrimSpace(cur))
	}
	if maxName == "" {
		maxName = MaxTierModelName(DefaultConfigName)
	}
	obj["config_name"] = maxName
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

// StripMaxTierMode 撤销 SetMaxTierMode：model/config_name 去掉 __max 后缀，
// 删除 extra_info.v2_max_mode_enabled。SOLO→Work 回退路径用 —— Work 通道
// 没有档位概念，bridge 模式透传原始 body，改写残留可能被桥接端误消费。
// 无法解析时原样返回。
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

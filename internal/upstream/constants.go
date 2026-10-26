// constants.go SOLO 上游技术常量（SPEC §1，来自实测，禁止改动）。
package upstream

const (
	AgentHost      = "https://trae-api-cn.mchost.guru"
	UgHost         = "https://api.trae.cn"
	OAuthHost      = "https://api.trae.com.cn"
	ConsoleHost    = "https://www.trae.cn"
	ClientID       = "en1oxy7wnw8j9n" // SOLO stable
	AppID          = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	IdeVersion     = "0.1.52"
	IdeVersionCode = "20260811"
	DeviceBrand    = "Apple"
	OSVersion      = "macOS 15.7.4"
	// Function 默认上游 function。
	//
	// 实测（2026-10-26）两者能力差异很大：
	//   solo_work_lite —— 48 config，无 max 档，glm-5.3 只有 200K/168K/32K
	//   solo_agent     —— 69 config，含 max 档，glm-5.3 = 1M/936K/64K
	//   chat_v3        —— 59 config，dev 档被压到 116K/100K/16K
	// 可用 config.upstream.function / TW2A_FUNCTION 覆盖回 solo_work_lite。
	Function = "solo_agent"
	// FunctionWorkLite 免费引流通道（无 max 档），保留作回滚取值。
	FunctionWorkLite = "solo_work_lite"

	// 端点
	EpChat          = "/api/agent/v3/llm_utils_chat"
	EpModels        = "/api/ide/v1/get_detail_param"
	EpExchange      = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	EpUserInfo      = "/cloudide/api/v3/trae/GetUserInfo"
	EpCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	EpCheckinClaim  = "/trae/api/v2/ug/checkin_credits/claim"
	EpEntUsage      = "/trae/api/v2/pay/ide_user_ent_usage"
)

// checkin_device.go — 签到路径专用设备号派生与极简请求头。
//
// 背景（2026-09-29 实测）：TRAE 签到接口对 X-Device-Id 做风控，auth 文件里
// 批量合成的 deviceId 被标记后恒定返回 9074「当前参与用户太多」，错峰重试
// 三轮全败；换成由 uid 派生的设备号后，连败 32 / 37 的两个钉子户首发即中。
//
// 机制来源：autumnsentiment/Trae2api-cn → 240xu/trae2api-more（MIT），
// 派生式 sha256(uid[#genN]) % 1e16，9074 命中后换代轮换 + 退避持久化。
//
// 作用域（重要）：仅签到 status/claim。对话（SOLOHeaders）与积分查询
// （UgHeaders）仍用 auth 文件里的原始 deviceId / machineId —— 聊天路径的
// 设备身份一个字节都不动。

package upstream

import (
	"crypto/sha256"
	"fmt"
	"math/big"
	"net/http"
	"strconv"

	"trae2api/internal/auth"
)

// CheckinBusyCode 签到业务码「当前参与用户太多」：被风控的是设备号，
// 不是账号也不是真拥堵 —— 换代换设备号即可破，硬重试只会空转。
const CheckinBusyCode = 9074

// CheckinIdentity 返回派生设备号用的身份串（优先 UID，回退 DeviceID）。
func CheckinIdentity(a *auth.Auth) string {
	if a == nil {
		return ""
	}
	if a.UID != "" {
		return a.UID
	}
	return a.DeviceID
}

// CheckinDeviceID 返回账号稳定的签到设备 ID（16 位数字）。
// generation 为 9074 限流后的轮换代数（0 = 基线），代数只增不减。
//
// 交叉验证向量（与 Python 版一致）：CheckinDeviceID("u1", 0) == "4302850041909017"。
func CheckinDeviceID(identity string, generation int) string {
	if identity == "" {
		return ""
	}
	material := identity
	if generation > 0 {
		material = identity + "#gen" + strconv.Itoa(generation)
	}
	sum := sha256.Sum256([]byte(material))
	n := new(big.Int).SetBytes(sum[:])
	n.Mod(n, big.NewInt(1e16))
	return fmt.Sprintf("%016d", n)
}

// CheckinHeaders 设置签到 status / claim 的极简头：Authorization + 派生设备号。
//
// 刻意不带 UA / X-User-Region / X-Machine-Id —— 与实证成功的请求形态对齐。
// 去掉 X-Machine-Id 是本次相对旧 UgHeaders 的关键差异。
func CheckinHeaders(req *http.Request, a *auth.Auth, deviceID string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+a.JWT()) // 读锁快照
	if deviceID != "" {
		req.Header.Set("X-Device-Id", deviceID)
	} else if a.DeviceID != "" {
		req.Header.Set("X-Device-Id", a.DeviceID)
	}
}

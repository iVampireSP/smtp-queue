package selector

import (
	"math/rand"

	"github.com/ivampiresp/smtp-queue/config"
)

// SelectWithFailover 按加权随机选出主服务器，其余作为备选，返回有序列表
// 仅包含 enabled: true 的服务器
func SelectWithFailover(servers []config.SMTPServer) []config.SMTPServer {
	var enabled []config.SMTPServer
	for _, s := range servers {
		if s.Enabled {
			enabled = append(enabled, s)
		}
	}

	if len(enabled) == 0 {
		return nil
	}

	if len(enabled) == 1 {
		return enabled
	}

	// 计算总权重
	totalWeight := 0
	for _, s := range enabled {
		w := s.Weight
		if w <= 0 {
			w = 1
		}
		totalWeight += w
	}

	// 加权随机选择主服务器
	r := rand.Intn(totalWeight)
	primaryIdx := 0
	cumulative := 0
	for i, s := range enabled {
		w := s.Weight
		if w <= 0 {
			w = 1
		}
		cumulative += w
		if r < cumulative {
			primaryIdx = i
			break
		}
	}

	// 主服务器排第一，其余按原始顺序排列
	result := make([]config.SMTPServer, 0, len(enabled))
	result = append(result, enabled[primaryIdx])
	for i, s := range enabled {
		if i != primaryIdx {
			result = append(result, s)
		}
	}

	return result
}

package acp

import "sync/atomic"

// 关键稳定性指标（T12，docs/cases/06 §5）。
//
// 进程内累计计数，经 /api/ping 暴露，用于把「网关抖动 / run 超时」从靠用户截图
// 变为可监控。三个计数对应三类已知的不稳定来源：
//   - gatewaySocketClosed       : gatewayRPCError 命中 OPENCLAW_GATEWAY_SOCKET_CLOSED（连接断）
//   - taskGetUnconfirmedFallback: tasks.get 走持久 run 仓兜底（gateway 无法确认 run，T7）
//   - runDeadlineInterrupt      : run 超过 DeadlineAt 且 gateway 无法确认，回 interrupted（T9）
var bridgeStabilityMetrics struct {
	gatewaySocketClosed        atomic.Int64
	taskGetUnconfirmedFallback atomic.Int64
	runDeadlineInterrupt       atomic.Int64
}

func metricGatewaySocketClosedInc()        { bridgeStabilityMetrics.gatewaySocketClosed.Add(1) }
func metricTaskGetUnconfirmedFallbackInc() { bridgeStabilityMetrics.taskGetUnconfirmedFallback.Add(1) }
func metricRunDeadlineInterruptInc()       { bridgeStabilityMetrics.runDeadlineInterrupt.Add(1) }

// bridgeStabilityMetricsSnapshot 返回当前计数快照，供 /api/ping 输出。
func bridgeStabilityMetricsSnapshot() map[string]any {
	return map[string]any{
		"gatewaySocketClosed":        bridgeStabilityMetrics.gatewaySocketClosed.Load(),
		"taskGetUnconfirmedFallback": bridgeStabilityMetrics.taskGetUnconfirmedFallback.Load(),
		"runDeadlineInterrupt":       bridgeStabilityMetrics.runDeadlineInterrupt.Load(),
	}
}

// truncation.go 工具调用的残缺参数检测（吸收参考仓库 sse.ts:158-167
// isTruncatedArguments 语义）。
//
// 背景：SSE 流被截断（连接中断 / finish_reason==length）时，工具调用的 arguments
// 会只剩半截 JSON。此时网关若把脏参数原样交给客户端，客户端解析会报非法 JSON 并卡死会话。
// 参考仓库的处置是丢弃残缺调用并触发重试（报告 max-tokens），而非补成 {} 伪造合法外观。
//
// 关键区分：只把「非空但无法解析」视为截断。空串是合法的无参数工具；能解析但类型不对
// （标量 / 数组）属于模型输出错误，交给客户端 schema 校验回传即可，不在此判定。
package upstream

import (
	"encoding/json"
	"strings"
)

// isTruncatedArguments 判定工具参数字符串是否因分片丢失而残缺（区别于「该工具本就无参数」）。
//   - 空串 / 纯空白 → false（合法无参工具）；
//   - 非空但 JSON 解析失败 → true（截断）；
//   - 能解析（含 null/标量/数组等任何合法 JSON）→ false。
func isTruncatedArguments(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	var v any
	return json.Unmarshal([]byte(trimmed), &v) != nil
}

// dropTruncatedToolCalls 过滤出 arguments 完整的 tool_call（返回新 slice）。
// 只依据 isTruncatedArguments 判定，不改动任何保留的调用（正例零改动）。
func dropTruncatedToolCalls(calls []map[string]any) []map[string]any {
	kept := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		fn, _ := call["function"].(map[string]any)
		if fn == nil {
			kept = append(kept, call)
			continue
		}
		args, _ := fn["arguments"].(string)
		if isTruncatedArguments(args) {
			continue
		}
		kept = append(kept, call)
	}
	return kept
}

// TruncatedKey 聚合响应里的截断标记键（网关内部约定，非 OpenAI 字段）。
//
// 背景：Aggregate 无法用 finish_reason 表达「流被掐断」——它的默认值就是 "stop"，
// 与「模型真说完」不可区分（OpenAI 协议本身也没有 incomplete 语义）。但兼容层
// （/v1/messages、/v1/responses）必须能区分，否则上游中途断流会被它们译成
// end_turn / completed，客户端把半截正文当最终答案。
//
// 故 Aggregate 在「上游 EOF 收尾但没给 finish_reason」时置此键为 true，由消费者
// **读取并删除**（见 TakeTruncated）——原生 /v1/chat/completions 路径删掉后逐字节
// 不变，兼容层删掉前先取走这个事实。
const TruncatedKey = "wb2api_stream_truncated"

// TakeTruncated 读取并移除截断标记（幂等：无标记返回 false）。
//
// 刻意用「取走」而非「只读」：标记绝不能出现在任何对外响应里（原生 OpenAI 客户端
// 看到未知顶层键属于协议污染）。一处取走即可保证干净透出。
func TakeTruncated(resp map[string]any) bool {
	v, ok := resp[TruncatedKey]
	if !ok {
		return false
	}
	delete(resp, TruncatedKey)
	b, _ := v.(bool)
	return b
}

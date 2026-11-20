package server

import (
	"bytes"
	"encoding/json"
)

// 响应体的 model 回填。
//
// OpenAI 的响应体里 `model` 字段是「谁真正回答了这次请求」。网关做过模型
// 改写（别名 / 兼容别名 / 强制模型）之后，如果还回填客户端请求里的名字，
// 就等于把「你被换成了别的模型」这件事藏起来 —— 用户看到 model=gpt-4o，
// 实际扣的是 GLM 的积分。因此这里统一回填实际执行的模型 ID。

// modelFieldKey 是 JSON 里 model 字段的起始字节序列，用于快速判断是否需要改写。
//
// 绝大多数流式分片不含 model 字段（只有首片与 usage 片带），
// 先用 bytes.Contains 过一道，避免每个分片都做一次 JSON 解析。
var modelFieldKey = []byte(`"model":"`)

// withExecutedModel 把响应体（或一个 SSE 分片）里的 model 回填为实际执行的模型。
//
// 解析失败或没有 model 字段时原样返回：这是展示性字段，不该因为改写失败
// 把整个响应弄坏。
func withExecutedModel(raw []byte, model string) []byte {
	if len(raw) == 0 || model == "" || !bytes.Contains(raw, modelFieldKey) {
		return raw
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	cur, ok := m["model"]
	if !ok {
		return raw
	}
	var name string
	if json.Unmarshal(cur, &name) == nil && name == model {
		return raw
	}
	m["model"] = mustJSON(model)
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

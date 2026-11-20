package server

import (
	"sync"
	"time"
)

// responseStore 是 Responses API 的**内存**响应存储（store / previous_response_id）。
//
// 为什么只放内存：
//
//  1. 条目里是完整对话正文。项目的铁律是默认不记录任何对话内容（请求采样默认
//     关闭），把它写进 state.json 等于把全部对话永久留在磁盘上。
//  2. 落盘还要 schema 版本、迁移、容量治理，而它的唯一用途是
//     previous_response_id 续写 —— 进程重启后客户端重发上下文即可。
//
// 代价写在 docs/COMPAT.md：重启或淘汰后旧 id 失效，返回
// 404 previous_response_not_found。
type responseStore struct {
	mu       sync.Mutex
	entries  map[string]*storedResponse
	order    []string
	ttl      time.Duration
	capacity int
}

// storedResponse 是一条被保存的响应。
type storedResponse struct {
	// ID 是 resp_… 形式的响应 id。
	ID string
	// CreatedAt 是创建时刻（Unix 秒）。
	CreatedAt int64
	// Response 是完整的 response 对象（GET /v1/responses/{id} 直接返回它）。
	Response map[string]any
	// Messages 是本轮结束后的 Chat Completions 消息快照（含 assistant 回复），
	// previous_response_id 续写时直接当历史消息用。
	Messages []map[string]any
	// Input 是原始 input 条目（GET /v1/responses/{id}/input_items 用）。
	Input []any
	// Expires 是过期时刻。
	Expires time.Time
}

const (
	// responseTTL 是响应条目的存活时长。
	//
	// 1 小时：覆盖一次多轮工具调用的常见时长，又不至于让内存长期驻留对话正文。
	responseTTL = time.Hour
	// responseCapacity 是内存里最多保留的响应条数（按写入顺序淘汰最旧的）。
	responseCapacity = 256
)

func newResponseStore() *responseStore {
	return &responseStore{
		entries:  map[string]*storedResponse{},
		ttl:      responseTTL,
		capacity: responseCapacity,
	}
}

// put 写入一条响应，并清理过期与超容量的条目。
func (st *responseStore) put(e *storedResponse) {
	if st == nil || e == nil || e.ID == "" {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()

	now := time.Now()
	e.Expires = now.Add(st.ttl)
	if _, ok := st.entries[e.ID]; !ok {
		st.order = append(st.order, e.ID)
	}
	st.entries[e.ID] = e

	// 过期清理：只扫顺序表头部不够（条目过期时间相同），但扫全表也就几百条。
	for id, ent := range st.entries {
		if now.After(ent.Expires) {
			delete(st.entries, id)
		}
	}
	for len(st.entries) > st.capacity && len(st.order) > 0 {
		id := st.order[0]
		st.order = st.order[1:]
		delete(st.entries, id)
	}
	// 顺序表可能因为过期删除留下墓碑，顺手压一遍，避免无限增长。
	if len(st.order) > st.capacity*2 {
		kept := st.order[:0]
		for _, id := range st.order {
			if _, ok := st.entries[id]; ok {
				kept = append(kept, id)
			}
		}
		st.order = kept
	}
}

// get 读取一条响应，已过期时按不存在处理。
func (st *responseStore) get(id string) (*storedResponse, bool) {
	if st == nil || id == "" {
		return nil, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	e, ok := st.entries[id]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.Expires) {
		delete(st.entries, id)
		return nil, false
	}
	return e, true
}

// delete 删除一条响应，返回是否存在。
func (st *responseStore) delete(id string) bool {
	if st == nil || id == "" {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.entries[id]; !ok {
		return false
	}
	delete(st.entries, id)
	return true
}

// len 返回当前条目数（测试与控制台观测用）。
func (st *responseStore) len() int {
	if st == nil {
		return 0
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.entries)
}

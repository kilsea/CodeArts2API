package store

import (
	"path/filepath"
	"testing"
	"time"

	"codearts2api/internal/codearts"
)

// TestUpsertAccountMergesByDomainID 回归 v0.1.7 的真实故障：
// 浏览器登录创建的账号 UserData 为空，STS 的 AccessKey 每次登录都会轮换，
// 旧逻辑按 AK 去重必然匹配不上，于是同一个华为账号每重登一次就多一条记录。
// 现在 DomainID 相同就必须合并，且保留原 ID 与用户备注。
func TestUpsertAccountMergesByDomainID(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	st.UpsertAccount(&codearts.Account{
		ID: "a1", Name: "hw71755092", Enabled: true, Status: "ok", Note: "13800000000",
		Session: &codearts.Session{
			AccessKey: "AK-old", SecretKey: "SK-old", DomainID: "dom-1",
			ExpiresAt: time.Now().Add(-48 * time.Hour),
		},
	})

	isNew := st.UpsertAccount(&codearts.Account{
		ID: "a2", Name: "hw71755092", Enabled: true, Status: "ok",
		Session: &codearts.Session{
			AccessKey: "AK-new", SecretKey: "SK-new", DomainID: "dom-1",
			ExpiresAt: time.Now().Add(time.Hour),
		},
	})
	if isNew {
		t.Fatal("同一 DomainID 重新登录不应新增账号")
	}
	if got := len(st.Accounts()); got != 1 {
		t.Fatalf("账号数 = %d，期望 1", got)
	}
	got := st.Account("a1")
	if got == nil {
		t.Fatal("合并后未保留原 ID a1")
	}
	if got.Session == nil || got.Session.AccessKey != "AK-new" {
		t.Errorf("凭据未更新为最新: %+v", got.Session)
	}
	if got.Note != "13800000000" {
		t.Errorf("备注被清空: %q", got.Note)
	}
}

// TestUpsertAccountMergesByUserData 桌面端导入按 profile 目录去重，
// 同样不受 AK 轮换影响。
func TestUpsertAccountMergesByUserData(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join("C:", "Users", "me", "AppData", "Roaming", "codearts-agent")
	st.UpsertAccount(&codearts.Account{
		ID: "a1", Name: "desktop", Enabled: true, Status: "ok", UserData: dir,
		Session: &codearts.Session{AccessKey: "AK1", SecretKey: "SK1", ExpiresAt: time.Now().Add(-time.Hour)},
	})
	isNew := st.UpsertAccount(&codearts.Account{
		ID: "a2", Name: "desktop", Enabled: true, Status: "ok", UserData: dir + string(filepath.Separator),
		Session: &codearts.Session{AccessKey: "AK2", SecretKey: "SK2", ExpiresAt: time.Now().Add(time.Hour)},
	})
	if isNew {
		t.Fatal("同一 UserData 目录不应新增账号")
	}
	if got := len(st.Accounts()); got != 1 {
		t.Fatalf("账号数 = %d，期望 1", got)
	}
}

// TestUpsertAccountDoesNotMergeSameNameWithoutIdentity 没有 DomainID / UserData
// 的账号无法安全判定是否同一账号，宁可保留两条，也不能按名字猜着合并。
func TestUpsertAccountDoesNotMergeSameNameWithoutIdentity(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	st.UpsertAccount(&codearts.Account{
		ID: "a1", Name: "same-name", Enabled: true, Status: "ok",
		Session: &codearts.Session{AccessKey: "AK1", SecretKey: "SK1"},
	})
	st.UpsertAccount(&codearts.Account{
		ID: "a2", Name: "same-name", Enabled: true, Status: "ok",
		Session: &codearts.Session{AccessKey: "AK2", SecretKey: "SK2"},
	})
	if got := len(st.Accounts()); got != 2 {
		t.Fatalf("账号数 = %d，期望 2（不能按重名合并）", got)
	}
}

// TestUpsertAccountKeepsNewerCredential 合并时更旧的凭据不能覆盖现有有效凭据。
func TestUpsertAccountKeepsNewerCredential(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	st.UpsertAccount(&codearts.Account{
		ID: "a1", Name: "hw", Enabled: true, Status: "ok",
		Session: &codearts.Session{AccessKey: "AK-new", SecretKey: "SK", DomainID: "d", ExpiresAt: time.Now().Add(2 * time.Hour)},
	})
	st.UpsertAccount(&codearts.Account{
		ID: "a2", Name: "hw", Enabled: true, Status: "ok",
		Session: &codearts.Session{AccessKey: "AK-stale", SecretKey: "SK", DomainID: "d", ExpiresAt: time.Now().Add(-2 * time.Hour)},
	})
	got := st.Account("a1")
	if got.Session.AccessKey != "AK-new" {
		t.Errorf("旧凭据覆盖了新凭据: %s", got.Session.AccessKey)
	}
}

// TestMergeDuplicatesCollapsesDomainGroups 存量收敛：
// 线上同一个华为账号曾有 3 条记录（1 条过期 error + 2 条新登录），
// 启动后应合并成 1 条，ID 沿用最早的一条，凭据取过期最晚的一份。
func TestMergeDuplicatesCollapsesDomainGroups(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	st.st.Accounts = []*codearts.Account{
		{
			ID: "old", Name: "hw71755092", Source: "manual", Enabled: true,
			Status: "error", Failures: 16, Note: "主号",
			Session: &codearts.Session{AccessKey: "AK1", DomainID: "dom-1", ExpiresAt: time.Now().Add(-72 * time.Hour)},
		},
		{
			ID: "mid", Name: "hw71755092", Source: "browser", Enabled: true, Status: "ok",
			Session: &codearts.Session{AccessKey: "AK2", DomainID: "dom-1", ExpiresAt: time.Now().Add(time.Hour)},
		},
		{
			ID: "new", Name: "hw71755092", Source: "browser", Enabled: true, Status: "ok",
			LastClaimDate: "2026-11-19",
			Session:       &codearts.Session{AccessKey: "AK3", DomainID: "dom-1", ExpiresAt: time.Now().Add(2 * time.Hour)},
		},
		{
			ID: "other", Name: "another", Source: "manual", Enabled: true, Status: "ok",
			Session: &codearts.Session{AccessKey: "AK9", DomainID: "dom-2", ExpiresAt: time.Now().Add(time.Hour)},
		},
	}
	st.mu.Unlock()

	groups, removed := st.MergeDuplicates()
	if groups != 1 || removed != 2 {
		t.Fatalf("groups,removed = %d,%d，期望 1,2", groups, removed)
	}
	accts := st.Accounts()
	if len(accts) != 2 {
		t.Fatalf("合并后账号数 = %d，期望 2", len(accts))
	}
	got := st.Account("old")
	if got == nil {
		t.Fatal("未保留最早记录的 ID")
	}
	if got.Session == nil || got.Session.AccessKey != "AK3" {
		t.Errorf("未采用过期时间最晚的凭据: %+v", got.Session)
	}
	if got.Note != "主号" {
		t.Errorf("备注未合并: %q", got.Note)
	}
	if got.LastClaimDate != "2026-11-19" {
		t.Errorf("签到记录未合并: %q", got.LastClaimDate)
	}
	if st.Account("dom-2") != nil {
		t.Error("不应保留已被合并的 ID")
	}
}

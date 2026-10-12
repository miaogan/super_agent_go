package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/aegis/idl/kitex_gen/agentv1"
	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
)

func newTestStore() *Store {
	return New(Options{
		MaxMessagesPerSession: 5,
		MaxSessionsPerUser:    3,
		SessionTTL:            time.Hour,
	})
}

func id(uid int64) *commonv1.Identity {
	return &commonv1.Identity{Uid: uid, Tenant: "default", Username: "u"}
}

func mkAgent(name string, owner int64) *agentv1.Agent {
	return &agentv1.Agent{
		Name:         name,
		Description:  "d",
		SystemPrompt: "p",
		Tools:        []string{"search", "calc"},
		KbIds:        []int64{1, 2, 3},
		RagEnabled:   true,
		OwnerUid:     owner,
		Tenant:       "default",
	}
}

// ★隔离性是这里最关键的不变量：
//
//	pb 生成结构体的 slice 字段共享底层数组，
//	`cp := *x` 只复制指针。调用方拿到返回值后改一个元素，
//	会连带改掉仓库里的数据 —— 跨请求串数据，且极难复现。
func TestGetAgentReturnsIndependentCopy(t *testing.T) {
	s := newTestStore()
	aid, err := s.CreateAgent(context.Background(), mkAgent("a", 1))
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.GetAgent(context.Background(), aid)
	if err != nil {
		t.Fatal(err)
	}
	got.Tools[0] = "HACKED"
	got.KbIds[0] = 999
	got.Name = "HACKED"

	again, _ := s.GetAgent(context.Background(), aid)
	if again.Tools[0] == "HACKED" || again.KbIds[0] == 999 || again.Name == "HACKED" {
		t.Fatalf("仓库数据被调用方修改污染: tools=%v kbids=%v name=%s",
			again.Tools, again.KbIds, again.Name)
	}
}

// 入参也不能被存进仓库：调用方复用同一个 pb 对象改字段
// 会连带改掉已入库的记录。
func TestCreateAgentCopiesInput(t *testing.T) {
	s := newTestStore()
	src := mkAgent("orig", 1)
	aid, err := s.CreateAgent(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	src.Name = "mutated"
	src.Tools[1] = "mutated"

	got, _ := s.GetAgent(context.Background(), aid)
	if got.Name != "orig" || got.Tools[1] == "mutated" {
		t.Fatalf("入参被共享: name=%s tools=%v", got.Name, got.Tools)
	}
}

func TestListAgentsIsolation(t *testing.T) {
	s := newTestStore()
	_, _ = s.CreateAgent(context.Background(), mkAgent("a", 1))
	list, total := s.ListAgents(context.Background(), "", 1, "default", 1, 10)
	if total != 1 || len(list) != 1 {
		t.Fatalf("total=%d len=%d", total, len(list))
	}
	list[0].Tools[0] = "HACKED"
	list[0].Name = "HACKED"

	again, _ := s.ListAgents(context.Background(), "", 1, "default", 1, 10)
	if again[0].Tools[0] == "HACKED" || again[0].Name == "HACKED" {
		t.Fatal("ListAgents 返回的切片与仓库共享内存")
	}
}

func TestCreateAgentNameUniquePerTenant(t *testing.T) {
	s := newTestStore()
	if _, err := s.CreateAgent(context.Background(), mkAgent("dup", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAgent(context.Background(), mkAgent("dup", 2)); err == nil {
		t.Fatal("同名Agent 应被拒绝（租户内唯一）")
	}
}

func TestGetAgentNotFound(t *testing.T) {
	s := newTestStore()
	if _, err := s.GetAgent(context.Background(), 999); err == nil {
		t.Fatal("不存在的 agent 应报错")
	}
}

func TestTenantIsolation(t *testing.T) {
	s := newTestStore()
	if _, err := s.CreateAgent(context.Background(), mkAgent("a", 1)); err != nil {
		t.Fatal(err)
	}
	// 其他租户查不到
	_, total := s.ListAgents(context.Background(), "", 0, "other", 1, 10)
	if total != 0 {
		t.Fatalf("跨租户泄漏:total=%d", total)
	}
}

func TestUpdateAgentMask(t *testing.T) {
	s := newTestStore()
	aid, _ := s.CreateAgent(context.Background(), mkAgent("a", 1))

	// 只更新 description，name 应保持不变
	got, err := s.UpdateAgent(context.Background(), &agentv1.Agent{
		AgentId:     aid,
		Name:        "SHOULD-NOT-APPLY",
		Description: "new-desc",
	}, []string{"description"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "a" || got.Description != "new-desc" {
		t.Fatalf("update_mask 未生效: name=%s desc=%s", got.Name, got.Description)
	}
}

func TestSessionOwnership(t *testing.T) {
	s := newTestStore()
	aid, _ := s.CreateAgent(context.Background(), mkAgent("a", 1))
	sess, err := s.CreateSession(context.Background(), aid, "t", id(1))
	if err != nil {
		t.Fatal(err)
	}
	if sess.GetAgentId() != aid || sess.GetUid() != 1 { //nolint
		t.Fatalf("会话归属错误: %+v", sess)
	}
	// 其他用户的会话列表里不应出现
	list, total := s.ListSessions(context.Background(), 2, 0, "default", 1, 10)
	if total != 0 || len(list) != 0 {
		t.Fatalf("会话越权泄漏:total=%d", total)
	}
}

func TestAppendMessageIsolation(t *testing.T) {
	s := newTestStore()
	aid, _ := s.CreateAgent(context.Background(), mkAgent("a", 1))
	sess, _ := s.CreateSession(context.Background(), aid, "t", id(1))

	msg := &agentv1.ChatMessage{
		Role:    "assistant",
		Content: "答案",
		Citations: []*agentv1.Citation{
			{Index: 1, DocId: 7, DocTitle: "d.md"},
		},
	}
	saved, err := s.AppendMessage(context.Background(), sess.SessionId, msg)
	if err != nil {
		t.Fatal(err)
	}
	// 入参改了不应影响已存
	msg.Content = "mutated"
	msg.Citations[0].DocTitle = "mutated"

	list, total := s.ListMessages(context.Background(), sess.SessionId, 0, 1, 10)
	if total != 1 {
		t.Fatalf("total=%d", total)
	}
	if list[0].Content != "答案" || list[0].Citations[0].GetDocTitle() != "d.md" {
		t.Fatalf("入参被共享: content=%q cite=%q",
			list[0].Content, list[0].Citations[0].GetDocTitle())
	}
	// 返回值也应独立
	saved.Content = "HACKED"
	again, _ := s.ListMessages(context.Background(), sess.SessionId, 0, 1, 10)
	if again[0].Content == "HACKED" {
		t.Fatal("AppendMessage 返回值与仓库共享内存")
	}
}

// ★ 内存实现必须设消息上限：
//
//	否则长时间运行的进程会被历史会话吃光内存，
//	表现为运行数天后 OOM，且无法从代码上直接看出原因。
func TestMaxMessagesPerSession(t *testing.T) {
	s := newTestStore() // 上限 5
	aid, _ := s.CreateAgent(context.Background(), mkAgent("a", 1))
	sess, _ := s.CreateSession(context.Background(), aid, "t", id(1))

	for i := 0; i < 12; i++ {
		if _, err := s.AppendMessage(context.Background(), sess.SessionId, &agentv1.ChatMessage{
			Role: "user", Content: fmt.Sprintf("m%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.MessageCount(context.Background(), sess.SessionId); got > 5 {
		t.Fatalf("消息数 %d 超过上限 5", got)
	}
	// 应保留最新的消息，而不是最旧的。
	// direction=1 为正序（时间升序），0 为倒序。
	list, _ := s.ListMessages(context.Background(), sess.SessionId, 1, 1, 100)
	if len(list) == 0 || list[len(list)-1].Content != "m11" {
		t.Fatalf("裁剪策略应保留最新消息，实际末条=%v", list)
	}
	if list[0].Content != "m7" {
		t.Fatalf("应丢弃最旧的 7 条，实际首条=%q", list[0].Content)
	}
}

func TestMaxSessionsPerUser(t *testing.T) {
	s := newTestStore() // 上限 3
	aid, _ := s.CreateAgent(context.Background(), mkAgent("a", 1))
	for i := 0; i < 8; i++ {
		if _, err := s.CreateSession(context.Background(), aid, fmt.Sprintf("s%d", i), id(1)); err != nil {
			t.Fatal(err)
		}
	}
	_, total := s.ListSessions(context.Background(), 1, 0, "default", 1, 100)
	if total > 3 {
		t.Fatalf("会话数 %d 超过上限 3", total)
	}
}

// TTL 清理由 janitor 周期驱动（惰性清理：
// 读路径不做过期判定，避免每次列表都扫全表）。
func TestSessionTTLExpiry(t *testing.T) {
	s := New(Options{SessionTTL: 40 * time.Millisecond})
	aid, _ := s.CreateAgent(context.Background(), mkAgent("a", 1))
	sess, err := s.CreateSession(context.Background(), aid, "t", id(1))
	if err != nil {
		t.Fatal(err)
	}

	stop := s.StartJanitor(20 * time.Millisecond)
	defer stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := s.GetSession(context.Background(), sess.SessionId); err != nil {
			return // 已被清理
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("过期会话未被 janitor 清理")
}

func TestStartJanitorStops(t *testing.T) {
	s := newTestStore()
	stop := s.StartJanitor(20 * time.Millisecond)
	stop()
	stop() // 重复调用不应panic
	time.Sleep(60 * time.Millisecond)
}

func TestRecentMessages(t *testing.T) {
	s := newTestStore()
	aid, _ := s.CreateAgent(context.Background(), mkAgent("a", 1))
	sess, _ := s.CreateSession(context.Background(), aid, "t", id(1))
	for i := 0; i < 4; i++ {
		_, _ = s.AppendMessage(context.Background(), sess.SessionId, &agentv1.ChatMessage{
			Role: "user", Content: fmt.Sprintf("m%d", i),
		})
	}
	got := s.RecentMessages(context.Background(), sess.SessionId, 2)
	if len(got) != 2 {
		t.Fatalf("期望 2 条，实际 %d", len(got))
	}
	if got[len(got)-1].Content != "m3" {
		t.Fatalf("RecentMessages 应返回最新 N 条，实际末条=%q", got[len(got)-1].Content)
	}
}

func TestDeleteAgentCascadesSessions(t *testing.T) {
	s := newTestStore()
	aid, _ := s.CreateAgent(context.Background(), mkAgent("a", 1))
	sess, _ := s.CreateSession(context.Background(), aid, "t", id(1))

	if err := s.DeleteAgent(context.Background(), aid); err != nil {
		t.Fatal(err)
	}
	// Agent 已删，其会话不应残留（否则成为孤儿数据）
	if _, err := s.GetSession(context.Background(), sess.SessionId); err == nil {
		t.Fatal("Agent 删除后会话应被级联清理")
	}
}

// ★ 并发读写：内存实现靠一把大锁，
//
//	这里验证不会 race，也不会出现半写状态。
func TestConcurrentAccess(t *testing.T) {
	s := newTestStore()
	aid, _ := s.CreateAgent(context.Background(), mkAgent("a", 1))
	sess, _ := s.CreateSession(context.Background(), aid, "t", id(1))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				switch j % 5 {
				case 0:
					_, _ = s.AppendMessage(context.Background(), sess.SessionId, &agentv1.ChatMessage{
						Role: "user", Content: fmt.Sprintf("m%d-%d", i, j),
					})
				case 1:
					_, _ = s.ListMessages(context.Background(), sess.SessionId, 0, 1, 10)
				case 2:
					_, _ = s.GetAgent(context.Background(), aid)
				case 3:
					_, _ = s.CreateSession(context.Background(), aid, "t", id(int64(i)))
				case 4:
					_, _, _ = s.Stats(context.Background()), s.RecentMessages(context.Background(), sess.SessionId, 3), s.MessageCount(context.Background(), sess.SessionId)
				}
			}
		}(i)
	}
	wg.Wait()
}

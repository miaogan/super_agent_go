package service

import (
	"context"
	"testing"

	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/errno"
	"github.com/aegis/pkg/mockx"

	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
	ragv1 "github.com/aegis/idl/kitex_gen/ragv1"
	"github.com/aegis/pkg/vectordb/mockstore"
	"github.com/aegis/rag_service/internal/meta"
)

// 文档级接口的归属校验。
//
// 这组用例覆盖的是一条真实存在过的越权路径：KB 级接口一直有
// checkKBPermission，但文档级接口只按自增 doc_id 直查元数据，
// 而网关对 /api/v1 只校验"是否登录"、不校验"是否属于你"，
// 于是任何登录用户枚举 doc_id 即可读取乃至删除他人文档。

const (
	kbID     = 7
	docID    = 101
	ownerUID = 1000
	tenant   = "acme"
)

func user(uid int64, tn string, scopes ...string) *commonv1.Identity {
	return &commonv1.Identity{Uid: uid, Tenant: tn, Scopes: scopes}
}

// newTestService 装配一个只带元数据与向量 mock 的门面实例：
// 鉴权发生在 pipeline/engine 之前，这两个依赖对被测路径没有影响。
func newTestService(t *testing.T) (*RagService, *meta.MemoryStore) {
	t.Helper()
	m := meta.NewMemoryStore()
	if err := m.CreateKB(context.Background(), &ragv1.KnowledgeBase{
		KbId:     kbID,
		Name:     "kb",
		OwnerUid: ownerUID,
		Tenant:   tenant,
	}); err != nil {
		t.Fatalf("建 KB 失败: %v", err)
	}
	if err := m.CreateDoc(context.Background(), &meta.DocumentRecord{
		Doc:      &ragv1.Document{DocId: docID, KbId: kbID, FileName: "a.pdf"},
		RawBytes: []byte("正文"),
	}); err != nil {
		t.Fatalf("建文档失败: %v", err)
	}
	s := New(m, mockstore.New(), nil, nil,
		mockx.NewFaultInjector(false, 0, nil), conf.RAGConf{}, nil)
	return s, m
}

// docStillExists 用于断言"被拒绝的删除没有产生副作用"。
func docStillExists(t *testing.T, m *meta.MemoryStore) bool {
	_, err := m.GetDoc(context.Background(), docID)
	return err == nil
}

func TestGetDocument_Permission(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()

	cases := []struct {
		name string
		id   *commonv1.Identity
		ok   bool
	}{
		{"属主可读", user(ownerUID, tenant), true},
		{"同租户可读", user(2000, tenant), true},
		{"admin 可读", user(3000, "other", "admin"), true},
		{"跨租户拒绝", user(2000, "evil"), false},
		{"匿名拒绝", user(0, ""), false},
		{"nil identity 拒绝", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.GetDocument(ctx, &ragv1.GetDocumentReq{DocId: docID, Identity: c.id})
			if c.ok {
				if err != nil {
					t.Fatalf("应放行, 实际 %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("应拒绝, 实际放行")
			}
			if got := errno.CodeOf(err); got != errno.ErrKBAccessDenied {
				t.Fatalf("应判为越权(40422)，实际 [%d] %v", got, err)
			}
		})
	}
}

// TestGetDocument_CrossTenantNotMaskedAsNotFound 确认拒绝来自鉴权层，
// 而不是"文档恰好查不到"顺带失败 —— 后者会让用例失去意义。
func TestGetDocument_CrossTenantNotMaskedAsNotFound(t *testing.T) {
	s, _ := newTestService(t)
	_, err := s.GetDocument(context.Background(),
		&ragv1.GetDocumentReq{DocId: docID, Identity: user(9999, "evilcorp")})
	if err == nil {
		t.Fatal("跨租户读取应被拒绝")
	}
	if c := errno.CodeOf(err); c != errno.ErrKBAccessDenied {
		t.Fatalf("错误码应为越权(40422)，实际 [%d] %v", c, err)
	}
}

func TestListChunks_Permission(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()

	if _, err := s.ListChunks(ctx, &ragv1.ListChunksReq{DocId: docID, Identity: user(ownerUID, tenant)}); err != nil {
		t.Fatalf("属主应可读切片: %v", err)
	}
	if _, err := s.ListChunks(ctx, &ragv1.ListChunksReq{DocId: docID, Identity: user(2000, "evil")}); err == nil {
		t.Fatal("跨租户应被拒绝")
	}
	// 切片正文等价于文档内容，匿名同样拒绝
	if _, err := s.ListChunks(ctx, &ragv1.ListChunksReq{DocId: docID}); err == nil {
		t.Fatal("无 identity 应被拒绝")
	}
}

// TestDeleteDocument_RequiresOwnerNotJustTenant 删除是破坏性操作：
// 同租户可共享阅读，但不能删别人上传的文档。
func TestDeleteDocument_RequiresOwnerNotJustTenant(t *testing.T) {
	s, m := newTestService(t)
	ctx := context.Background()

	// 同租户非属主：读得到，删不掉
	if _, err := s.GetDocument(ctx, &ragv1.GetDocumentReq{DocId: docID, Identity: user(2000, tenant)}); err != nil {
		t.Fatalf("前置条件失败，同租户应可读: %v", err)
	}
	if _, err := s.DeleteDocument(ctx, &ragv1.DeleteDocumentReq{DocId: docID, Identity: user(2000, tenant)}); err == nil {
		t.Fatal("同租户非属主不应能删除文档")
	}
	if !docStillExists(t, m) {
		t.Fatal("被拒绝的删除仍产生了副作用")
	}

	// 跨租户同样拒绝
	if _, err := s.DeleteDocument(ctx, &ragv1.DeleteDocumentReq{DocId: docID, Identity: user(2000, "evil")}); err == nil {
		t.Fatal("跨租户不应能删除文档")
	}
	if !docStillExists(t, m) {
		t.Fatal("被拒绝的删除仍产生了副作用")
	}

	// admin 可删
	if _, err := s.DeleteDocument(ctx, &ragv1.DeleteDocumentReq{DocId: docID, Identity: user(4000, "other", "admin")}); err != nil {
		t.Fatalf("admin 应可删除: %v", err)
	}
	if docStillExists(t, m) {
		t.Fatal("admin 删除后文档应已不存在")
	}
}

func TestDeleteDocument_OwnerAllowed(t *testing.T) {
	s, m := newTestService(t)
	if _, err := s.DeleteDocument(context.Background(),
		&ragv1.DeleteDocumentReq{DocId: docID, Identity: user(ownerUID, tenant)}); err != nil {
		t.Fatalf("属主应可删除: %v", err)
	}
	if docStillExists(t, m) {
		t.Fatal("删除未生效")
	}
}

func TestListDocuments_Permission(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()

	if _, err := s.ListDocuments(ctx, &ragv1.ListDocumentsReq{KbId: kbID, Identity: user(2000, tenant)}); err != nil {
		t.Fatalf("同租户应可列举: %v", err)
	}
	if _, err := s.ListDocuments(ctx, &ragv1.ListDocumentsReq{KbId: kbID, Identity: user(2000, "evil")}); err == nil {
		t.Fatal("跨租户应被拒绝")
	}
	// 不存在且无权限的 KB 也不能泄露差异信息，只要求被拒
	if _, err := s.ListDocuments(ctx, &ragv1.ListDocumentsReq{KbId: 999, Identity: user(2000, tenant)}); err == nil {
		t.Fatal("不存在的 KB 应报错")
	}
}

// TestRetryDocument_DeniedBeforeIngest 重试会重写切片与向量，属破坏性操作；
// 且必须在触达 pipeline 之前就拒绝（本用例里 pipeline 为 nil，
// 一旦漏过鉴权会直接 panic，因此这同时是顺序守卫）。
func TestRetryDocument_DeniedBeforeIngest(t *testing.T) {
	s, _ := newTestService(t)
	cases := []*commonv1.Identity{
		user(2000, tenant), // 同租户非属主
		user(2000, "evil"), // 跨租户
		user(0, ""),        // 匿名
	}
	for _, id := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("identity=%+v 时漏过鉴权并进入了 pipeline（panic: %v）", id, r)
				}
			}()
			if _, err := s.RetryDocument(context.Background(),
				&ragv1.RetryDocumentReq{DocId: docID, Identity: id}); err == nil {
				t.Errorf("identity=%+v 应被拒绝", id)
			}
		}()
	}
}

// TestDocumentOps_DenyMissingKBParent 文档指向一个不存在的 KB 时，
// 不能因为"取不到 KB 就没法判权限"而放行。
func TestDocumentOps_DenyMissingKBParent(t *testing.T) {
	m := meta.NewMemoryStore()
	if err := m.CreateDoc(context.Background(), &meta.DocumentRecord{
		Doc: &ragv1.Document{DocId: 555, KbId: 9999, FileName: "orphan.pdf"},
	}); err != nil {
		t.Fatalf("建文档失败: %v", err)
	}
	s := New(m, mockstore.New(), nil, nil, mockx.NewFaultInjector(false, 0, nil), conf.RAGConf{}, nil)

	if _, err := s.GetDocument(context.Background(),
		&ragv1.GetDocumentReq{DocId: 555, Identity: user(1, "t")}); err == nil {
		t.Fatal("所属 KB 不存在时应拒绝，而不是放行")
	}
}

// TestAgentKBList_StillGuardedByKBRules 确认这次改动没有回头削弱
// KB 级接口原有行为：匿名内部调用在 checkKBPermission 下仍按原语义处理。
func TestAgentKBList_StillGuardedByKBRules(t *testing.T) {
	s, _ := newTestService(t)
	kb, err := s.meta.GetKB(context.Background(), kbID)
	if err != nil {
		t.Fatalf("取 KB 失败: %v", err)
	}
	// 既有语义：uid==0 视为内部调用放行（文档级接口不接受，见上面用例）
	if err := s.checkKBPermission(user(0, ""), kb); err != nil {
		t.Fatalf("KB 级接口的内部调用语义被改变: %v", err)
	}
	// 而文档级必须拒绝同一个 identity
	if err := s.checkDocPermission(user(0, ""), kb, false); err == nil {
		t.Fatal("文档级接口不应沿用 uid==0 放行")
	}
}

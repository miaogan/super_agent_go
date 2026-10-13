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
		mockx.NewFaultInjector(false, 0, nil), conf.RAGConf{}, nil, EmbedDefaults{})
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

// TestRetrieve_AuthorizesRequestedKBIDs 检索入口的 kb_ids 来自请求体，
// 必须逐个校验归属。
//
// 这条路径过去可以越权：租户过滤条件是 `(tenant=="" or tenant==请求租户)`，
// 而 ingest 曾不写 Tenant，使该过滤恒真；配合未校验的 kb_ids，
// 任何登录用户传入他人 kb_id（自增整数，可枚举）即可取回其知识库切片正文。
//
// 断言时 engine 传 nil：鉴权必须在调用 engine 之前完成，
// 一旦漏过会直接 panic，因此这同时是顺序守卫。
func TestRetrieve_AuthorizesRequestedKBIDs(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()

	cases := []struct {
		name string
		id   *commonv1.Identity
		deny bool
	}{
		{"属主可检索", user(ownerUID, tenant), false},
		{"同租户可检索", user(2000, tenant), false},
		{"admin 可检索", user(3000, "other", "admin"), false},
		{"跨租户拒绝", user(2000, "evil"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := s.authorizeKBIDs(ctx, c.id, []int64{kbID})
			if c.deny && err == nil {
				t.Fatal("应被拒绝")
			}
			if !c.deny && err != nil {
				t.Fatalf("应放行, 实际 %v", err)
			}
			if c.deny {
				if got := errno.CodeOf(err); got != errno.ErrKBAccessDenied {
					t.Fatalf("应判为越权(40422)，实际 [%d] %v", got, err)
				}
			}
		})
	}

	// 端到端：拒绝必须发生在调用 engine 之前（engine 为 nil，漏过即 panic）
	s.cfg.EnableRAG = true
	for _, id := range []*commonv1.Identity{user(2000, "evil")} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("跨租户请求漏过鉴权并进入了 engine（panic: %v）", r)
				}
			}()
			if _, err := s.Retrieve(ctx, &ragv1.RetrieveReq{
				Query: "x", KbIds: []int64{kbID}, Identity: id,
			}); err == nil {
				t.Fatal("应被拒绝")
			}
		}()
	}
}

// TestRetrieve_DeniedIfAnyKBForbidden 多个 kb_id 中夹一个无权项时整体拒绝。
// 静默剔除会让调用方以为检索范围就是自己请求的范围，只是结果偏少。
func TestRetrieve_DeniedIfAnyKBForbidden(t *testing.T) {
	s, _ := newTestService(t)
	s.cfg.EnableRAG = true
	// 再造一个属于别的租户的 KB
	if err := s.meta.CreateKB(context.Background(), &ragv1.KnowledgeBase{
		KbId: 888, Name: "other", OwnerUid: 7777, Tenant: "evilcorp",
	}); err != nil {
		t.Fatalf("建 KB 失败: %v", err)
	}
	_, err := s.Retrieve(context.Background(), &ragv1.RetrieveReq{
		Query: "x", KbIds: []int64{kbID, 888}, Identity: user(2000, tenant),
	})
	if err == nil {
		t.Fatal("夹带无权 kb_id 应整体拒绝")
	}
	if got := errno.CodeOf(err); got != errno.ErrKBAccessDenied {
		t.Fatalf("应判为越权, 实际 [%d] %v", got, err)
	}
}

// TestRetrieve_EmptyKBIDsNotAnAuthzError 空 kb_ids 不是权限问题：
// agent 未绑定知识库时按既有语义走，不应在这里报错。
func TestRetrieve_EmptyKBIDsNotAnAuthzError(t *testing.T) {
	s, _ := newTestService(t)
	// 空列表直接放行给上层语义处理（engine 为 nil，不能真的走完整 Retrieve）
	if err := s.authorizeKBIDs(context.Background(), user(ownerUID, tenant), nil); err != nil {
		t.Fatalf("空 kb_ids 不应判为越权: %v", err)
	}
}

// TestDebugRetrieve_AuthorizesKBIDs DebugRetrieve 虽在网关侧限 admin，
// 但 Kitex 内部调用不经过网关，服务侧仍需自校验。
func TestDebugRetrieve_AuthorizesKBIDs(t *testing.T) {
	s, _ := newTestService(t)
	_, err := s.DebugRetrieve(context.Background(), &ragv1.DebugRetrieveReq{
		Query: "x", KbIds: []int64{kbID}, Identity: user(2000, "evil"),
	})
	if err == nil {
		t.Fatal("跨租户 DebugRetrieve 应被拒绝")
	}
	if got := errno.CodeOf(err); got != errno.ErrKBAccessDenied {
		t.Fatalf("应判为越权, 实际 [%d] %v", got, err)
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
	s := New(m, mockstore.New(), nil, nil, mockx.NewFaultInjector(false, 0, nil), conf.RAGConf{}, nil, EmbedDefaults{})

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

// TestEmbedDefaults_ComeFromConfig 建库的维度与默认模型必须来自注入的配置。
//
// 这条是回归防线：历史上 cfgEmbedDim() 恒返回 1024、默认模型名硬编码，
// 换用 1536 维模型时集合仍按 1024 建立，直到向量化写入阶段才炸维度不匹配，
// 而 configs 的自洽检查看不到这里。
func TestEmbedDefaults_ComeFromConfig(t *testing.T) {
	s, _ := newTestService(t)

	s.embed = EmbedDefaults{Model: "text-embedding-v2", Dim: 1536}
	if got := s.cfgEmbedDim(); got != 1536 {
		t.Errorf("维度应取配置值 1536, 实际 %d", got)
	}
	if got := s.defaultEmbedModel(); got != "text-embedding-v2" {
		t.Errorf("默认模型应取配置值, 实际 %q", got)
	}

	// 未注入时回退历史默认，不能让建库直接失败
	s.embed = EmbedDefaults{}
	if got := s.cfgEmbedDim(); got != 1024 {
		t.Errorf("零值应回退 1024, 实际 %d", got)
	}
	if got := s.defaultEmbedModel(); got != "text-embedding-v4" {
		t.Errorf("零值应回退 text-embedding-v4, 实际 %q", got)
	}

	// dimOf 要认得配置里的模型，否则"维度与模型一致"的校验会被静默跳过
	if got := s.dimOf(s.defaultEmbedModel()); got == 0 && s.embed.Dim > 0 {
		t.Logf("提示：模型 %q 不在 dimOf 表中，维度一致性校验不会生效", s.defaultEmbedModel())
	}
}

// TestCreateKB_DimMatchesDeclaredModel 配置 1536 维模型时，建库不应仍用 1024。
func TestCreateKB_DimMatchesDeclaredModel(t *testing.T) {
	s, _ := newTestService(t)
	s.embed = EmbedDefaults{Model: "text-embedding-v2", Dim: 1536}

	_, err := s.CreateKnowledgeBase(context.Background(), &ragv1.CreateKnowledgeBaseReq{
		Identity: user(ownerUID, tenant),
		Name:     "dim-check",
	})
	if err != nil {
		t.Fatalf("用配置的模型/维度建库应成功: %v", err)
	}
	kbs, _ := s.meta.ListKB(context.Background(), "", tenant, ownerUID, 1, 20)
	for _, kb := range kbs {
		if kb.GetName() == "dim-check" && kb.GetEmbeddingDim() != 1536 {
			t.Errorf("集合维度应为 1536，实际 %d", kb.GetEmbeddingDim())
		}
	}
}

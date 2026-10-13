package tokenstore

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aegis/pkg/conf"
)

func TestNewPGValidatesConfig(t *testing.T) {
	ctx := context.Background()
	for name, dsn := range map[string]string{
		"空 DSN": "",
		"纯空白":   "   ",
		"非法连接串": "not-a-dsn",
		"协议不支持": "mysql://u:p@127.0.0.1:3306/db",
		"端口非数字": "postgres://u:p@127.0.0.1:abc/db",
	} {
		t.Run(name, func(t *testing.T) {
			s, err := NewPG(ctx, conf.PostgresConf{DSN: dsn})
			if err == nil {
				if s != nil {
					s.Close()
				}
				t.Fatalf("DSN=%q 应返回 error", dsn)
			}
			if s != nil {
				t.Fatal("返回 error 时 store 必须为 nil，让调用方干净降级")
			}
		})
	}
}

// TestCrossInstanceVisibility ★ 这条直接测被修复的缺陷本身：
// 副本 A 吊销、副本 B 必须看得见。原先两个 Manager 各有内存 map，
// 互相看不到，已登出的票在另一副本上继续放行。
func TestCrossInstanceVisibility(t *testing.T) {
	a := mustStore(t)
	defer a.Close()
	// 独立建第二个连接池，模拟另一个副本（不是复用同一个对象）
	b, err := NewPG(context.Background(), conf.PostgresConf{DSN: dsn(t)})
	if err != nil {
		t.Fatalf("建第二个实例失败: %v", err)
	}
	defer b.Close()

	ctx := context.Background()
	uid := probeUID()
	jti := "jti-x-" + uidStr(uid)

	if err := a.Revoke(ctx, uid, jti, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Revoke 失败: %v", err)
	}
	revoked, err := b.IsRevoked(ctx, jti)
	if err != nil {
		t.Fatalf("IsRevoked 失败: %v", err)
	}
	if !revoked {
		t.Fatal("副本 B 应看到副本 A 的吊销（跨副本可见性失效）")
	}

	// 反向：未吊销的 jti 必须返回 false
	got, err := b.IsRevoked(ctx, "never-issued-"+uidStr(uid))
	if err != nil {
		t.Fatalf("IsRevoked 失败: %v", err)
	}
	if got {
		t.Error("未登记的 jti 不应被判为已吊销")
	}
}

// TestRevokeIsIdempotent 同一令牌重复登出（客户端重试、access/refresh 成对吊销）
// 不应报错，否则"再点一次退出登录"会变成 500。
func TestRevokeIsIdempotent(t *testing.T) {
	s := mustStore(t)
	defer s.Close()
	ctx := context.Background()
	uid := probeUID()
	jti := "jti-idem-" + uidStr(uid)
	exp := time.Now().Add(time.Hour)

	for i := 0; i < 3; i++ {
		if err := s.Revoke(ctx, uid, jti, exp); err != nil {
			t.Fatalf("第 %d 次 Revoke 失败: %v", i+1, err)
		}
	}
	// TrackIssued 同样要能重入
	if err := s.TrackIssued(ctx, uid, jti, exp); err != nil {
		t.Fatal(err)
	}
	if err := s.TrackIssued(ctx, uid, jti, exp); err != nil {
		t.Fatalf("重复登记签发关系应幂等: %v", err)
	}
}

// TestActiveJTIExcludesExpired 按用户列出在用令牌时必须滤掉已过期的 ——
// 否则过期票会被"再吊销一遍"，白占表空间也误导排查。
func TestActiveJTIExcludesExpired(t *testing.T) {
	s := mustStore(t)
	defer s.Close()
	ctx := context.Background()
	uid := probeUID()
	tag := uidStr(uid)

	if err := s.TrackIssued(ctx, uid, "live-"+tag, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.TrackIssued(ctx, uid, "dead-"+tag, time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	list, err := s.ActiveJTI(ctx, uid)
	if err != nil {
		t.Fatalf("ActiveJTI 失败: %v", err)
	}
	var hasLive, hasDead bool
	for _, j := range list {
		switch j {
		case "live-" + tag:
			hasLive = true
		case "dead-" + tag:
			hasDead = true
		}
	}
	if !hasLive {
		t.Errorf("未过期的 jti 应在列表中, 实际 %v", list)
	}
	if hasDead {
		t.Errorf("已过期的 jti 不该出现在列表中（登出范围被放大）: %v", list)
	}
}

// TestPruneExpiredOnlyRemovesExpired 清理必须精确：只删过期行。
// 这两张表若被误删未过期行，等于把已登出的令牌重新放行 —— 是安全回退。
func TestPruneExpiredOnlyRemovesExpired(t *testing.T) {
	s := mustStore(t)
	defer s.Close()
	ctx := context.Background()
	uid := probeUID()
	tag := uidStr(uid)

	expired := time.Now().Add(-time.Hour)
	future := time.Now().Add(3 * time.Hour)

	if err := s.Revoke(ctx, uid, "rvd-expired-"+tag, expired); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(ctx, uid, "rvd-future-"+tag, future); err != nil {
		t.Fatal(err)
	}
	before, err := s.ActiveJTI(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	_ = before

	n, err := s.PruneExpired(ctx)
	if err != nil {
		t.Fatalf("PruneExpired 失败: %v", err)
	}
	if n < 1 {
		t.Errorf("至少应清掉那条已过期的吊销记录, 实际 %d", n)
	}
	// 未过期的吊销必须仍然生效
	ok, err := s.IsRevoked(ctx, "rvd-future-"+tag)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("未过期的吊销记录被清掉了 —— 等于把已登出的令牌重新放行")
	}
	// 已过期的可以消失（JWT 校验层会先拒过期票，留着无意义）
	if got, _ := s.IsRevoked(ctx, "rvd-expired-"+tag); got {
		t.Log("提示：过期吊销行仍可见（Prune 未覆盖该条），不影响安全性")
	}
}

// TestRevokeRejectsEmptyJTI 空 jti 不能静默写进去。
//
// 一旦存在一行 jti=” ，IsRevoked(”) 就会把所有无 jti 的票据判为已吊销；
// 反之若空 jti 被当普通值写入，吊销名单就多了一条永远不会命中的垃圾行。
// 校验在碰连接池之前，所以这条不需要数据库也能跑。
func TestRevokeRejectsEmptyJTI(t *testing.T) {
	s := &PG{}
	ctx := context.Background()
	if err := s.Revoke(ctx, 1, "", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("空 jti 的 Revoke 应报错")
	}
	if err := s.TrackIssued(ctx, 1, "", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("空 jti 的 TrackIssued 应报错")
	}
	// IsRevoked 对空 jti 选择"未吊销 + 无错误"：合法请求不该被一条
	// 不存在的吊销记录影响，但也不该为此打库。
	ok, err := s.IsRevoked(ctx, "")
	if err != nil || ok {
		t.Errorf("空 jti 应直接判未吊销, 实际 (%v, %v)", ok, err)
	}
}

// TestIsRevokedErrorsNotSilentFalse ★ 存储故障时不能返回"未吊销 + nil"。
//
// 那会让鉴权在 DB 抖动时全部放行。返回 error 是把决策权交回调用方：
// 它可以选择拒绝请求，或显式退回进程内名单，但不能被骗。
func TestIsRevokedErrorsNotSilentFalse(t *testing.T) {
	s := mustStore(t)
	jti := "jti-err-" + uidStr(probeUID())
	if err := s.Revoke(context.Background(), 1, jti, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.Close() // 池已关：后续查询必然失败

	ok, err := s.IsRevoked(context.Background(), jti)
	if err == nil {
		t.Fatalf("存储不可用时应返回 error, 实际返回 (%v, nil) —— 会被上层当作未吊销放行", ok)
	}
	if ok {
		t.Error("出错时不应顺带返回 true（会让所有请求被拒，掩盖真实故障）")
	}
}

// ---- 夹具 ----

func dsn(t *testing.T) string {
	t.Helper()
	d := strings.TrimSpace(os.Getenv("AEGIS_TEST_PG_DSN"))
	if d == "" {
		t.Skip("未设置 AEGIS_TEST_PG_DSN，跳过真实库验证")
	}
	return d
}

func mustStore(t *testing.T) *PG {
	t.Helper()
	s, err := NewPG(context.Background(), conf.PostgresConf{DSN: dsn(t)})
	if err != nil {
		t.Fatalf("建 store 失败: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// probeUID 用时间戳造一个不会与既有数据冲突的 uid，
// 各用例因此可以并发跑、且不需要事务回滚来清理。
func probeUID() int64 { return time.Now().UnixNano() % 1_000_000_000 }

func uidStr(uid int64) string { return time.Unix(0, uid).Format("150405.000000000") }

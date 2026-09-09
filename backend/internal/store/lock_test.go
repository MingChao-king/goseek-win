package store

import (
	"errors"
	"testing"

	"goseek/internal/domain"
)

// 同一个会话不能被两个进程同时打开，否则两边各自追加消息会得到一段谁也说不清
// 顺序的历史。
func TestSecondLoadOfTheSameSessionIsRefused(t *testing.T) {
	first, dataDirectory := newTestStore(t)
	session := sampleSession(t, first)

	second := openStoreAt(t, dataDirectory, fixedTime)
	if _, err := second.Load(session.ID); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("第二次 Load 的错误 = %v; want ErrSessionBusy", err)
	}
}

// 释放之后同一个会话应当可以被重新打开。
func TestSessionCanBeReopenedAfterClose(t *testing.T) {
	first, dataDirectory := newTestStore(t)
	session := sampleSession(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("Close 返回错误: %v", err)
	}

	second := openStoreAt(t, dataDirectory, fixedTime)
	if _, err := second.Load(session.ID); err != nil {
		t.Fatalf("释放后重新 Load 失败: %v", err)
	}
}

// 锁是会话级的，不是数据库级的：另一个进程可以同时打开别的会话。
func TestDifferentSessionsCanBeHeldByDifferentStores(t *testing.T) {
	first, dataDirectory := newTestStore(t)
	sampleSession(t, first)

	second := openStoreAt(t, dataDirectory, fixedTime)
	if _, err := second.Create("/tmp/another", ""); err != nil {
		t.Fatalf("打开另一个会话失败: %v", err)
	}
}

// 一个 Store 只服务一个会话；重复打开是调用方用错了，要明确报错。
func TestOpeningASecondSessionOnTheSameStoreIsRefused(t *testing.T) {
	store, _ := newTestStore(t)
	sampleSession(t, store)

	if _, err := store.Create("/tmp/other", ""); err == nil {
		t.Error("同一个 Store 上创建第二个会话没有报错")
	}
	other, err := domain.NewSessionID()
	if err != nil {
		t.Fatalf("NewSessionID 返回错误: %v", err)
	}
	if _, err := store.Load(other); err == nil {
		t.Error("同一个 Store 上加载第二个会话没有报错")
	}
}

// Create 写库失败时不能留下已经持有的锁，否则这个 ID 在本进程里就再也打不开了。
func TestCreateReleasesTheLockWhenInsertFails(t *testing.T) {
	store, _ := newTestStore(t)
	// 关掉数据库连接，让随后的 INSERT 必然失败。
	if err := store.connection.Close(); err != nil {
		t.Fatalf("关闭连接失败: %v", err)
	}

	if _, err := store.Create("/tmp/goseek-work", ""); err == nil {
		t.Fatal("连接已关闭时 Create 成功了")
	}
	if store.held != nil {
		t.Error("Create 失败后仍然持有锁")
	}
}

// Close 之后再 Close 不能出错：正常退出路径上它会被 defer 调用，
// 而出错路径上可能已经显式关过一次。
func TestCloseIsIdempotent(t *testing.T) {
	store, _ := newTestStore(t)
	sampleSession(t, store)

	if err := store.Close(); err != nil {
		t.Fatalf("第一次 Close 返回错误: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Errorf("第二次 Close 返回错误: %v", err)
	}
}

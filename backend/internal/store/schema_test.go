package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goseek/internal/domain"
)

// 建库之后版本号要停在最新一版；再次打开同一个库不能重复执行迁移。
func TestMigrateIsIdempotentAcrossReopen(t *testing.T) {
	first, dataDirectory := newTestStore(t)
	sampleSession(t, first)

	var version int
	if err := first.connection.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("读取版本失败: %v", err)
	}
	if version != len(migrations) {
		t.Errorf("user_version = %d; want %d", version, len(migrations))
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close 返回错误: %v", err)
	}

	// 重新打开：迁移已经执行过，不能再建一次表。
	second := openStoreAt(t, dataDirectory, fixedTime)
	summaries, err := second.List(false)
	if err != nil {
		t.Fatalf("重新打开后 List 失败: %v", err)
	}
	if len(summaries) != 1 {
		t.Errorf("重新打开后看到 %d 个会话; want 1", len(summaries))
	}
}

// 比本程序更新的库要明确拒绝：那里面可能有本程序不认识的表和列，
// 继续写下去会留下一份两边都读不好的数据。
func TestOpenRejectsNewerDatabaseVersion(t *testing.T) {
	store, dataDirectory := newTestStore(t)
	if _, err := store.connection.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatalf("改写版本失败: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close 返回错误: %v", err)
	}

	if _, err := New(dataDirectory); err == nil {
		t.Fatal("更新的数据库版本被接受了")
	} else if !strings.Contains(err.Error(), "版本") {
		t.Errorf("错误信息 = %q", err.Error())
	}
}

// 角色的 CHECK 约束必须真的在库里生效，而不只是写在建表语句里。
func TestRoleCheckConstraintIsEnforced(t *testing.T) {
	store, _ := newTestStore(t)
	session := sampleSession(t, store)

	_, err := store.connection.Exec(
		`INSERT INTO messages (session_id, seq, role, content) VALUES (?, ?, ?, ?)`,
		string(session.ID), 99, "system", "我不该被写进去")
	if err == nil {
		t.Fatal("CHECK 约束没有拦住非法角色")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "constraint") {
		t.Errorf("错误信息 = %q; want 约束错误", err.Error())
	}
}

// STRICT 表要在写入时就拒绝类型不符的值，而不是等读出来才出问题。
func TestStrictTablesRejectWrongColumnTypes(t *testing.T) {
	store, _ := newTestStore(t)
	session := sampleSession(t, store)

	_, err := store.connection.Exec(
		`INSERT INTO messages (session_id, seq, role, content) VALUES (?, ?, ?, ?)`,
		string(session.ID), "不是整数", "user", "内容")
	if err == nil {
		t.Fatal("STRICT 表接受了类型不符的 seq")
	}
}

// 外键必须启用：没有对应会话的消息不该能写进去。
func TestForeignKeyConstraintIsEnforced(t *testing.T) {
	store, _ := newTestStore(t)

	_, err := store.connection.Exec(
		`INSERT INTO messages (session_id, seq, role, content) VALUES (?, ?, ?, ?)`,
		"ses_0123456789abcdef0123456789abcdef", 0, "user", "孤儿消息")
	if err == nil {
		t.Fatal("外键约束没有拦住孤儿消息")
	}
}

// 删除会话要连带删掉它的消息，不能留下永远没人读的行。
func TestDeletingASessionCascadesToItsMessages(t *testing.T) {
	store, _ := newTestStore(t)
	session := sampleSession(t, store)

	if _, err := store.connection.Exec(
		`DELETE FROM sessions WHERE id = ?`, string(session.ID)); err != nil {
		t.Fatalf("删除会话失败: %v", err)
	}

	var remaining int
	if err := store.connection.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE session_id = ?`, string(session.ID)).Scan(&remaining); err != nil {
		t.Fatalf("统计消息失败: %v", err)
	}
	if remaining != 0 {
		t.Errorf("删除会话后还剩 %d 条消息", remaining)
	}
}

// 同一个会话里 seq 不能重复，否则历史顺序就没有意义了。
func TestMessagePrimaryKeyPreventsDuplicateSeq(t *testing.T) {
	store, _ := newTestStore(t)
	session := sampleSession(t, store)

	_, err := store.connection.Exec(
		`INSERT INTO messages (session_id, seq, role, content) VALUES (?, ?, ?, ?)`,
		string(session.ID), 0, "user", "抢占已有的位置")
	if err == nil {
		t.Fatal("主键没有拦住重复的 seq")
	}
}

// WAL 与外键是连接级设置，必须确认它们真的生效了。
func TestConnectionPragmasAreInEffect(t *testing.T) {
	store, _ := newTestStore(t)

	var journalMode string
	if err := store.connection.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("读取 journal_mode 失败: %v", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Errorf("journal_mode = %q; want wal", journalMode)
	}

	var foreignKeys int
	if err := store.connection.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("读取 foreign_keys 失败: %v", err)
	}
	if foreignKeys != 1 {
		t.Errorf("foreign_keys = %d; want 1", foreignKeys)
	}
}

// 会话 ID 是外部输入，加载前必须先过格式校验，不能直接拿去查库。
func TestLoadValidatesSessionIDBeforeQuerying(t *testing.T) {
	store, _ := newTestStore(t)

	if _, err := store.Load(domain.SessionID("../../etc/passwd")); err == nil {
		t.Fatal("非法的会话 ID 被接受了")
	}
}

// 数据库里有命令输出和文件内容，权限必须限制到本用户。
// SQLite 按 umask 建文件（通常 0644），所以这一条是靠我们自己收紧的。
func TestDatabaseFileIsOnlyReadableByTheOwner(t *testing.T) {
	store, dataDirectory := newTestStore(t)
	sampleSession(t, store)

	for _, name := range []string{databaseFileName, databaseFileName + "-wal"} {
		path := filepath.Join(dataDirectory, name)
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue // WAL 在检查点之后可能不存在
		}
		if err != nil {
			t.Fatalf("Stat %s 失败: %v", name, err)
		}
		if permissions := info.Mode().Perm(); permissions != databasePerm {
			t.Errorf("%s 权限 = %o; want %o", name, permissions, databasePerm)
		}
	}

	info, err := os.Stat(filepath.Join(dataDirectory, lockDirName))
	if err != nil {
		t.Fatalf("Stat 锁目录失败: %v", err)
	}
	if permissions := info.Mode().Perm(); permissions != dataDirPerm {
		t.Errorf("锁目录权限 = %o; want %o", permissions, dataDirPerm)
	}
}

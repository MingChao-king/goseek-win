package store

import (
	"database/sql"
	"fmt"
)

// migrations 按顺序保存每一版的建表语句，下标加一就是它对应的 user_version。
//
// 用 SQLite 自带的 user_version 而不是自建一张版本表：它是数据库头部的一个整数，
// 读写都不需要先有表，因此空库和已有库走的是同一条路径。
//
// 追加新版本只能往末尾加，永远不改已经发布过的那几条——用户库里的数据是按旧语句
// 建起来的，改动历史迁移不会让它们跟着变。
var migrations = []string{
	// v1：会话与消息。
	//
	// 表用 STRICT：SQLite 默认允许往任何列写任何类型，一个写错类型的值会一直潜伏到
	// 读出来才出问题。STRICT 让它在写入时就失败。
	//
	// role 用 CHECK 约束把取值锁死在三个角色上。这与读取时仍然解析角色不冲突：
	// 约束防止本程序写进垃圾，解析防止相信别人（比如手工 sqlite3）写进来的垃圾。
	`
CREATE TABLE sessions (
    id                   TEXT PRIMARY KEY,
    workspace            TEXT NOT NULL,
    pending_tool_call_id TEXT NOT NULL DEFAULT '',
    created_at           TEXT NOT NULL,
    updated_at           TEXT NOT NULL
) STRICT;

CREATE TABLE messages (
    session_id   TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    seq          INTEGER NOT NULL,
    role         TEXT    NOT NULL CHECK (role IN ('user', 'assistant', 'tool')),
    content      TEXT    NOT NULL,
    tool_calls   TEXT    NOT NULL DEFAULT '',
    tool_call_id TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (session_id, seq)
) STRICT;

-- 会话列表按最后活动倒序，这个索引让排序不必扫全表。
CREATE INDEX idx_sessions_updated_at ON sessions (updated_at DESC);

-- 列表的标题取每个会话的首条 user 消息。局部索引只覆盖 user 行，
-- 让这个查询不用翻过大量 assistant 和 tool 消息。
CREATE INDEX idx_messages_first_user ON messages (session_id, seq) WHERE role = 'user';
`,

	// v2：轮次归属与运行事件。
	//
	// turn_id 加在消息上而不是单独建一张 turns 表：轮次此刻只需要"把一组消息和
	// 事件圈出来"这一个能力，而它自身的状态还没有读者。等到会话快照需要"每一轮
	// 是什么状态"时，再决定建表还是从事件推导。
	//
	// last_sequence 让事件序号的分配和消息 seq 一样，在同一个事务里完成：读出
	// 当前值、加一、写回，中途失败整批回滚，因此序号不会跳号也不会重复。
	//
	// events 的主键是 (session_id, sequence)，它同时就是重放要用的索引——
	// "取某会话中序号大于 N 的事件，按序号排序"正好走这个主键，不必另建索引。
	`
ALTER TABLE messages ADD COLUMN turn_id TEXT NOT NULL DEFAULT '';

ALTER TABLE sessions ADD COLUMN last_sequence INTEGER NOT NULL DEFAULT 0;

CREATE TABLE events (
    session_id TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    sequence   INTEGER NOT NULL,
    turn_id    TEXT    NOT NULL,
    type       TEXT    NOT NULL,
    payload    TEXT    NOT NULL,
    at         TEXT    NOT NULL,
    PRIMARY KEY (session_id, sequence)
) STRICT;
`,

	// v3：会话记忆（摘要树）。
	//
	// 摘要节点单独一张表而不是塞进 sessions 的一列 JSON：节点是只增不删的，
	// 每次压缩只新增一两个，用一行一个节点就能增量插入；塞进一列 JSON 则每次
	// 压缩都要重写整棵树，历史越长写得越多——那正是 M2.5 换掉 JSON 文件时想
	// 摆脱的写放大。
	//
	// source_batch_ids 存成 JSON 文本而不是再拆一张关联表：它是一个至多两项的
	// 有序列表，没有任何查询需要按子节点反查父节点。拆表只会多一次 join。
	//
	// active_batch_ids 留在 sessions 行上（也是 JSON 文本）：它是"当前哪些节点
	// 生效"这一个整体状态，每次压缩必然整体改写，没有增量可言。
	`
ALTER TABLE sessions ADD COLUMN raw_compaction_cursor INTEGER NOT NULL DEFAULT 0;

ALTER TABLE sessions ADD COLUMN active_batch_ids TEXT NOT NULL DEFAULT '';

CREATE TABLE memory_batches (
    session_id          TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    id                  TEXT    NOT NULL,
    level               INTEGER NOT NULL,
    content             TEXT    NOT NULL,
    start_message_index INTEGER NOT NULL,
    end_message_index   INTEGER NOT NULL,
    source_batch_ids    TEXT    NOT NULL DEFAULT '',
    created_at          TEXT    NOT NULL,
    PRIMARY KEY (session_id, id)
) STRICT;
`,

	// v4：摘要的人工修订版。
	//
	// 加一列而不是让 content 变成可写的：content 是**模型当初生成了什么**，写入
	// 之后永不修改；edited_content 是**现在拿哪一版进上下文**。不可变的是事实，
	// 可变的是取用（见 domain.MemoryBatch.EditedContent 的说明）。
	//
	// 两份都留着，界面才能让人对照"我改了什么"，出问题时也才查得清是模型总结得
	// 不好还是人改坏了。
	//
	// 默认空串而不是 NULL：空串表示"没改过"，读回来直接就是零值，不需要
	// sql.NullString 那一层包装。
	`
ALTER TABLE memory_batches ADD COLUMN edited_content TEXT NOT NULL DEFAULT '';
`,

	// v5：会话的归档状态与自定义标题。
	//
	// archived_at 用时间戳而不是布尔值：将来要按"归档时间"排序、或者做"最近归档
	// 的"，都不必再迁移一次。空串表示未归档。
	//
	// title 空串表示"沿用派生标题"（首条 user 消息的开头）。**不在创建时把派生
	// 结果写进去**：那样派生规则一改，老会话的标题就和新会话对不上了，而这一列
	// 又看不出哪些是自动填的、哪些是用户改的。
	`
ALTER TABLE sessions ADD COLUMN archived_at TEXT NOT NULL DEFAULT '';

ALTER TABLE sessions ADD COLUMN title TEXT NOT NULL DEFAULT '';
`,

	// v6：用户原话的第二条水位线，以及被并掉的那段原话的整理结果。
	//
	// 摘要里的"期间用户说过"那一栏是**渲染时派生的**，不进 memory_batches——
	// 派生的东西不需要存储（见 domain.ConversationMemory.QuotesCollapsedBefore）。
	// 需要存的只有这两样：并到哪里为止，以及那段话被整理成了什么。
	//
	// 和 raw_compaction_cursor 一样放在 sessions 行上：它们是同一类东西——
	// "当前的记忆状态"，每次压缩整体改写，没有增量可言。
	//
	// 默认 0 / 空串正是绝大多数会话的终生状态：这条降级路径要用户原话本身就撑爆
	// 目标线才会触发，那是数以千计的用户消息。
	`
ALTER TABLE sessions ADD COLUMN quotes_collapsed_before INTEGER NOT NULL DEFAULT 0;

ALTER TABLE sessions ADD COLUMN collapsed_quotes TEXT NOT NULL DEFAULT '';
`,

	// v7：会话级模型选择。
	//
	// 窗口跟着模型走，而压缩历史按当时的窗口生成；模型必须是会话属性，否则改全局
	// 默认模型会静默改变老会话的压缩阈值。空串表示沿用启动时的默认模型，与 title
	// 一样不在创建时写死，保证默认值升级后新老会话仍能区分。
	`
ALTER TABLE sessions ADD COLUMN model TEXT NOT NULL DEFAULT '';
`,

	// v8：消息图片。
	//
	// 图片是附属媒体：它不属于 messages.content（那里是文本协议事实，被摘要、
	// 检索、前端和 token 估算多处消费），也不以 base64 进库（会让库和事件重放
	// 膨胀几个数量级）。独立一张表，消息落库时在同一个事务里绑定。
	//
	// message_seq 用 0 表示"尚未发送"：上传后用户可能反悔删掉，或者一直没发。
	// 发送成功后（消息 append 进历史）由 insertMessages 在同一事务里推进到真实
	// 的消息下标。
	`
CREATE TABLE message_images (
    id          TEXT PRIMARY KEY,
    session_id  TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    message_seq INTEGER NOT NULL DEFAULT 0,
    file_path   TEXT    NOT NULL,
    media_type  TEXT    NOT NULL,
    width       INTEGER NOT NULL DEFAULT 0,
    height      INTEGER NOT NULL DEFAULT 0,
    byte_size   INTEGER NOT NULL,
    created_at  TEXT    NOT NULL
) STRICT;

CREATE INDEX idx_message_images_session ON message_images (session_id, message_seq);
`,
}

// migrate 把数据库升级到本程序支持的最新版本。
//
// 比本程序更新的库要明确拒绝，而不是当作当前版本继续用：那份库里可能有本程序不认识
// 的表和列，继续写下去会留下一份两边都读不好的数据。
func migrate(database *sql.DB) error {
	var version int
	if err := database.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("读取数据库版本失败: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("数据库版本是 %d，本程序只支持到 %d，请升级 GoSeek",
			version, len(migrations))
	}

	for next := version; next < len(migrations); next++ {
		if err := applyMigration(database, next); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration 在一个事务里执行第 index 版迁移并推进 user_version。
//
// user_version 必须和建表语句在同一个事务里提交，否则中途失败会留下一个
// “表建了一半、版本号还是旧的”的库，下次启动重跑迁移就会撞上已存在的表。
//
// PRAGMA user_version 不接受参数占位符，只能拼进语句；这里拼的是循环下标，
// 不是外部输入。
func applyMigration(database *sql.DB, index int) error {
	transaction, err := database.Begin()
	if err != nil {
		return fmt.Errorf("开启迁移事务失败: %w", err)
	}
	defer transaction.Rollback()

	if _, err := transaction.Exec(migrations[index]); err != nil {
		return fmt.Errorf("执行第 %d 版迁移失败: %w", index+1, err)
	}
	if _, err := transaction.Exec(fmt.Sprintf("PRAGMA user_version = %d", index+1)); err != nil {
		return fmt.Errorf("写入数据库版本失败: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("提交第 %d 版迁移失败: %w", index+1, err)
	}
	return nil
}

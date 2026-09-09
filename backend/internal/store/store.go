// Package store 把会话持久化到一个 SQLite 数据库，并保证同一会话同时只被一个进程持有。
//
// 一个会话是 sessions 表里的一行加上 messages 表里的若干行。追加一条消息是一次
// INSERT，而不是重写整个会话；会话列表是一条查询，而不是读遍所有文件。事务保证
// “消息 + 会话状态”要么一起生效要么都不生效，取代了此前的原子文件替换。
//
// 数据库形态与领域类型分开定义：领域类型会随阶段推进改名和拆分，而表结构一旦有了
// 用户数据就必须靠迁移演进，绑在一起意味着一次重构让已有数据读不出来。
//
// 会话锁仍然是文件锁，不放进数据库。SQLite 的锁保护的是文件，不是“这个会话正被谁
// 用”；而用表里的一行加 pid 来表示持有者，进程被 kill -9 之后会留下陈旧记录，又得
// 回去做存活探测——文件锁由内核在 fd 关闭时释放，正好避开这一摊。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 注册纯 Go 的 sqlite 驱动，无需 cgo

	"goseek/internal/domain"
)

// ErrSessionNotFound 表示数据库里没有这个会话。
//
// 它是哨兵错误，让 HTTP 层能用 errors.Is 判断并返回 404，而不是把一句
// "没有找到会话 ses_xxx" 当成 500。
var ErrSessionNotFound = errors.New("会话不存在")

const (
	// driverName 是 modernc.org/sqlite 注册的驱动名。
	driverName = "sqlite"
	// databaseFileName 是数据库文件名。
	databaseFileName = "goseek.db"
	// lockDirName 存放每个会话的锁文件。
	lockDirName = "locks"

	// dataDirPerm 与 databasePerm 限制数据目录和数据库文件：会话里有命令输出和
	// 文件内容。SQLite 自己按 umask 建文件，通常是 0644，因此必须显式收紧。
	//三位数字 7 0 0 分别对应 属主 / 同组用户 / 其他所有人，每位对应 读4 + 写2 + 执行1 之和。
	dataDirPerm  = 0o700
	databasePerm = 0o600

	// titleRunes 是会话列表里标题的最大长度。
	titleRunes = 48

	// timeLayout 是时间戳的存储格式。
	//
	// 存 UTC 的 RFC3339：用 sqlite3 打开时人能直接读，而且固定偏移量下字典序正好
	// 等于时间序，ORDER BY updated_at 不需要额外转换。
	timeLayout = time.RFC3339Nano
)

// Store 读写一个数据目录下的全部会话。
//
// 一个 Store 同一时间只打开一个会话：一个终端进程就在一个会话里工作。
// 需要换会话时先 Close。
type Store struct {
	database   string
	lockDir    string
	connection *sql.DB
	// now 提供保存时间，测试用固定时钟替换。
	now func() time.Time

	// held 是当前打开会话的文件锁；没有打开会话时为 nil。
	held *lock
	// openID 是当前打开的会话。
	openID domain.SessionID
	// persisted 是该会话已经写进数据库的消息条数。
	//
	// 因为历史是 append-only 的，这个计数就足以算出每次保存要插入哪几条，
	// 不需要和数据库比对差异。领域不变量在这里直接兑现成了写入量。
	persisted int
	// lastSequence 是该会话已经分配出去的最大事件序号。
	//
	// 与 persisted 同理：会话被独占持有，因此进程内的这个值就是权威的，每次分配
	// 不必回数据库查一次 MAX。它只在事务提交成功之后才前进。
	lastSequence int64
	// persistedBatches 是该会话已经写进数据库的摘要节点数。
	//
	// 与 persisted 同理：节点只增不删，因此这个计数就足以算出每次要插入哪几个。
	persistedBatches int
}

// New 打开数据目录下的数据库，必要时建库并升级到最新版本。
func New(dataDirectory string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dataDirectory, lockDirName), dataDirPerm); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}

	path := filepath.Join(dataDirectory, databaseFileName)
	connection, err := sql.Open(driverName, path)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	// PRAGMA 是**每个连接**的设置，而 database/sql 是一个连接池：在池上执行一次
	// PRAGMA 只作用于当时拿到的那条连接，后续查询可能落到没设置过的连接上。把池
	// 限制成一条连接，既让下面的 PRAGMA 确定生效，也符合这里的实际用法——一个
	// 终端进程串行地读写一个会话，不存在并发查询。
	connection.SetMaxOpenConns(1)

	if err := configure(connection); err != nil {
		connection.Close()
		return nil, err
	}
	// configure 里的 PRAGMA 已经促使 SQLite 真正建出文件，此时才能改权限。
	// 趁写入任何数据之前改：WAL 和 shm 文件由 SQLite 按主库文件的权限创建，
	// 先收紧主库，它们才不会带着 0644 出生。
	//数据库文件权限设置为 600
	if err := os.Chmod(path, databasePerm); err != nil {
		connection.Close()
		return nil, fmt.Errorf("设置数据库文件权限失败: %w", err)
	}
	if err := migrate(connection); err != nil {
		connection.Close()
		return nil, err
	}

	return &Store{
		database:   path,
		lockDir:    filepath.Join(dataDirectory, lockDirName),
		connection: connection,
		now:        time.Now,
	}, nil
}

// configure 设置连接级别的 PRAGMA。
// 每个连接的配置
func configure(connection *sql.DB) error {
	pragmas := []string{
		// WAL 让读写不互相阻塞，并且崩溃后由日志恢复，不依赖我们自己做原子替换。
		"PRAGMA journal_mode = WAL",
		// FULL 表示每次提交都 fsync。进程被杀本来 NORMAL 就够，但断电时 NORMAL
		// 可能丢掉最后一批提交；本包的存在意义就是崩溃之后还能恢复，所以取严的。
		"PRAGMA synchronous = FULL",
		// SQLite 默认不启用外键，messages 上的 ON DELETE CASCADE 需要它。
		"PRAGMA foreign_keys = ON",
		// 万一有另一个进程正在写，等一会儿而不是立刻报 database is locked。
		"PRAGMA busy_timeout = 5000",
	}
	for _, pragma := range pragmas {
		if _, err := connection.Exec(pragma); err != nil {
			return fmt.Errorf("设置 %q 失败: %w", pragma, err)
		}
	}
	return nil
}

// Summary 是会话列表需要的信息，不含消息正文。
type Summary struct {
	// ID 是会话的应用 ID。
	ID domain.SessionID
	// Title 是首条用户消息的开头，用来在列表里辨认会话。
	//
	// 取真实的第一句话而不是让模型生成摘要：那要多一次调用和费用，
	// 而且摘要是模型自述，第一句话是事实。
	Title string
	// MessageCount 是历史中的消息条数。
	MessageCount int
	// UpdatedAt 是最后一次保存时间。
	UpdatedAt time.Time
	// Archived 表示这个会话被归档了：列表默认不显示它。
	//
	// 归档不动任何数据，只影响"要不要出现在眼前"。它和删除是两件事：归档解决
	// "列表太长"，删除解决"这个会话不该存在"。
	Archived bool
	// ArchivedAt 是归档时间，未归档时为零值。
	ArchivedAt time.Time
	// CustomTitle 表示 Title 是用户自己起的，而不是从首条消息派生的。
	//
	// 界面不需要区分显示，但排查"为什么这个会话叫这个名字"时有用。
	CustomTitle bool
	// Workspace 是该会话执行命令的目录。
	Workspace string
	// Model 是该会话使用的模型；空串表示沿用启动时的默认模型。
	Model string
}

// Create 新建一个会话，取得它的锁并写入数据库。
//
// 先落库再返回：这样"会话已经存在"在用户看到提示符之前就是事实。
func (store *Store) Create(workspace, model string) (*domain.Session, error) {
	if store.held != nil {
		return nil, errors.New("当前 Store 已经打开了一个会话，请先 Close")
	}

	id, err := domain.NewSessionID()
	if err != nil {
		return nil, err
	}
	held, err := acquireLock(store.lockPath(id))
	if err != nil {
		return nil, err
	}

	session := domain.NewSession(id, workspace)
	session.Model = model
	session.CreatedAt = store.now()
	session.UpdatedAt = session.CreatedAt

	_, err = store.connection.Exec(
		`INSERT INTO sessions (id, workspace, pending_tool_call_id, created_at, updated_at, model)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		string(id), workspace, "",
		formatTime(session.CreatedAt), formatTime(session.UpdatedAt), model)
	if err != nil {
		_ = held.release()
		return nil, fmt.Errorf("创建会话失败: %w", err)
	}

	store.held = held
	store.openID = id
	store.persisted = 0
	store.lastSequence = 0
	store.persistedBatches = 0
	return session, nil
}

// Load 读入指定会话并取得它的锁。
//
// 会话正被另一个进程持有时返回 ErrSessionBusy：两个进程同时往同一个会话追加消息，
// 会得到一段谁也说不清顺序的历史。
func (store *Store) Load(id domain.SessionID) (*domain.Session, error) {
	if store.held != nil {
		return nil, errors.New("当前 Store 已经打开了一个会话，请先 Close")
	}
	if err := id.Validate(); err != nil {
		return nil, err
	}
	//取锁，至此会话被该窗口锁住
	held, err := acquireLock(store.lockPath(id))
	if err != nil {
		return nil, err
	}
	//根据id取得对话
	session, lastSequence, err := store.read(id)
	if err != nil {
		_ = held.release()
		return nil, err
	}

	store.held = held
	store.openID = id
	//会话消息数
	store.persisted = len(session.Messages())
	store.lastSequence = lastSequence
	store.persistedBatches = len(session.Memory.Batches)
	return session, nil
}

// Save 把会话自上次保存以来的变化，连同本批 durable 事件，一次性写入数据库。
//
// 返回已经分配好 sequence 的 durable 事件，供调用方在提交成功之后推送出去。
// 顺序是有讲究的：**先落库、再推送**。sequence 只有在事务提交时才确定，而消费端
// （M3.2 的 SSE 断线重连）要靠它续传；提前推一个序号为 0 的事件，消费端就无从
// 判断自己看到哪儿了。传入的切片不会被修改。
//
// 消息和事件必须在同一个事务里：分两次提交的话，进程可以死在中间，留下"消息在
// 而事件缺"（重放少一段）或者反过来（重放多出不存在的事实）。两种都会让消费端
// 看到与数据库不一致的历史。
//
// Agent 在每一次外部调用之前都会调用它。返回错误意味着这次变更没有落库，
// 调用方必须停下——继续请求模型或执行命令会产生数据库里没有记录的事实。
func (store *Store) Save(session *domain.Session, events []domain.RunEvent) ([]domain.RunEvent, error) {
	if store.held == nil {
		return nil, errors.New("没有打开的会话")
	}
	if session.ID != store.openID {
		return nil, fmt.Errorf("会话 %q 不是当前打开的 %q", session.ID, store.openID)
	}

	history := session.Messages()
	if len(history) < store.persisted {
		return nil, fmt.Errorf("历史从 %d 条变成了 %d 条，会话历史只能追加",
			store.persisted, len(history))
	}
	session.UpdatedAt = store.now()

	// 一次保存要动四张地方：messages、memory_batches、events、sessions。
	// 它们必须一起成功或一起失败，所以包在一个事务里。
	transaction, err := store.connection.Begin()
	if err != nil {
		return nil, fmt.Errorf("开启保存事务失败: %w", err)
	}
	// 提交之后再 Rollback 是空操作（database/sql 会返回 ErrTxDone，这里忽略），
	// 所以可以无条件 defer——这样下面每一条 return 错误的路径都不必自己回滚，
	// 也就不会漏掉某一条。
	defer transaction.Rollback()
	//存消息
	if err := store.insertMessages(transaction, session, history); err != nil {
		return nil, err
	}
	// 摘要节点和消息一样是只增不删的，同样只插入新增的那几个。
	if err := store.insertMemoryBatches(transaction, session.ID, session.Memory.Batches); err != nil {
		return nil, err
	}
	// 在事务内的一个局部变量上推进序号；只有提交成功之后才写回 store，
	// 否则一次失败的保存会让后续事件从一个已经被回滚掉的号码继续排。
	//
	// 序号由本进程分配而不是用 SQLite 的 AUTOINCREMENT：会话被这个 Store 独占
	// 持有（有锁），所以进程内的计数就是权威；而且用局部变量才能做到"回滚即
	// 复原"——数据库的自增列回滚之后不会把号还回来。
	//lastSequence最初为0
	nextSequence := store.lastSequence
	//存事件
	persistedEvents, err := store.insertEvents(transaction, session.ID, events, &nextSequence)
	if err != nil {
		return nil, err
	}

	// 活跃前沿每次压缩都整体改写，没有增量可言，因此和会话状态一起 UPDATE。
	encodedActive, err := encodeActiveBatchIDs(session.Memory.ActiveBatchIDs)
	if err != nil {
		return nil, err
	}
	_, err = transaction.Exec(
		`UPDATE sessions
		    SET pending_tool_call_id = ?, updated_at = ?, last_sequence = ?,
		        raw_compaction_cursor = ?, active_batch_ids = ?,
		        quotes_collapsed_before = ?, collapsed_quotes = ?
		  WHERE id = ?`,
		session.PendingToolCallID, formatTime(session.UpdatedAt), nextSequence,
		session.Memory.RawCompactionCursor, encodedActive,
		session.Memory.QuotesCollapsedBefore, session.Memory.CollapsedQuotes,
		string(session.ID))
	if err != nil {
		return nil, fmt.Errorf("更新会话状态失败: %w", err)
	}

	if err := transaction.Commit(); err != nil {
		return nil, fmt.Errorf("提交保存事务失败: %w", err)
	}
	// 只有提交成功之后才认这些进度，否则下一次保存会漏掉消息、或者重用序号。
	store.persisted = len(history)
	store.lastSequence = nextSequence
	store.persistedBatches = len(session.Memory.Batches)
	return persistedEvents, nil
}

// insertMessages 写入自上次保存以来新增的消息。
//
// 从 store.persisted 开始而不是每次全量重写：历史是 append-only 的，前面那些
// 一个字都不会变。这个计数是进程内的记账，靠 Load 时的一次统计和每次提交成功后
// 的更新维持——它成立的前提正是"会话被本 Store 独占持有"。
//
// seq 直接用切片下标：它既是排序键，也是读取时校验历史完整性的依据
// （见 queryMessages 里那段连续性检查）。
func (store *Store) insertMessages(transaction *sql.Tx, session *domain.Session, history []domain.Message) error {
	for index := store.persisted; index < len(history); index++ {
		message := history[index]
		encodedCalls, err := encodeToolCalls(message.ToolCalls)
		if err != nil {
			return err
		}
		_, err = transaction.Exec(
			`INSERT INTO messages (session_id, seq, role, content, tool_calls, tool_call_id, turn_id)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			string(session.ID), index, string(message.Role),
			message.Content, encodedCalls, message.ToolCallID, string(message.TurnID))
		if err != nil {
			return fmt.Errorf("写入第 %d 条消息失败: %w", index+1, err)
		}
		// 绑定这条消息附带的图片。图片在上传时已登记（message_seq=0），
		// 消息落库后在同一事务里推进到这条消息的下标。
		for _, image := range message.Images {
			if _, err := transaction.Exec(
				`UPDATE message_images SET message_seq = ? WHERE id = ? AND session_id = ? AND message_seq = 0`,
				index, image.ID, string(session.ID)); err != nil {
				return fmt.Errorf("绑定图片 %s 到第 %d 条消息失败: %w", image.ID, index+1, err)
			}
		}
	}
	return nil
}

// insertEvents 为每个 durable 事件分配序号并写入，返回带序号的副本。
//
// transient 事件（各种 delta）被跳过：它们不落库也不占序号，是行为底线第 11 条
// 要求的——delta 只是打字动画，把它们存下来既会让事件表膨胀几个数量级，也会让
// "重放一遍就能还原发生过什么"这件事失去意义。
func (store *Store) insertEvents(
	transaction *sql.Tx,
	sessionID domain.SessionID,
	events []domain.RunEvent,
	nextSequence *int64,
) ([]domain.RunEvent, error) {
	persisted := make([]domain.RunEvent, 0, len(events))
	for _, event := range events {
		if !event.Durable() {
			continue
		}
		*nextSequence++
		event.Sequence = *nextSequence

		_, err := transaction.Exec(
			`INSERT INTO events (session_id, sequence, turn_id, type, payload, at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			string(sessionID), event.Sequence, string(event.TurnID),
			string(event.Type), string(event.Payload), formatTime(event.At))
		if err != nil {
			return nil, fmt.Errorf("写入第 %d 号事件失败: %w", event.Sequence, err)
		}
		persisted = append(persisted, event)
	}
	return persisted, nil
}

// List 返回全部会话，按最后活动时间倒序。
func (store *Store) List(includeArchived bool) ([]Summary, error) {
	return querySummaries(store.connection, includeArchived)
}

// Close 释放会话锁并关闭数据库连接。多次调用是安全的。
func (store *Store) Close() error {
	var released error
	if store.held != nil {
		released = store.held.release()
		store.held = nil
		store.openID = ""
		store.persisted = 0
	}
	if store.connection != nil {
		if err := store.connection.Close(); err != nil && released == nil {
			released = fmt.Errorf("关闭数据库失败: %w", err)
		}
		store.connection = nil
	}
	return released
}

// read 从数据库读入一个会话，同时返回它已经分配到的最大事件序号。不涉及加锁。
func (store *Store) read(id domain.SessionID) (*domain.Session, int64, error) {
	return querySession(store.connection, id)
}

// lockPath 返回锁文件路径。ID 已经通过格式校验，因此可以安全拼接。
func (store *Store) lockPath(id domain.SessionID) string {
	return filepath.Join(store.lockDir, string(id)+".lock")
}

// titleOf 把首条用户消息折成一行并截断，作为列表标题。
func titleOf(firstUserMessage string) string {
	text := strings.Join(strings.Fields(firstUserMessage), " ")
	if text == "" {
		return "（还没有对话）"
	}
	runes := []rune(text)
	if len(runes) > titleRunes {
		return string(runes[:titleRunes]) + "…"
	}
	return text
}

// formatTime 把时间转换成存储格式。
func formatTime(when time.Time) string {
	return when.UTC().Format(timeLayout)
}

// parseTime 把存储格式还原成时间。
//
// 时间戳是外部输入——数据库文件可以被 sqlite3 直接改——所以解析失败要报错，
// 而不是退回零值让它在会话列表里显示成"未知"。
func parseTime(text string) (time.Time, error) {
	return time.Parse(timeLayout, text)
}

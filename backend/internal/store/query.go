package store

import (
	"database/sql"
	"errors"
	"fmt"

	"goseek/internal/domain"
)

// 本文件放所有只读查询。
//
// 它们被两条路共用：持有会话的 Store（写路径顺带要读），以及不加锁的 Reader
// （HTTP 服务查列表、快照和重放事件）。因此这些函数不挂在任何类型上，只接一个
// queryer——传 *sql.DB 还是 *sql.Tx 由调用方决定。
//
// 为什么读不需要加会话锁：SQLite 的 WAL 允许一个写者和多个读者并存，读到的永远
// 是某个已提交事务的一致快照。而会话锁保护的是"同一会话同时只有一个写者"，
// 与读无关——让读也去抢锁，只会让一轮交互期间的快照请求全部排队。

// queryer 是 *sql.DB 和 *sql.Tx 的公共部分（只读）。
type queryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// execer 是写入方需要的部分。单独一个接口是为了让写入函数无法误用读的能力，
// 也让它们能在事务和裸连接之间通用。
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// querySummaries 返回全部会话摘要，按最后活动倒序。
//
// 标题和消息条数用关联子查询在同一条语句里取出，不必为每个会话再查一次。
// 这就是所谓的 N+1 查询问题：先查出 N 个会话、再为每个会话查一次消息，一共
// N+1 次往返。会话列表是打开界面时的第一屏，这里多一次往返用户就多等一次。
//
// 标题取的是**最早那条 user 消息**（ORDER BY seq LIMIT 1）：一个会话的主题由
// 用户第一句话决定，后面的消息大多是追问和细节。取不到时 COALESCE 成空串，
// 由 titleOf 换成占位文案——列表里出现一行空白比出现 NULL 更糟。
// includeArchived 让调用方选择要不要把已归档的会话也列出来。
func querySummaries(db queryer, includeArchived bool) ([]Summary, error) {
	// 归档过滤放在 SQL 里而不是读出来再筛：绝大多数请求只要未归档的那些，
	// 把归档的也捞出来再扔掉，白读一遍。
	filter := "WHERE s.archived_at = ''"
	if includeArchived {
		filter = ""
	}
	rows, err := db.Query(`
		SELECT s.id,
		       s.workspace,
	       s.updated_at,
	       s.archived_at,
	       s.title,
	       s.model,
	       (SELECT COUNT(*) FROM messages m WHERE m.session_id = s.id),
		       COALESCE((SELECT m.content FROM messages m
		                  WHERE m.session_id = s.id AND m.role = 'user'
		                  ORDER BY m.seq LIMIT 1), '')
		  FROM sessions s ` + filter + `
		 ORDER BY s.updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("查询会话列表失败: %w", err)
	}
	defer rows.Close()

	var summaries []Summary
	for rows.Next() {
		var id, workspace, updatedAt, archivedAt, title, model, firstUserMessage string
		var messageCount int
		if err := rows.Scan(&id, &workspace, &updatedAt, &archivedAt, &title, &model,
			&messageCount, &firstUserMessage); err != nil {
			return nil, fmt.Errorf("读取会话列表失败: %w", err)
		}
		when, err := parseTime(updatedAt)
		if err != nil {
			return nil, fmt.Errorf("会话 %s 的时间戳无法解析: %w", id, err)
		}

		summary := Summary{
			ID:           domain.SessionID(id),
			Title:        titleOf(firstUserMessage),
			MessageCount: messageCount,
			UpdatedAt:    when,
			Workspace:    workspace,
			Model:        model,
		}
		// 用户起过名字就用它，否则沿用从首条消息派生的那个。
		if title != "" {
			summary.Title = title
			summary.CustomTitle = true
		}
		if archivedAt != "" {
			summary.Archived = true
			// 归档时间解析不了不算致命：那只影响排序和显示，会话本身还在。
			// 这里和 updated_at 不同——后者解析失败说明这一行确实坏了。
			if archived, parseErr := parseTime(archivedAt); parseErr == nil {
				summary.ArchivedAt = archived
			}
		}
		summaries = append(summaries, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历会话列表失败: %w", err)
	}
	return summaries, nil
}

// querySession 读出一个会话，同时返回它已经分配到的最大事件序号。
//
// 一个会话的状态散在三处，这里把它们拼回一个完整的 domain.Session：
//
//	sessions      一行：身份、工作目录、检查点、时间戳、事件序号、记忆游标与前沿
//	messages      N 行：完整历史，按 seq 排序
//	memory_batches N 行：摘要树
//
// 序号单独返回而不是塞进 Session：它是**存储层的记账**（下一个事件该编几号），
// 不是会话本身的属性。放进领域类型，领域就得关心一件与它无关的事。
//
// 先校验 ID 再查询。它是外部输入（HTTP 路径参数、命令行 --resume），而且会被
// 当成锁文件名使用——校验放在最外层，格式不对的输入根本走不到数据库和文件系统。
func querySession(db queryer, id domain.SessionID) (*domain.Session, int64, error) {
	if err := id.Validate(); err != nil {
		return nil, 0, err
	}

	var workspace, pending, createdAt, updatedAt, activeBatchIDs, collapsedQuotes, model string
	var lastSequence int64
	var rawCursor, quotesCollapsedBefore int
	err := db.QueryRow(
		`SELECT workspace, pending_tool_call_id, created_at, updated_at, last_sequence,
		        raw_compaction_cursor, active_batch_ids,
		        quotes_collapsed_before, collapsed_quotes, model
		   FROM sessions WHERE id = ?`, string(id)).
		Scan(&workspace, &pending, &createdAt, &updatedAt, &lastSequence,
			&rawCursor, &activeBatchIDs, &quotesCollapsedBefore, &collapsedQuotes, &model)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("读取会话失败: %w", err)
	}

	messages, err := queryMessages(db, id)
	if err != nil {
		return nil, 0, err
	}

	// 摘要树和活跃前沿一起读出来。会话没压缩过时它们都是空的，
	// 对应"活跃前沿覆盖 [0,0)、游标为 0"，那正好满足记忆的不变量。
	//
	// 两者分开存是因为生命周期不同：节点不可变、只增不删，所以单独一张表；
	// 前沿每次压缩都会换一批，所以是 sessions 表上的一列，覆盖写。
	batches, err := queryMemoryBatches(db, id)
	if err != nil {
		return nil, 0, err
	}
	active, err := decodeActiveBatchIDs(activeBatchIDs)
	if err != nil {
		return nil, 0, fmt.Errorf("会话 %s: %w", id, err)
	}

	session := domain.LoadSession(id, workspace, messages)
	session.Model = model
	session.PendingToolCallID = pending
	session.Memory = domain.ConversationMemory{
		RawCompactionCursor:   rawCursor,
		QuotesCollapsedBefore: quotesCollapsedBefore,
		CollapsedQuotes:       collapsedQuotes,
		Batches:               batches,
		ActiveBatchIDs:        active,
	}
	if session.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, 0, fmt.Errorf("会话 %s 的创建时间无法解析: %w", id, err)
	}
	if session.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, 0, fmt.Errorf("会话 %s 的更新时间无法解析: %w", id, err)
	}
	return session, lastSequence, nil
}

// queryMemory 只读出一个会话的记忆，不读消息。
//
// 单独一条查询而不是复用 querySession：面板展开摘要树时要的只是这些节点，
// 而 querySession 会把整段历史一起捞出来——一个长会话几百条消息，为了看一棵树
// 全读一遍是白费。
func queryMemory(db queryer, id domain.SessionID) (domain.ConversationMemory, error) {
	if err := id.Validate(); err != nil {
		return domain.ConversationMemory{}, err
	}

	var rawCursor, quotesCollapsedBefore int
	var activeBatchIDs, collapsedQuotes string
	err := db.QueryRow(
		`SELECT raw_compaction_cursor, active_batch_ids,
		        quotes_collapsed_before, collapsed_quotes
		   FROM sessions WHERE id = ?`,
		string(id)).Scan(&rawCursor, &activeBatchIDs, &quotesCollapsedBefore, &collapsedQuotes)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ConversationMemory{}, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	if err != nil {
		return domain.ConversationMemory{}, fmt.Errorf("读取会话记忆失败: %w", err)
	}

	batches, err := queryMemoryBatches(db, id)
	if err != nil {
		return domain.ConversationMemory{}, err
	}
	active, err := decodeActiveBatchIDs(activeBatchIDs)
	if err != nil {
		return domain.ConversationMemory{}, fmt.Errorf("会话 %s: %w", id, err)
	}
	return domain.ConversationMemory{
		RawCompactionCursor:   rawCursor,
		QuotesCollapsedBefore: quotesCollapsedBefore,
		CollapsedQuotes:       collapsedQuotes,
		Batches:               batches,
		ActiveBatchIDs:        active,
	}, nil
}

// queryMessages 按 seq 顺序读出一个会话的全部消息。
//
// seq 既是排序键也是完整性校验的依据：它是消息在历史里的下标，必须从 0 开始
// 连续。历史是 append-only 的事实序列，中间缺一条不是"少看到一句话"，而是
// 工具调用与观察的配对可能断掉——下一次请求就会因为悬空调用被供应商拒。
func queryMessages(db queryer, id domain.SessionID) ([]domain.Message, error) {
	rows, err := db.Query(
		`SELECT seq, role, content, tool_calls, tool_call_id, turn_id
		   FROM messages WHERE session_id = ? ORDER BY seq`, string(id))
	if err != nil {
		return nil, fmt.Errorf("查询消息失败: %w", err)
	}
	defer rows.Close()

	var messages []domain.Message
	for rows.Next() {
		var seq int
		var role, content, encodedCalls, toolCallID, turnID string
		if err := rows.Scan(&seq, &role, &content, &encodedCalls, &toolCallID, &turnID); err != nil {
			return nil, fmt.Errorf("读取消息失败: %w", err)
		}
		// len(messages) 就是"这一条应该是第几条"：已经读进来 k 条时，
		// 下一条的 seq 必须正好是 k。缺口意味着数据被外部改过（有人手工删了
		// 一行），此时报错而不是接受一段有洞的历史。
		if seq != len(messages) {
			return nil, fmt.Errorf("会话 %s 的消息序号不连续：第 %d 条的 seq 是 %d",
				id, len(messages)+1, seq)
		}
		message, err := toMessage(role, content, encodedCalls, toolCallID, turnID)
		if err != nil {
			return nil, fmt.Errorf("会话 %s 第 %d 条消息: %w", id, seq+1, err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历消息失败: %w", err)
	}
	// 为每条 user / tool 消息加载附带的图片。
	//
	// 走单独一轮查询而不是 LEFT JOIN：图片数远小于消息数，N+1 的实际开销可以
	// 忽略，换来的是主查询不用为"有没有图"改变形状。
	for index := range messages {
		if messages[index].Role != domain.RoleUser && messages[index].Role != domain.RoleTool {
			continue
		}
		images, err := queryMessageImages(db, domain.SessionID(id), index)
		if err != nil {
			return nil, err
		}
		messages[index].Images = images
	}
	return messages, nil
}

// queryMessageImages 按 ID 读出绑定到某条消息的图片列表，按创建时间排序。
func queryMessageImages(db queryer, sessionID domain.SessionID, messageSeq int) ([]domain.MessageImage, error) {
	rows, err := db.Query(
		`SELECT id, file_path, media_type, width, height
		   FROM message_images WHERE session_id = ? AND message_seq = ? ORDER BY created_at`,
		string(sessionID), messageSeq)
	if err != nil {
		return nil, fmt.Errorf("查询消息图片失败: %w", err)
	}
	defer rows.Close()

	var images []domain.MessageImage
	for rows.Next() {
		var image domain.MessageImage
		if err := rows.Scan(&image.ID, &image.FilePath, &image.MediaType, &image.Width, &image.Height); err != nil {
			return nil, fmt.Errorf("读取消息图片失败: %w", err)
		}
		images = append(images, image)
	}
	return images, rows.Err()
}

// queryImage 按 ID 读出一张图片的完整引用和归属信息。
func queryImage(db queryer, id string) (domain.MessageImage, int64, domain.SessionID, int, error) {
	var image domain.MessageImage
	var byteSize int64
	var sessionID string
	var messageSeq int
	err := db.QueryRow(
		`SELECT file_path, media_type, width, height, byte_size, session_id, message_seq
		   FROM message_images WHERE id = ?`, id).
		Scan(&image.FilePath, &image.MediaType, &image.Width, &image.Height,
			&byteSize, &sessionID, &messageSeq)
	if err == sql.ErrNoRows {
		return domain.MessageImage{}, 0, "", 0, ErrImageNotFound
	}
	if err != nil {
		return domain.MessageImage{}, 0, "", 0, fmt.Errorf("查询图片失败: %w", err)
	}
	image.ID = id
	return image, byteSize, domain.SessionID(sessionID), messageSeq, nil
}

// queryEventsAfter 按序号顺序读出某个会话中序号大于 after 的 durable 事件。
//
// 这就是 SSE 断线重连要用的重放查询。客户端一条都没看过时传 0——序号从 1 开始，
// 因此 `> 0` 正好返回全部。
//
// 用**严格大于**而不是大于等于：after 是"我已经看过的最后一条"，把它再发一遍
// 就是重复。前端虽然有按序号去重的兜底，但那道兜底是给"重放窗口与实时窗口重叠"
// 准备的，不该让服务端也来贡献重复。
//
// 只有 durable 事件在表里——各种 delta 是 transient 的，从来不落库。这正是重放
// 语义的定义：重连之后看到的是"发生过什么"的完整记录，而不是逐字重演一遍打字
// 过程。少了逐字动画不影响理解，而落库几万条 delta 会让库迅速膨胀。
//
// 这条查询走 events 的主键 (session_id, sequence)，不需要额外索引。
func queryEventsAfter(db queryer, id domain.SessionID, after int64) ([]domain.RunEvent, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}

	rows, err := db.Query(
		`SELECT sequence, turn_id, type, payload, at
		   FROM events WHERE session_id = ? AND sequence > ? ORDER BY sequence`,
		string(id), after)
	if err != nil {
		return nil, fmt.Errorf("查询事件失败: %w", err)
	}
	defer rows.Close()

	var events []domain.RunEvent
	for rows.Next() {
		var sequence int64
		var turnID, eventType, payload, at string
		if err := rows.Scan(&sequence, &turnID, &eventType, &payload, &at); err != nil {
			return nil, fmt.Errorf("读取事件失败: %w", err)
		}
		when, err := parseTime(at)
		if err != nil {
			return nil, fmt.Errorf("事件 %d 的时间戳无法解析: %w", sequence, err)
		}
		events = append(events, domain.RunEvent{
			Sequence: sequence,
			TurnID:   domain.TurnID(turnID),
			Type:     domain.EventType(eventType),
			Payload:  []byte(payload),
			At:       when,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历事件失败: %w", err)
	}
	return events, nil
}

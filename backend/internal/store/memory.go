package store

import (
	"encoding/json"
	"errors"
	"fmt"

	"goseek/internal/domain"
)

// 会话记忆在数据库里的形态，以及它与领域类型之间的映射。
//
// 与消息一样：摘要节点只增不删，因此保存时只插入新增的那几个，靠进程内的计数
// 判断哪些是新的（见 Store.persistedBatches）。

// insertMemoryBatches 写入自上次保存以来新增的摘要节点。
//
// 节点是不可变的：一旦生成就不再修改，合并只产生新的父节点。因此这里只有 INSERT，
// 没有 UPDATE——如果哪天出现了对已有节点的修改，说明那条不可变性被破坏了。
func (store *Store) insertMemoryBatches(
	transaction execer,
	sessionID domain.SessionID,
	batches []domain.MemoryBatch,
) error {
	for index := store.persistedBatches; index < len(batches); index++ {
		batch := batches[index]
		encodedSources, err := json.Marshal(batch.SourceBatchIDs)
		if err != nil {
			return fmt.Errorf("编码摘要节点 %s 的子节点失败: %w", batch.ID, err)
		}
		_, err = transaction.Exec(
			`INSERT INTO memory_batches
			   (session_id, id, level, content, edited_content,
			    start_message_index, end_message_index, source_batch_ids, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			string(sessionID), string(batch.ID), batch.Level, batch.Content,
			// 新生成的节点不可能带修订，但显式写出来：将来若有"导入历史记忆"
			// 这类路径，漏掉这一列就会把修订悄悄丢掉。
			batch.EditedContent,
			batch.StartMessageIndex, batch.EndMessageIndex,
			string(encodedSources), formatTime(batch.CreatedAt))
		if err != nil {
			return fmt.Errorf("写入摘要节点 %s 失败: %w", batch.ID, err)
		}
	}
	return nil
}

// EditMemoryBatch 把一个摘要节点的人工修订版写进库。
//
// # 这是 memory_batches 上唯一的 UPDATE
//
// 别处只有 INSERT，因为节点不可变。这里之所以可以写，是因为改的**不是** content
// ——那一列仍然永不修改，存的是模型当初生成了什么。这里改的是 edited_content，
// 也就是"现在拿哪一版进上下文"。两件事分开之后，不可变性原样成立。
//
// content 传空串表示撤销修订，回到模型原始的那一版。
//
// 不校验节点是否活跃：那是领域规则，由 domain.ConversationMemory.EditBatch 负责，
// 存储层只管把字节写对。调用方（Runner）先过领域校验再走到这里。
func (store *Store) EditMemoryBatch(
	sessionID domain.SessionID,
	batchID domain.MemoryBatchID,
	content string,
) error {
	if store.held == nil {
		return errors.New("没有打开的会话")
	}
	if sessionID != store.openID {
		return fmt.Errorf("会话 %q 不是当前打开的 %q", sessionID, store.openID)
	}

	result, err := store.connection.Exec(
		`UPDATE memory_batches SET edited_content = ? WHERE session_id = ? AND id = ?`,
		content, string(sessionID), string(batchID))
	if err != nil {
		return fmt.Errorf("保存摘要修订失败: %w", err)
	}
	// 一行都没改到，说明这个节点不在库里。静默成功会让界面显示"已保存"，
	// 而刷新之后修订不见了——那种 bug 最难查。
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认摘要修订是否写入失败: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("摘要节点 %s 不存在", batchID)
	}
	return nil
}

// encodeActiveBatchIDs 把活跃前沿编码成一列 JSON 文本。
//
// 空前沿存成空串而不是 "[]"：一眼能看出"这个会话没压缩过"，用 sqlite3 翻表时省事。
func encodeActiveBatchIDs(ids []domain.MemoryBatchID) (string, error) {
	if len(ids) == 0 {
		return "", nil
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return "", fmt.Errorf("编码活跃摘要前沿失败: %w", err)
	}
	return string(encoded), nil
}

// decodeActiveBatchIDs 还原活跃前沿。
func decodeActiveBatchIDs(encoded string) ([]domain.MemoryBatchID, error) {
	if encoded == "" {
		return nil, nil
	}
	var ids []domain.MemoryBatchID
	if err := json.Unmarshal([]byte(encoded), &ids); err != nil {
		return nil, fmt.Errorf("活跃摘要前沿不是合法 JSON: %w", err)
	}
	return ids, nil
}

// queryMemoryBatches 按创建顺序读出一个会话的全部摘要节点。
//
// 按 created_at 排序而不是插入顺序：父节点必然晚于它的子节点生成，这个顺序让
// 读出来的切片天然满足"引用的节点一定在前面"，便于人工阅读时理解树的形成过程。
func queryMemoryBatches(db queryer, id domain.SessionID) ([]domain.MemoryBatch, error) {
	rows, err := db.Query(
		`SELECT id, level, content, edited_content, start_message_index, end_message_index,
		        source_batch_ids, created_at
		   FROM memory_batches WHERE session_id = ? ORDER BY created_at, id`, string(id))
	if err != nil {
		return nil, fmt.Errorf("查询摘要节点失败: %w", err)
	}
	defer rows.Close()

	var batches []domain.MemoryBatch
	for rows.Next() {
		var batchID, content, editedContent, encodedSources, createdAt string
		var level, start, end int
		if err := rows.Scan(&batchID, &level, &content, &editedContent,
			&start, &end, &encodedSources, &createdAt); err != nil {
			return nil, fmt.Errorf("读取摘要节点失败: %w", err)
		}

		var sources []domain.MemoryBatchID
		if encodedSources != "" && encodedSources != "null" {
			if err := json.Unmarshal([]byte(encodedSources), &sources); err != nil {
				return nil, fmt.Errorf("摘要节点 %s 的子节点不是合法 JSON: %w", batchID, err)
			}
		}
		when, err := parseTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("摘要节点 %s 的时间戳无法解析: %w", batchID, err)
		}

		batches = append(batches, domain.MemoryBatch{
			ID:                domain.MemoryBatchID(batchID),
			Level:             level,
			Content:           content,
			EditedContent:     editedContent,
			StartMessageIndex: start,
			EndMessageIndex:   end,
			SourceBatchIDs:    sources,
			CreatedAt:         when,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历摘要节点失败: %w", err)
	}
	return batches, nil
}

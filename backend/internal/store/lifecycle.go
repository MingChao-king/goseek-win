package store

import (
	"errors"
	"fmt"
	"os"

	"goseek/internal/domain"
)

// 会话的生命周期操作：归档、重命名、删除。
//
// # 为什么归档和删除都要
//
//	归档  解决"列表太长，旧会话碍事"。可逆，一条数据不动
//	删除  解决"这个会话不该存在"（误建、含敏感内容、测试残留）。不可逆
//
// 只做归档不够——测试会造出一堆垃圾会话，那些应该能真删。只做删除也不够——
// 绝大多数旧会话是有价值的，只是不该占着列表。
//
// # 这三个操作都不需要持有会话锁
//
// 归档和重命名只改 sessions 表上的一列，不碰历史；删除则由调用方保证"这个会话
// 此刻没有写者"——HTTP 层在调 Delete 之前会先把 Runner 停掉（见 httpapi）。
// 锁保护的是"同一会话同时只有一个写者"，而这三个操作的正确性来自调用顺序，
// 不是来自锁。

// SetArchived 归档或取消归档一个会话。
//
// 归档时间由 Store 的时钟填写，与 CreatedAt / UpdatedAt 一致——它们记录的都是
// "什么时候发生的存储事实"。
func (store *Store) SetArchived(id domain.SessionID, archived bool) error {
	if err := id.Validate(); err != nil {
		return err
	}

	// 空串表示未归档。用空串而不是 NULL：读回来直接就是零值，
	// 不需要 sql.NullString 那一层包装（同 edited_content）。
	value := ""
	if archived {
		value = formatTime(store.now())
	}
	return store.updateSessionColumn(id, "archived_at", value)
}

// SetTitle 给会话起一个自定义标题。空串表示恢复成从首条消息派生的标题。
func (store *Store) SetTitle(id domain.SessionID, title string) error {
	if err := id.Validate(); err != nil {
		return err
	}
	return store.updateSessionColumn(id, "title", title)
}

// SetModel 切换会话模型。
//
// 调用方必须先确认会话空闲且没有悬空工具调用；这里只负责持久化，不复跑业务校验。
// 这样可以让 HTTP 层在同一个 Runner 命令队列里完成“检查 + 写库 + 重建 Agent”，
// 不需要给 Store 引入对模型目录的依赖。
func (store *Store) SetModel(id domain.SessionID, model string) error {
	if err := id.Validate(); err != nil {
		return err
	}
	return store.updateSessionColumn(id, "model", model)
}

// updateSessionColumn 改 sessions 表上的一列。
//
// 列名由调用方以字面量传入，从不来自外部输入——SQLite 的占位符不能用在列名上，
// 所以这里必须拼字符串，而拼字符串的安全性只能靠"调用方全是内部代码"来保证。
// 这就是为什么它是私有的，而且只有上面两个薄封装。
func (store *Store) updateSessionColumn(id domain.SessionID, column, value string) error {
	result, err := store.connection.Exec(
		fmt.Sprintf(`UPDATE sessions SET %s = ? WHERE id = ?`, column),
		value, string(id))
	if err != nil {
		return fmt.Errorf("更新会话 %s 的 %s 失败: %w", id, column, err)
	}
	// 一行都没改到说明这个会话不在库里。静默成功会让界面显示"已归档"，
	// 而刷新之后什么都没变。
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认更新是否写入失败: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	return nil
}

// Delete 彻底删除一个会话：数据库里的行、它的消息事件摘要，以及锁文件。
//
// # 调用方必须先停掉这个会话的写者
//
// 会话可能正被打开着——有人持有它的文件锁、内存里的 *domain.Session、甚至有一轮
// 正在跑。直接删数据库行会留下一个对着不存在的会话继续写入的 goroutine。
// HTTP 层因此在调这里之前先 runner.Stop()。这个顺序无法在这一层强制，
// 只能写清楚。
//
// # 消息、事件、摘要靠外键级联
//
// 三张表都声明了 ON DELETE CASCADE，所以只删 sessions 这一行就够。这也是当初
// 把外键约束写进 schema 而不是靠代码维护的收益之一。
//
// # 锁文件单独删
//
// 它不在数据库里。删不掉不算致命——那只是一个空文件，下次谁用这个 ID
// （实际上不会有，ID 是随机的）会重新创建。所以这里只记不阻断。
func (store *Store) Delete(id domain.SessionID) error {
	if err := id.Validate(); err != nil {
		return err
	}
	if store.held != nil && store.openID == id {
		return errors.New("这个会话正被当前 Store 持有，删除之前要先 Close")
	}

	result, err := store.connection.Exec(`DELETE FROM sessions WHERE id = ?`, string(id))
	if err != nil {
		return fmt.Errorf("删除会话 %s 失败: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认删除是否生效失败: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}

	// 锁文件删不掉不影响正确性，因此不把它的错误往上抛。
	_ = os.Remove(store.lockPath(id))
	return nil
}

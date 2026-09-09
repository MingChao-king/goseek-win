package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"time"

	"goseek/internal/domain"
)

// Reader 只读地查询会话，不加任何锁。
//
// 它服务的是 HTTP 的读路径：会话列表、会话快照、事件重放。这些请求在一轮交互
// 正在进行时也必须能立刻响应，因此不能排在写路径后面。
//
// 安全性来自 SQLite 的 WAL：一个写者和多个读者可以并存，读到的永远是某个已提交
// 事务的一致快照，不会看到写了一半的状态。会话锁保护的是"同一会话同时只有一个
// 写者"，与读无关。
//
// 与 Store 的区别只有两点：不加锁，不写。查询语句两边共用（见 query.go）。
type Reader struct {
	connection *sql.DB
}

// NewReader 打开数据库的只读句柄。
//
// 数据库必须已经存在并且迁移过——Reader 不建库也不迁移，那是写路径的责任。
// 单独打开一个连接而不是复用 Store 的：Store 把连接池限制成一条连接（为了让
// PRAGMA 确定生效），读写共用那一条就会互相排队，正好是这里要避免的。
func NewReader(dataDirectory string) (*Reader, error) {
	path := filepath.Join(dataDirectory, databaseFileName)
	connection, err := sql.Open(driverName, path)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	if err := configure(connection); err != nil {
		connection.Close()
		return nil, err
	}
	return &Reader{connection: connection}, nil
}

// List 返回会话列表，按最后活动时间倒序。
//
// includeArchived 为 false 时只返回未归档的——那是界面的默认视图。
func (reader *Reader) List(includeArchived bool) ([]Summary, error) {
	return querySummaries(reader.connection, includeArchived)
}

// Session 返回一个会话的当前快照，以及它已经分配到的最大事件序号。
//
// 序号是前端衔接实时事件流的锚点：先用快照渲染出完整历史，再从这个序号往后订阅，
// 中间不重不漏。
func (reader *Reader) Session(id domain.SessionID) (*domain.Session, int64, error) {
	return querySession(reader.connection, id)
}

// Memory 返回一个会话的完整摘要树、活跃前沿和游标。
//
// 它不读消息，因此比 Session 轻得多——面板展开摘要树时用它。原始消息不需要另一个
// 端点：快照里已经有完整历史，节点的 [start, end) 直接就是它在那个数组里的下标区间。
func (reader *Reader) Memory(id domain.SessionID) (domain.ConversationMemory, error) {
	return queryMemory(reader.connection, id)
}

// EventsAfter 返回某个会话中序号大于 after 的 durable 事件，按序号排列。
func (reader *Reader) EventsAfter(id domain.SessionID, after int64) ([]domain.RunEvent, error) {
	return queryEventsAfter(reader.connection, id, after)
}

// Close 关闭只读连接。
func (reader *Reader) Close() error {
	if reader.connection == nil {
		return nil
	}
	connection := reader.connection
	reader.connection = nil
	return connection.Close()
}

// LoadImage 按 ID 读出一张图片的引用。
func (reader *Reader) LoadImage(id string) (domain.MessageImage, int64, domain.SessionID, int, error) {
	return queryImage(reader.connection, id)
}

// SaveImageRecord 把一张已落盘的图片登记进数据库（写路径）。
//
// 它在 Reader 上是因为上传走 HTTP，不需要打开会话的 Store（那会抢锁）。
// 单行 INSERT 用只读连接没有额外风险——SQLite 的 WAL 允许并发写。
func (reader *Reader) SaveImageRecord(image domain.MessageImage, sessionID domain.SessionID, byteSize int64) error {
	_, err := reader.connection.Exec(
		`INSERT INTO message_images (id, session_id, message_seq, file_path, media_type, width, height, byte_size, created_at)
		 VALUES (?, ?, 0, ?, ?, ?, ?, ?, ?)`,
		image.ID, string(sessionID), image.FilePath, image.MediaType,
		image.Width, image.Height, byteSize, formatTime(time.Now()))
	return err
}

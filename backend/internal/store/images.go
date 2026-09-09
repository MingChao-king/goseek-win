package store

import (
	"database/sql"
	"errors"
	"fmt"

	"goseek/internal/domain"
)

// SaveImage 把一张已写入磁盘的图片登记进数据库。
//
// 上传时文件先落盘（httpapi 负责），然后在这里登记。此时 message_seq 为 0，
// 表示还没有绑定到任何消息；发送成功后由 insertMessages 在同一事务里绑定。
func (store *Store) SaveImage(image domain.MessageImage, sessionID domain.SessionID, byteSize int64) error {
	if store.connection == nil {
		return errors.New("数据库未打开")
	}
	_, err := store.connection.Exec(
		`INSERT INTO message_images (id, session_id, message_seq, file_path, media_type, width, height, byte_size, created_at)
		 VALUES (?, ?, 0, ?, ?, ?, ?, ?, ?)`,
		image.ID, string(sessionID), image.FilePath, image.MediaType,
		image.Width, image.Height, byteSize, formatTime(store.now()))
	if err != nil {
		return fmt.Errorf("登记图片失败: %w", err)
	}
	return nil
}

// LoadImage 按 ID 读出一张图片的引用。
func (store *Store) LoadImage(id string) (domain.MessageImage, int64, domain.SessionID, int, error) {
	if store.connection == nil {
		return domain.MessageImage{}, 0, "", 0, errors.New("数据库未打开")
	}
	var image domain.MessageImage
	var byteSize int64
	var sessionID string
	var messageSeq int
	err := store.connection.QueryRow(
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

// ImagesForMessage 读出绑定到某条消息的图片列表，按创建时间排序。
func (store *Store) ImagesForMessage(sessionID domain.SessionID, messageSeq int) ([]domain.MessageImage, error) {
	if store.connection == nil {
		return nil, errors.New("数据库未打开")
	}
	rows, err := store.connection.Query(
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

// UnsentImages 读出某个会话里尚未绑定到消息的图片，按创建时间排序。
func (store *Store) UnsentImages(sessionID domain.SessionID) ([]domain.MessageImage, error) {
	if store.connection == nil {
		return nil, errors.New("数据库未打开")
	}
	rows, err := store.connection.Query(
		`SELECT id, file_path, media_type, width, height
		   FROM message_images WHERE session_id = ? AND message_seq = 0 ORDER BY created_at`,
		string(sessionID))
	if err != nil {
		return nil, fmt.Errorf("查询未发送图片失败: %w", err)
	}
	defer rows.Close()

	var images []domain.MessageImage
	for rows.Next() {
		var image domain.MessageImage
		if err := rows.Scan(&image.ID, &image.FilePath, &image.MediaType, &image.Width, &image.Height); err != nil {
			return nil, fmt.Errorf("读取未发送图片失败: %w", err)
		}
		images = append(images, image)
	}
	return images, rows.Err()
}

// DeleteUnsentImage 删除一张尚未绑定到消息的图片记录。
//
// 文件由调用方（httpapi）负责删除；这里只清数据库行。
func (store *Store) DeleteUnsentImage(id string, sessionID domain.SessionID) error {
	if store.connection == nil {
		return errors.New("数据库未打开")
	}
	result, err := store.connection.Exec(
		`DELETE FROM message_images WHERE id = ? AND session_id = ? AND message_seq = 0`,
		id, string(sessionID))
	if err != nil {
		return fmt.Errorf("删除图片失败: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrImageNotFound
	}
	return nil
}

// ErrImageNotFound 表示数据库里没有这张图片。
var ErrImageNotFound = errors.New("图片不存在")

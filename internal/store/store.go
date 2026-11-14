// Package store 用单文件嵌入式 KV（bbolt）落地数据。
// 单文件意味着备份 = 复制一个文件，迁移 = 拷走 data 目录。
package store

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/asoe/beian2api/internal/model"
	bolt "go.etcd.io/bbolt"
)

var (
	bucketCache   = []byte("cache")   // domain -> Entry
	bucketHistory = []byte("history") // domain\x00ts -> Record
	bucketWatch   = []byte("watch")   // domain -> WatchItem
	bucketMeta    = []byte("meta")    // 计数器等
	bucketKeys    = []byte("apikeys") // key -> KeyMeta
	bucketChanges = []byte("changes") // changeID -> Change
)

// ErrNotFound 表示 key 不存在。
var ErrNotFound = errors.New("not found")

// Entry 是缓存中的一条结果快照。
type Entry struct {
	Domain    string         `json:"domain"`
	Found     bool           `json:"found"`
	Records   []model.Record `json:"records"`
	Source    string         `json:"source"`
	FetchedAt int64          `json:"fetched_at"`
	ExpireAt  int64          `json:"expire_at"`
}

// WatchItem 是被监控的域名。
type WatchItem struct {
	Domain    string `json:"domain"`
	AddedAt   int64  `json:"added_at"`
	CheckedAt int64  `json:"checked_at"`
	LastFP    string `json:"last_fp"`
	LastICP   string `json:"last_icp"`
	Changes   int    `json:"changes"`
	LastError string `json:"last_error,omitempty"`
}

// Store 是对 bbolt 的薄封装，负责序列化与索引。
type Store struct {
	db   *bolt.DB
	path string
	mu   sync.RWMutex
	// 命中计数器，用于统计报表；定期刷盘，避免每次请求都写。
	hits   int64
	misses int64
}

// Open 打开（或创建）数据目录下的数据文件。
func Open(dir string) (*Store, error) {
	return OpenFile(filepath.Join(dir, "beian2api.db"))
}

// OpenFile 打开指定路径的数据文件，目录会按需创建。
func OpenFile(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("创建数据目录 %s: %w", dir, err)
		}
	}
	db, err := bolt.Open(path, 0o640, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		if strings.Contains(err.Error(), "timeout") {
			return nil, fmt.Errorf("数据文件 %s 正被其他进程占用（服务是否在运行？）", path)
		}
		return nil, fmt.Errorf("打开数据文件 %s: %w", path, err)
	}
	s := &Store{db: db, path: path}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketCache, bucketHistory, bucketWatch, bucketMeta, bucketKeys, bucketChanges} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Path 返回数据文件路径。
func (s *Store) Path() string { return s.path }

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// GetCache 读取缓存，不存在返回 ErrNotFound。
func (s *Store) GetCache(domain string) (*Entry, error) {
	var e Entry
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketCache).Get([]byte(domain))
		if v == nil {
			return ErrNotFound
		}
		return json.Unmarshal(v, &e)
	})
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// PutCache 写入缓存。
func (s *Store) PutCache(e *Entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketCache).Put([]byte(e.Domain), b)
	})
}

// DeleteCache 删除某域名的缓存。
func (s *Store) DeleteCache(domain string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketCache).Delete([]byte(domain))
	})
}

// appendHistory 追加一条历史快照（同日同域名覆盖，避免膨胀）。
func (s *Store) AppendHistory(r model.Record) error {
	day := time.Unix(r.FetchedAt, 0).Format("20060102")
	key := []byte(fmt.Sprintf("%s\x00%s", r.Domain, day))
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketHistory).Put(key, b)
	})
}

// History 返回某域名的历史快照，按时间升序。
func (s *Store) History(domain string, limit int) ([]model.Record, error) {
	var out []model.Record
	prefix := []byte(domain + "\x00")
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketHistory).Cursor()
		for k, v := c.Seek(prefix); k != nil && hasPrefix(k, prefix); k, v = c.Next() {
			var r model.Record
			if json.Unmarshal(v, &r) == nil {
				out = append(out, r)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func hasPrefix(b, p []byte) bool {
	if len(b) < len(p) {
		return false
	}
	for i := range p {
		if b[i] != p[i] {
			return false
		}
	}
	return true
}

// PutWatch 新增/更新监控项。
func (s *Store) PutWatch(w *WatchItem) error {
	b, err := json.Marshal(w)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketWatch).Put([]byte(w.Domain), b)
	})
}

// GetWatch 获取监控项。
func (s *Store) GetWatch(domain string) (*WatchItem, error) {
	var w WatchItem
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketWatch).Get([]byte(domain))
		if v == nil {
			return ErrNotFound
		}
		return json.Unmarshal(v, &w)
	})
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// DeleteWatch 删除监控项。
func (s *Store) DeleteWatch(domain string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketWatch).Delete([]byte(domain))
	})
}

// ListWatch 列出全部监控项。
func (s *Store) ListWatch() ([]WatchItem, error) {
	var out []WatchItem
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketWatch).ForEach(func(k, v []byte) error {
			var w WatchItem
			if json.Unmarshal(v, &w) == nil {
				out = append(out, w)
			}
			return nil
		})
	})
	return out, err
}

// Count 统计各 bucket 的条目数。
type Counts struct {
	Cache   int `json:"cache"`
	History int `json:"history"`
	Watch   int `json:"watch"`
	Keys    int `json:"keys"`
	Changes int `json:"changes"`
}

// Count 返回各 bucket 条目数。
func (s *Store) Count() (Counts, error) {
	var c Counts
	err := s.db.View(func(tx *bolt.Tx) error {
		c.Cache = tx.Bucket(bucketCache).Stats().KeyN
		c.History = tx.Bucket(bucketHistory).Stats().KeyN
		c.Watch = tx.Bucket(bucketWatch).Stats().KeyN
		c.Keys = tx.Bucket(bucketKeys).Stats().KeyN
		c.Changes = tx.Bucket(bucketChanges).Stats().KeyN
		return nil
	})
	return c, err
}

// Bulk 在单个事务里写入若干缓存条目，导入用。
func (s *Store) Bulk(entries []*Entry) error {
	if len(entries) == 0 {
		return nil
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketCache)
		for _, e := range entries {
			data, err := json.Marshal(e)
			if err != nil {
				return err
			}
			if err := b.Put([]byte(e.Domain), data); err != nil {
				return err
			}
		}
		return nil
	})
}

// AllCache 遍历全部缓存（备份用）。
func (s *Store) AllCache(fn func(*Entry) error) error {
	return s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketCache).ForEach(func(k, v []byte) error {
			var e Entry
			if json.Unmarshal(v, &e) != nil {
				return nil
			}
			return fn(&e)
		})
	})
}

// Incr 原子自增计数器。
func (s *Store) Incr(name string, delta int64) (int64, error) {
	var n int64
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		cur := b.Get([]byte(name))
		var v uint64
		if len(cur) == 8 {
			v = binary.BigEndian.Uint64(cur)
		}
		v = uint64(int64(v) + delta)
		n = int64(v)
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], v)
		return b.Put([]byte(name), buf[:])
	})
	return n, err
}

// SetStat 直接写入一个整数值（不是自增），用于「上次巡检时间」这类
// 需要跨重启保留、又不需要累加的运行态。失败只影响展示，不算致命。
func (s *Store) SetStat(name string, v int64) error {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(v))
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMeta).Put([]byte(name), buf[:])
	})
}

// Stat 读取计数器。
func (s *Store) Stat(name string) int64 {
	var n int64
	_ = s.db.View(func(tx *bolt.Tx) error {
		cur := tx.Bucket(bucketMeta).Get([]byte(name))
		if len(cur) == 8 {
			n = int64(binary.BigEndian.Uint64(cur))
		}
		return nil
	})
	return n
}

// Snapshot 生成数据文件的副本路径（bbolt 的 Tx.WriteTo 保证一致性）。
func (s *Store) Snapshot(dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	return s.db.View(func(tx *bolt.Tx) error {
		_, err := tx.WriteTo(f)
		return err
	})
}

// AppendChange 记录一条变更事件，key 按时间倒序可遍历。
func (s *Store) AppendChange(id string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketChanges).Put([]byte(id), b)
	})
}

// ListChanges 按时间倒序（新的在前）遍历变更事件，最多保留 limit 条。
//
// fn 返回 keep=false 时该条被丢弃，且不占用 limit 名额。过滤必须发生在
// 截断之前，否则「since 增量拉取」会先被 limit 截断，中间的事件就漏了。
func (s *Store) ListChanges(limit int, fn func(raw []byte) (keep bool, err error)) error {
	if limit <= 0 {
		limit = 50
	}
	return s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketChanges).Cursor()
		n := 0
		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			keep, err := fn(v)
			if err != nil {
				return err
			}
			if !keep {
				continue
			}
			n++
			if n >= limit {
				break
			}
		}
		return nil
	})
}

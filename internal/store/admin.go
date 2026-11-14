package store

import (
	"bytes"
	"encoding/json"
	"os"
	"sort"
	"strings"

	"github.com/asoe/beian2api/internal/model"
	bolt "go.etcd.io/bbolt"
)

// 这个文件是「后台」用的那一层存储能力。
//
// 为什么单独放一处：这里的每个方法都对应一个「看着数据、动手改数据」的
// 管理动作（列缓存、删某域名历史、清变更流水、裁掉过期缓存），
// 它们和查询路径上的 GetCache / AppendHistory 语义不同——
// 前者是给人看的、允许慢；后者在热路径上、必须快。
// 混在一个文件里最容易发生的事，是有人为了「复用一下」把管理用的
// 全表遍历搬进请求路径。分开之后，读代码的人一眼能看出哪边该省。

// CacheRow 是后台缓存列表里的一行。
//
// 不复用 Entry：Entry 里塞着完整的 Records 数组，一页 50 行就是几十条
// 备案记录的 JSON。后台列表只需要「哪个域名、有没有备案、备案号、
// 什么时候抓的、什么时候过期」——把 Record 摊平到这一层，列表页的
// 响应体大概能小一个数量级。
type CacheRow struct {
	Domain    string `json:"domain"`
	Found     bool   `json:"found"`
	ICPCode   string `json:"icp_code"`
	UnitName  string `json:"unit_name"`
	Source    string `json:"source"`
	FetchedAt int64  `json:"fetched_at"`
	ExpireAt  int64  `json:"expire_at"`
}

// ListCachePage 分页列出缓存，keyword 会在域名、备案号、主办单位上做子串匹配。
//
// 为什么要关键词：一个跑了半年的实例有几千个域名，后台翻页找到某个域名
// 需要几十页。而「我记得有个 qq 相关的」是排查时最常见的起点。
func (s *Store) ListCachePage(limit, offset int, keyword string) ([]CacheRow, int, error) {
	kw := strings.ToLower(strings.TrimSpace(keyword))
	out := make([]CacheRow, 0, 32)
	total := 0
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketCache).ForEach(func(k, v []byte) error {
			var e Entry
			if json.Unmarshal(v, &e) != nil {
				return nil
			}
			row := CacheRow{
				Domain: e.Domain, Found: e.Found, Source: e.Source,
				FetchedAt: e.FetchedAt, ExpireAt: e.ExpireAt,
			}
			if r, ok := model.Primary(e.Records); ok {
				row.ICPCode, row.UnitName = r.ICPCode, r.UnitName
			}
			if kw != "" && !matches(kw, row.Domain, row.ICPCode, row.UnitName) {
				return nil
			}
			total++
			// 先数总数再截断：分页控件的「共 N 条」必须按过滤后的集合算，
			// 否则勾了关键词之后页码还按全量显示，翻到后面全是空页。
			if total <= offset {
				return nil
			}
			if len(out) < limit {
				out = append(out, row)
			}
			return nil
		})
	})
	return out, total, err
}

func matches(kw string, fields ...string) bool {
	for _, f := range fields {
		if f != "" && strings.Contains(strings.ToLower(f), kw) {
			return true
		}
	}
	return false
}

// ClearCache 清缓存。onlyExpired 为真时只删已经过期且超出回退窗口的部分。
//
// 「已经过期的缓存为什么还要留着」这个问题在 service 里有答案：
// 上游全挂时会回退到最近一次成功快照，所以过期条目在回退窗口内仍然有用。
// 后台的「清理」因此不能无脑全删——它给的是两个按钮，一个只清真正
// 没用的（超出回退窗口），一个清全部（等于让所有域名下次都要回源）。
func (s *Store) ClearCache(onlyExpired bool, now int64) (int, error) {
	cutoff := now - s.staleMax
	removed := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketCache)
		var doomed [][]byte
		_ = b.ForEach(func(k, v []byte) error {
			if !onlyExpired {
				doomed = append(doomed, append([]byte(nil), k...))
				return nil
			}
			var e Entry
			if json.Unmarshal(v, &e) == nil && e.ExpireAt > 0 && e.ExpireAt < cutoff {
				doomed = append(doomed, append([]byte(nil), k...))
			}
			return nil
		})
		for _, k := range doomed {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		removed = len(doomed)
		return nil
	})
	return removed, err
}

// HistoryRow 是后台「历史快照」页的一行：一个域名一行。
//
// 历史 bucket 的 key 是「域名\x00天\x00时间戳\x00指纹」，按域名分组，
// 一条域名几百条快照是常态。后台要回答的是「谁占得最多、谁的最后一次
// 是什么时候」，所以这里做一次分组聚合，而不是把几万条快照摊到页面上。
type HistoryRow struct {
	Domain    string `json:"domain"`
	Count     int    `json:"count"`
	LastAt    int64  `json:"last_at"`
	LastICP   string `json:"last_icp"`
	LastUnit  string `json:"last_unit"`
	OldestAt  int64  `json:"oldest_at"`
	KeptLimit int    `json:"kept_limit"`
}

// ListHistoryDomains 按域名聚合历史快照，keyword 匹配域名或备案号。
func (s *Store) ListHistoryDomains(limit, offset int, keyword string) ([]HistoryRow, int, error) {
	kw := strings.ToLower(strings.TrimSpace(keyword))
	var rows []HistoryRow
	err := s.db.View(func(tx *bolt.Tx) error {
		// 按域名分组：key 的前缀就是域名，游标天然按域名连续排列，
		// 所以只要记住「上一行的域名」，遇到新域名就收口，不必建 map。
		var cur *HistoryRow
		flush := func() {
			if cur == nil {
				return
			}
			if kw == "" || matches(kw, cur.Domain, cur.LastICP, cur.LastUnit) {
				rows = append(rows, *cur)
			}
			cur = nil
		}
		err := tx.Bucket(bucketHistory).ForEach(func(k, v []byte) error {
			i := bytes.IndexByte(k, 0)
			if i <= 0 {
				return nil
			}
			domain := string(k[:i])
			if cur == nil || cur.Domain != domain {
				flush()
				cur = &HistoryRow{Domain: domain, KeptLimit: s.retainHistory}
			}
			var r model.Record
			if json.Unmarshal(v, &r) != nil {
				return nil
			}
			cur.Count++
			if r.FetchedAt > cur.LastAt {
				cur.LastAt = r.FetchedAt
				cur.LastICP = r.ICPCode
				cur.LastUnit = r.UnitName
			}
			if cur.OldestAt == 0 || r.FetchedAt < cur.OldestAt {
				cur.OldestAt = r.FetchedAt
			}
			return nil
		})
		// 最后一组必须收口。ForEach 只在「域名变了」的时候 flush，
		// 遍历结束时不补一次，桶里最后一个域名就永远不会出现在列表里——
		// 而这恰好是最常见的场景：只有一个域名有历史。
		flush()
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	// 快照多的域名排前面：后台页面上「谁在吃空间」比字母序有用。
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		return rows[i].Domain < rows[j].Domain
	})
	total := len(rows)
	if offset >= total {
		return []HistoryRow{}, total, nil
	}
	rows = rows[offset:]
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, total, nil
}

// DeleteHistoryDomain 删掉某个域名的全部历史快照，返回删除条数。
func (s *Store) DeleteHistoryDomain(domain string) (int, error) {
	prefix := []byte(domain + "\x00")
	removed := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketHistory)
		var doomed [][]byte
		c := b.Cursor()
		for k, _ := c.Seek(prefix); k != nil && hasPrefix(k, prefix); k, _ = c.Next() {
			doomed = append(doomed, append([]byte(nil), k...))
		}
		for _, k := range doomed {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		removed = len(doomed)
		return nil
	})
	if removed > 0 {
		// 绕开了 AppendHistory，增量维护的条数缓存必须作废，
		// 否则全局上限的裁剪节奏会一直按旧值算。
		s.invalidateHistoryCount()
	}
	return removed, err
}

// ClearChanges 清空变更流水，返回删除条数。
func (s *Store) ClearChanges() (int, error) {
	n := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketChanges)
		n = b.Stats().KeyN
		return clearBucket(b)
	})
	return n, err
}

// DeleteChange 按 key 删一条变更流水。
func (s *Store) DeleteChange(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketChanges).Delete([]byte(id))
	})
}

// PurgeAllWatch 清空监控列表，返回删除条数。
func (s *Store) PurgeAllWatch() (int, error) {
	n := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketWatch)
		n = b.Stats().KeyN
		return clearBucket(b)
	})
	return n, err
}

// HistoryCount 返回历史快照总条数（后台概览用）。
func (s *Store) HistoryCount() int {
	n := 0
	_ = s.db.View(func(tx *bolt.Tx) error {
		n = tx.Bucket(bucketHistory).Stats().KeyN
		return nil
	})
	return n
}

// BackupInfo 是 backups 目录里的一个文件。
type BackupInfo struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	CreatedAt int64  `json:"created_at"`
}

// ListBackups 列出 dir 下以 .db 结尾的快照文件，新的在前。
func ListBackups(dir string) ([]BackupInfo, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		// 目录不存在不是错误：还没备份过的实例本来就没有 backups/。
		if os.IsNotExist(err) {
			return []BackupInfo{}, nil
		}
		return nil, err
	}
	out := make([]BackupInfo, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, BackupInfo{Name: info.Name(), Size: info.Size(), CreatedAt: info.ModTime().Unix()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}
